// Package server accepts TCP connections and runs the read-execute-reply
// loop for each client. It knows RESP framing and connection lifecycle, but
// not what any command does: that is delegated to an Executor.
package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"goredis/internal/command"
	"goredis/internal/protocol"
)

// Executor runs one parsed command for a client session and returns its
// reply. The session carries per-connection state such as authentication.
type Executor interface {
	Execute(sess *command.Session, args [][]byte) protocol.Reply
}

// Config controls connection handling.
type Config struct {
	// IdleTimeout closes a connection that sends nothing for this long.
	// Zero disables it.
	IdleTimeout time.Duration
	// WriteTimeout bounds how long one reply flush may block on a slow or
	// stalled client. Zero disables it.
	WriteTimeout time.Duration
	// MaxClients caps concurrent connections. Zero means unlimited.
	MaxClients int
	// Limits bounds request sizes.
	Limits protocol.Limits
	// OnAccept and OnReject, if set, are called for every accepted and
	// every rejected (max clients) connection. Used for metrics.
	OnAccept func()
	OnReject func()
}

// Server is a RESP TCP server.
type Server struct {
	cfg  Config
	exec Executor
	log  *slog.Logger

	mu      sync.Mutex
	ln      net.Listener
	conns   map[net.Conn]struct{}
	closing atomic.Bool
	wg      sync.WaitGroup
}

// New returns a server. Call Serve to start accepting connections.
func New(cfg Config, exec Executor, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{cfg: cfg, exec: exec, log: log, conns: make(map[net.Conn]struct{})}
}

// ErrServerClosed is returned by Serve after Shutdown.
var ErrServerClosed = errors.New("server closed")

// Serve accepts connections on ln until Shutdown is called. It always
// returns a non-nil error; after Shutdown that error is ErrServerClosed.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closing.Load() {
		s.mu.Unlock()
		_ = ln.Close()
		return ErrServerClosed
	}
	s.ln = ln
	s.mu.Unlock()

	var backoff time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if s.closing.Load() {
				return ErrServerClosed
			}
			// Temporary failures such as EMFILE should not kill the
			// server; back off briefly and retry.
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() || isTemporary(err) {
				backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
				s.log.Warn("accept failed, retrying", "err", err, "backoff", backoff)
				time.Sleep(backoff)
				continue
			}
			return err
		}
		backoff = 0
		if !s.track(c) {
			continue
		}
		go s.handle(c)
	}
}

// isTemporary reports whether an accept error is worth retrying.
func isTemporary(err error) bool {
	var t interface{ Temporary() bool }
	return errors.As(err, &t) && t.Temporary()
}

// track registers a new connection, enforcing MaxClients. It reports whether
// the connection should be served.
func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing.Load() {
		_ = c.Close()
		return false
	}
	if s.cfg.MaxClients > 0 && len(s.conns) >= s.cfg.MaxClients {
		_ = c.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = io.WriteString(c, "-ERR max number of clients reached\r\n")
		_ = c.Close()
		s.log.Warn("connection rejected: max clients reached", "remote", c.RemoteAddr().String())
		if s.cfg.OnReject != nil {
			s.cfg.OnReject()
		}
		return false
	}
	if s.cfg.OnAccept != nil {
		s.cfg.OnAccept()
	}
	s.conns[c] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	s.wg.Done()
}

// ActiveConnections returns the number of open client connections.
func (s *Server) ActiveConnections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// handle runs the read-execute-reply loop for one client.
//
// Replies are buffered and flushed only when the reader has no more complete
// input buffered. A client that pipelines 100 commands in one segment gets
// 100 replies in (usually) one write, while an interactive client gets each
// reply immediately.
func (s *Server) handle(c net.Conn) {
	defer s.untrack(c)
	defer c.Close()

	r := protocol.NewReader(c, s.cfg.Limits)
	w := protocol.NewWriter(c)
	sess := command.NewSession()
	for {
		if s.cfg.IdleTimeout > 0 {
			_ = c.SetReadDeadline(time.Now().Add(s.cfg.IdleTimeout))
		}
		// Check for shutdown only after (re)arming the read deadline.
		// Shutdown sets closing before it sets a past deadline, so either
		// we see closing here, or Shutdown's deadline overrides ours and
		// wakes the read below. Checking first would leave a window where
		// our deadline overwrites Shutdown's and we block until idle timeout.
		if s.closing.Load() {
			_ = w.Flush()
			return
		}
		args, err := r.ReadCommand()
		if err != nil {
			s.readFailed(c, w, err)
			return
		}
		if args == nil {
			continue // empty command, e.g. a blank line from telnet
		}

		reply := s.exec.Execute(sess, args)

		if s.cfg.WriteTimeout > 0 {
			_ = c.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
		}
		if err := w.WriteReply(reply); err != nil {
			s.log.Debug("write failed", "remote", c.RemoteAddr().String(), "err", err)
			return
		}
		if sess.Quit {
			_ = w.Flush()
			return
		}
		if r.Buffered() == 0 {
			if err := w.Flush(); err != nil {
				s.log.Debug("flush failed", "remote", c.RemoteAddr().String(), "err", err)
				return
			}
		}
	}
}

// readFailed handles the end of a connection's input. Protocol errors get an
// error reply before the connection closes, because the stream cannot be
// resynchronised. EOF, timeouts and shutdown just flush what is pending.
func (s *Server) readFailed(c net.Conn, w *protocol.Writer, err error) {
	remote := c.RemoteAddr().String()
	if protocol.IsProtocolError(err) {
		s.log.Info("closing connection after protocol error", "remote", remote, "err", err)
		_ = c.SetWriteDeadline(time.Now().Add(time.Second))
		_ = w.WriteReply(protocol.Err("ERR " + err.Error()))
		_ = w.Flush()
		lingerClose(c)
		return
	}
	if s.cfg.WriteTimeout > 0 {
		_ = c.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
	}
	_ = w.Flush()
	var ne net.Error
	switch {
	case errors.Is(err, io.EOF), s.closing.Load():
	case errors.As(err, &ne) && ne.Timeout():
		s.log.Debug("closing idle connection", "remote", remote)
	default:
		s.log.Debug("read failed", "remote", remote, "err", err)
	}
}

// lingerClose half-closes the connection and discards a bounded amount of
// remaining client input before the caller closes it. Closing a TCP socket
// that still has unread input makes the kernel send RST, which can destroy
// the error reply we just sent before the client reads it. The drain is
// capped in time and bytes so a hostile client cannot hold the goroutine.
func lingerClose(c net.Conn) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tc.CloseWrite()
	_ = tc.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, _ = io.CopyN(io.Discard, tc, 1<<20)
}

// Shutdown stops accepting connections, lets in-flight commands finish and
// their replies flush, then closes every connection. If ctx expires first,
// remaining connections are closed forcibly and ctx's error is returned.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing.Store(true)
	if s.ln != nil {
		_ = s.ln.Close()
	}
	// Wake connections blocked in Read. A connection busy executing a
	// command finishes it, writes the reply, then sees closing and exits.
	for c := range s.conns {
		_ = c.SetReadDeadline(time.Now())
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		<-done
		return ctx.Err()
	}
}
