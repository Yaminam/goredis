// Package protocol implements the subset of the Redis Serialization Protocol
// (RESP2) that goredis speaks: parsing client commands, encoding replies, and
// parsing replies (used by tests, the benchmark tool and the smoke client).
//
// The parser never assumes that one network read equals one command. It reads
// through a bufio.Reader, so a command split across many TCP segments, or many
// commands packed into one segment (pipelining), are handled the same way.
package protocol

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// Limits bounds the resources a single client request may consume. Every
// length the client sends is checked against these limits before any memory
// is allocated for it.
type Limits struct {
	// MaxBulkBytes is the largest single argument (bulk string) accepted.
	MaxBulkBytes int64
	// MaxArgs is the largest number of arguments in one command, including
	// the command name.
	MaxArgs int
	// MaxInlineBytes is the longest inline command line (e.g. "PING\r\n"
	// typed into telnet or nc).
	MaxInlineBytes int
	// MaxRequestBytes caps the sum of all argument lengths in one command.
	MaxRequestBytes int64
}

// DefaultLimits are conservative defaults suitable for a development server.
func DefaultLimits() Limits {
	return Limits{
		MaxBulkBytes:    8 << 20,  // 8 MiB per argument
		MaxArgs:         4096,     // DEL/EXISTS with many keys
		MaxInlineBytes:  64 << 10, // 64 KiB
		MaxRequestBytes: 16 << 20, // 16 MiB per command
	}
}

// Error is a protocol violation by the peer. After a protocol error the byte
// stream can no longer be trusted, so servers reply with the error and close
// the connection.
type Error struct {
	Msg string
}

func (e *Error) Error() string { return "Protocol error: " + e.Msg }

func protoErr(format string, args ...any) error {
	return &Error{Msg: fmt.Sprintf(format, args...)}
}

// IsProtocolError reports whether err is (or wraps) a *Error.
func IsProtocolError(err error) bool {
	var pe *Error
	return errors.As(err, &pe)
}

// growChunk is how much memory is reserved at a time while reading a large
// bulk string. A client that announces "$8000000" but then sends nothing
// costs us at most one chunk, not the full announced size.
const growChunk = 64 << 10

// Reader parses RESP commands and replies from a buffered stream.
type Reader struct {
	br      *bufio.Reader
	limits  Limits
	scratch []byte
}

// NewReader wraps r. If r is already a *bufio.Reader it is used directly.
func NewReader(r io.Reader, limits Limits) *Reader {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 16<<10)
	}
	return &Reader{br: br, limits: limits}
}

// Buffered returns the number of bytes already read from the network but not
// yet parsed. Servers use it to decide when to flush pipelined replies.
func (r *Reader) Buffered() int { return r.br.Buffered() }

// PeekByte returns the next unread byte without consuming it.
func (r *Reader) PeekByte() (byte, error) {
	b, err := r.br.Peek(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

// ReadCommand reads one client command and returns its arguments, the first
// being the command name. It accepts both RESP arrays of bulk strings (what
// real clients send) and inline commands (what a human types into nc).
//
// It returns io.EOF if the stream ends cleanly between commands,
// io.ErrUnexpectedEOF if it ends in the middle of a command, and a *Error for
// malformed or oversized input. An empty command (blank inline line or "*0")
// is returned as a nil slice with a nil error; callers should skip it.
func (r *Reader) ReadCommand() ([][]byte, error) {
	b, err := r.br.Peek(1)
	if err != nil {
		return nil, err // io.EOF here means a clean end of stream
	}
	if b[0] == '*' {
		return r.readMultiBulk()
	}
	return r.readInline()
}

func (r *Reader) readMultiBulk() ([][]byte, error) {
	line, err := r.readLine(64)
	if err != nil {
		return nil, unexpected(err)
	}
	n, ok := parseInt(line[1:])
	if !ok {
		return nil, protoErr("invalid multibulk length")
	}
	if n <= 0 {
		return nil, nil // "*0" and "*-1" are treated as no-ops, like Redis
	}
	if n > int64(r.limits.MaxArgs) {
		return nil, protoErr("invalid multibulk length")
	}

	args := make([][]byte, 0, n)
	var total int64
	for i := int64(0); i < n; i++ {
		line, err := r.readLine(64)
		if err != nil {
			return nil, unexpected(err)
		}
		if len(line) == 0 {
			return nil, protoErr("expected '$', got empty line")
		}
		if line[0] != '$' {
			return nil, protoErr("expected '$', got '%c'", line[0])
		}
		size, ok := parseInt(line[1:])
		if !ok || size < 0 || size > r.limits.MaxBulkBytes {
			return nil, protoErr("invalid bulk length")
		}
		total += size
		if total > r.limits.MaxRequestBytes {
			return nil, protoErr("request exceeds max-request-bytes")
		}
		arg, err := r.readBulkBody(size)
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
	}
	return args, nil
}

// readBulkBody reads size bytes followed by CRLF. Large bodies are read in
// chunks so memory grows with bytes actually received, not bytes promised.
func (r *Reader) readBulkBody(size int64) ([]byte, error) {
	initial := size
	if initial > growChunk {
		initial = growChunk
	}
	buf := make([]byte, 0, initial)
	for int64(len(buf)) < size {
		want := size - int64(len(buf))
		if want > growChunk {
			want = growChunk
		}
		start := len(buf)
		buf = append(buf, make([]byte, want)...)
		if _, err := io.ReadFull(r.br, buf[start:]); err != nil {
			return nil, unexpected(err)
		}
	}
	var crlf [2]byte
	if _, err := io.ReadFull(r.br, crlf[:]); err != nil {
		return nil, unexpected(err)
	}
	if crlf[0] != '\r' || crlf[1] != '\n' {
		return nil, protoErr("bulk string not terminated by CRLF")
	}
	return buf, nil
}

func (r *Reader) readInline() ([][]byte, error) {
	line, err := r.readLine(r.limits.MaxInlineBytes)
	if err != nil {
		var pe *Error
		if errors.As(err, &pe) {
			return nil, protoErr("too big inline request")
		}
		return nil, unexpected(err)
	}
	fields := splitInline(line)
	if len(fields) > r.limits.MaxArgs {
		return nil, protoErr("too many arguments in inline request")
	}
	return fields, nil
}

// splitInline splits an inline command on spaces and tabs. Quoting is not
// supported; use the RESP array form for values containing whitespace.
func splitInline(line []byte) [][]byte {
	var out [][]byte
	start := -1
	for i, c := range line {
		if c == ' ' || c == '\t' {
			if start >= 0 {
				out = append(out, append([]byte(nil), line[start:i]...))
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, append([]byte(nil), line[start:]...))
	}
	return out
}

// readLine returns the next line without its terminator. It accepts "\r\n"
// and, for inline commands typed by humans, a bare "\n". The returned slice
// is only valid until the next read. Lines longer than max are rejected.
func (r *Reader) readLine(max int) ([]byte, error) {
	r.scratch = r.scratch[:0]
	for {
		chunk, err := r.br.ReadSlice('\n')
		if len(r.scratch)+len(chunk) > max+2 {
			return nil, protoErr("line too long")
		}
		if err == nil {
			if len(r.scratch) == 0 {
				return trimEOL(chunk)
			}
			r.scratch = append(r.scratch, chunk...)
			return trimEOL(r.scratch)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			r.scratch = append(r.scratch, chunk...)
			continue
		}
		if len(chunk) > 0 || len(r.scratch) > 0 {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
		}
		return nil, err
	}
}

func trimEOL(line []byte) ([]byte, error) {
	line = line[:len(line)-1] // drop '\n'
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	return line, nil
}

// unexpected converts a clean EOF that happens mid-command into
// io.ErrUnexpectedEOF so callers can tell "client hung up between commands"
// from "client hung up halfway through one".
func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// parseInt parses a signed decimal integer strictly: optional leading '-',
// then at least one digit, no '+', no spaces, no overflow.
func parseInt(b []byte) (int64, bool) {
	if len(b) == 0 || len(b) > 20 {
		return 0, false
	}
	neg := false
	if b[0] == '-' {
		neg = true
		b = b[1:]
		if len(b) == 0 {
			return 0, false
		}
	}
	var n int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		d := int64(c - '0')
		if n > (1<<63-1-d)/10 {
			return 0, false
		}
		n = n*10 + d
	}
	if neg {
		n = -n
	}
	return n, true
}

// ParseInt is the strict integer parser used for command arguments such as
// EXPIRE seconds. It is exported so the command layer parses integers exactly
// the way the protocol layer does.
func ParseInt(b []byte) (int64, bool) { return parseInt(b) }
