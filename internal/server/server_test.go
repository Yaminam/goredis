package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"goredis/internal/command"
	"goredis/internal/protocol"
	"goredis/internal/store"
)

type testServer struct {
	srv  *Server
	addr string
	errc chan error
}

func start(t *testing.T, cfg Config) *testServer {
	t.Helper()
	if cfg.Limits == (protocol.Limits{}) {
		cfg.Limits = protocol.DefaultLimits()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{
		srv:  New(cfg, command.New(store.New(nil), command.Options{}), nil),
		addr: ln.Addr().String(),
		errc: make(chan error, 1),
	}
	go func() { ts.errc <- ts.srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ts.srv.Shutdown(ctx)
	})
	return ts
}

// client is a minimal RESP client for tests.
type client struct {
	conn net.Conn
	r    *protocol.Reader
}

func dial(t *testing.T, addr string) *client {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	return &client{conn: c, r: protocol.NewReader(c, protocol.DefaultLimits())}
}

func (c *client) send(t *testing.T, args ...string) {
	t.Helper()
	b := make([][]byte, len(args))
	for i, a := range args {
		b[i] = []byte(a)
	}
	if _, err := c.conn.Write(protocol.AppendCommand(nil, b...)); err != nil {
		t.Fatal(err)
	}
}

func (c *client) read(t *testing.T) protocol.Reply {
	t.Helper()
	r, err := c.r.ReadReply()
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	return r
}

func (c *client) do(t *testing.T, args ...string) string {
	t.Helper()
	c.send(t, args...)
	return c.read(t).String()
}

func TestPingSetGetOverTCP(t *testing.T) {
	ts := start(t, Config{})
	c := dial(t, ts.addr)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"PING"}, "+PONG"},
		{[]string{"SET", "greeting", "hello world"}, "+OK"},
		{[]string{"GET", "greeting"}, `"hello world"`},
		{[]string{"GET", "nope"}, "(nil)"},
		{[]string{"BOGUS"}, "-ERR unknown command 'BOGUS', with args beginning with: "},
	} {
		if got := c.do(t, tc.args...); got != tc.want {
			t.Fatalf("%v: got %s, want %s", tc.args, got, tc.want)
		}
	}
	// The connection survives an unknown command.
	if got := c.do(t, "PING"); got != "+PONG" {
		t.Fatalf("after error: got %s", got)
	}
}

func TestInlineCommands(t *testing.T) {
	ts := start(t, Config{})
	c := dial(t, ts.addr)
	if _, err := io.WriteString(c.conn, "SET k v\r\nGET k\r\nPING\n"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"+OK", `"v"`, "+PONG"} {
		if got := c.read(t).String(); got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
}

// Many commands written in one go must each get a reply, in order.
func TestPipelining(t *testing.T) {
	ts := start(t, Config{})
	c := dial(t, ts.addr)
	var buf []byte
	const n = 500
	for i := 0; i < n; i++ {
		buf = protocol.AppendCommand(buf, []byte("SET"), []byte(fmt.Sprint("k", i)), []byte(fmt.Sprint(i)))
		buf = protocol.AppendCommand(buf, []byte("GET"), []byte(fmt.Sprint("k", i)))
	}
	go func() { _, _ = c.conn.Write(buf) }()
	for i := 0; i < n; i++ {
		if got := c.read(t).String(); got != "+OK" {
			t.Fatalf("reply %d: got %s", 2*i, got)
		}
		if got := c.read(t).String(); got != fmt.Sprintf("%q", fmt.Sprint(i)) {
			t.Fatalf("reply %d: got %s", 2*i+1, got)
		}
	}
}

// A command trickling in one byte per write must still parse correctly.
func TestFragmentedWrites(t *testing.T) {
	ts := start(t, Config{})
	c := dial(t, ts.addr)
	cmd := protocol.AppendCommand(nil, []byte("SET"), []byte("frag"), []byte("mented value"))
	cmd = protocol.AppendCommand(cmd, []byte("GET"), []byte("frag"))
	if tcp, ok := c.conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	for _, b := range cmd {
		if _, err := c.conn.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.read(t).String(); got != "+OK" {
		t.Fatalf("got %s", got)
	}
	if got := c.read(t).String(); got != `"mented value"` {
		t.Fatalf("got %s", got)
	}
}

func TestConcurrentClients(t *testing.T) {
	ts := start(t, Config{})
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", ts.addr)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			r := protocol.NewReader(conn, protocol.DefaultLimits())
			for i := 0; i < 200; i++ {
				key := fmt.Sprintf("client%d:key%d", w, i)
				val := fmt.Sprintf("value-%d-%d", w, i)
				_, _ = conn.Write(protocol.AppendCommand(nil, []byte("SET"), []byte(key), []byte(val)))
				_, _ = conn.Write(protocol.AppendCommand(nil, []byte("GET"), []byte(key)))
				if rep, err := r.ReadReply(); err != nil || rep.String() != "+OK" {
					errs <- fmt.Errorf("SET %s: %v %v", key, rep, err)
					return
				}
				if rep, err := r.ReadReply(); err != nil || string(rep.Bulk) != val {
					errs <- fmt.Errorf("GET %s: %v %v", key, rep, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// After a protocol error the server replies with an error and closes the
// connection, because it can no longer find command boundaries.
func TestMalformedInputClosesConnection(t *testing.T) {
	ts := start(t, Config{})
	for name, in := range map[string]string{
		"bad array length": "*abc\r\n",
		"missing dollar":   "*1\r\n:5\r\n",
		"bad terminator":   "*1\r\n$4\r\nPINGXX",
	} {
		t.Run(name, func(t *testing.T) {
			c := dial(t, ts.addr)
			_, _ = io.WriteString(c.conn, in)
			rep := c.read(t)
			if !rep.IsError() || !strings.Contains(rep.Str, "Protocol error") {
				t.Fatalf("got %s, want protocol error", rep)
			}
			if _, err := c.r.ReadReply(); !errors.Is(err, io.EOF) {
				t.Fatalf("connection not closed after protocol error: %v", err)
			}
		})
	}
}

// Even when the client keeps sending after a protocol error, the error reply
// must arrive (no TCP reset racing it).
func TestProtocolErrorReplySurvivesTrailingInput(t *testing.T) {
	ts := start(t, Config{})
	c := dial(t, ts.addr)
	_, _ = io.WriteString(c.conn, "*1\r\n:oops\r\n"+strings.Repeat("GARBAGE ", 4096))
	rep := c.read(t)
	if !rep.IsError() || !strings.Contains(rep.Str, "Protocol error") {
		t.Fatalf("got %s, want protocol error", rep)
	}
}

func TestOversizedRequestRejected(t *testing.T) {
	lim := protocol.DefaultLimits()
	lim.MaxBulkBytes = 1024
	ts := start(t, Config{Limits: lim})
	c := dial(t, ts.addr)
	_, _ = io.WriteString(c.conn, "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1025\r\n")
	rep := c.read(t)
	if !rep.IsError() || !strings.Contains(rep.Str, "invalid bulk length") {
		t.Fatalf("got %s", rep)
	}
	// The server must still serve other clients.
	if got := dial(t, ts.addr).do(t, "PING"); got != "+PONG" {
		t.Fatalf("server unhealthy after oversized request: %s", got)
	}
}

func TestMaxClients(t *testing.T) {
	ts := start(t, Config{MaxClients: 2})
	a, b := dial(t, ts.addr), dial(t, ts.addr)
	// Make sure both are registered before the third connects.
	a.do(t, "PING")
	b.do(t, "PING")
	c := dial(t, ts.addr)
	rep := c.read(t)
	if !rep.IsError() || !strings.Contains(rep.Str, "max number of clients") {
		t.Fatalf("third client got %s", rep)
	}
	a.conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for ts.srv.ActiveConnections() > 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := dial(t, ts.addr).do(t, "PING"); got != "+PONG" {
		t.Fatalf("slot not freed after a client left: %s", got)
	}
}

func TestIdleTimeout(t *testing.T) {
	ts := start(t, Config{IdleTimeout: 100 * time.Millisecond})
	c := dial(t, ts.addr)
	c.do(t, "PING")
	time.Sleep(300 * time.Millisecond)
	if _, err := c.r.ReadReply(); !errors.Is(err, io.EOF) {
		t.Fatalf("idle connection not closed: %v", err)
	}
}

// Shutdown must not wait for idle clients, must stop accepting, and must
// return promptly.
func TestGracefulShutdown(t *testing.T) {
	ts := start(t, Config{IdleTimeout: time.Hour})
	idle := dial(t, ts.addr)
	idle.do(t, "SET", "k", "v")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	began := time.Now()
	if err := ts.srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if d := time.Since(began); d > 2*time.Second {
		t.Fatalf("Shutdown took %v with only idle clients", d)
	}
	if err := <-ts.errc; !errors.Is(err, ErrServerClosed) {
		t.Fatalf("Serve returned %v, want ErrServerClosed", err)
	}
	if _, err := idle.r.ReadReply(); !errors.Is(err, io.EOF) {
		t.Fatalf("idle client not disconnected: %v", err)
	}
	if c, err := net.DialTimeout("tcp", ts.addr, time.Second); err == nil {
		c.Close()
		t.Fatal("server still accepting after Shutdown")
	}
	if n := ts.srv.ActiveConnections(); n != 0 {
		t.Fatalf("%d connections still tracked", n)
	}
}

// Replies already computed before shutdown must reach the client.
func TestShutdownFlushesPipelinedReplies(t *testing.T) {
	ts := start(t, Config{})
	c := dial(t, ts.addr)
	w := bufio.NewWriter(c.conn)
	_, _ = w.Write(protocol.AppendCommand(nil, []byte("SET"), []byte("a"), []byte("1")))
	_, _ = w.Write(protocol.AppendCommand(nil, []byte("GET"), []byte("a")))
	_ = w.Flush()
	if got := c.read(t).String(); got != "+OK" {
		t.Fatalf("got %s", got)
	}
	if got := c.read(t).String(); got != `"1"` {
		t.Fatalf("got %s", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ts.srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestQuitClosesAfterReply(t *testing.T) {
	ts := start(t, Config{})
	c := dial(t, ts.addr)
	if got := c.do(t, "QUIT"); got != "+OK" {
		t.Fatalf("QUIT: got %s", got)
	}
	if _, err := c.r.ReadReply(); !errors.Is(err, io.EOF) {
		t.Fatalf("connection still open after QUIT: %v", err)
	}
}

func TestTTLOverTCP(t *testing.T) {
	ts := start(t, Config{})
	c := dial(t, ts.addr)
	for _, step := range [][2]string{
		{"SET k v PX 150", "+OK"},
		{"PTTL k", ""},
		{"EXISTS k", ":1"},
	} {
		got := c.do(t, strings.Fields(step[0])...)
		if step[1] != "" && got != step[1] {
			t.Fatalf("%s: got %s, want %s", step[0], got, step[1])
		}
	}
	time.Sleep(300 * time.Millisecond)
	if got := c.do(t, "GET", "k"); got != "(nil)" {
		t.Fatalf("key alive after its TTL: %s", got)
	}
	if got := c.do(t, "DBSIZE"); got != ":0" {
		t.Fatalf("DBSIZE counts expired key: %s", got)
	}
}
