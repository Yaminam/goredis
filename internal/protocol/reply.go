package protocol

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// Kind identifies the RESP2 type of a Reply.
type Kind uint8

const (
	KindSimple    Kind = iota // +OK
	KindError                 // -ERR message
	KindInteger               // :42
	KindBulk                  // $5\r\nhello
	KindNullBulk              // $-1
	KindArray                 // *2 ...
	KindNullArray             // *-1
)

// Reply is a RESP2 value. Command handlers return Replies; the server encodes
// them. Bulk payloads may alias stored values, which is safe because the store
// never mutates a value slice after storing it.
type Reply struct {
	Kind  Kind
	Str   string // simple string or error message
	Int   int64
	Bulk  []byte
	Array []Reply
}

// Constructors keep handler code short and readable.

func Simple(s string) Reply { return Reply{Kind: KindSimple, Str: s} }
func Err(msg string) Reply  { return Reply{Kind: KindError, Str: msg} }
func Int(n int64) Reply     { return Reply{Kind: KindInteger, Int: n} }
func Bulk(b []byte) Reply   { return Reply{Kind: KindBulk, Bulk: b} }
func BulkString(s string) Reply {
	return Reply{Kind: KindBulk, Bulk: []byte(s)}
}
func NullBulk() Reply            { return Reply{Kind: KindNullBulk} }
func Array(items ...Reply) Reply { return Reply{Kind: KindArray, Array: items} }
func NullArray() Reply           { return Reply{Kind: KindNullArray} }

// OK is the most common reply.
var OK = Simple("OK")

// IsError reports whether the reply is a RESP error.
func (r Reply) IsError() bool { return r.Kind == KindError }

// String renders a reply for test failure messages and logs.
func (r Reply) String() string {
	switch r.Kind {
	case KindSimple:
		return "+" + r.Str
	case KindError:
		return "-" + r.Str
	case KindInteger:
		return ":" + strconv.FormatInt(r.Int, 10)
	case KindBulk:
		return strconv.Quote(string(r.Bulk))
	case KindNullBulk:
		return "(nil)"
	case KindNullArray:
		return "(nil array)"
	case KindArray:
		parts := make([]string, len(r.Array))
		for i, item := range r.Array {
			parts[i] = item.String()
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return "?"
}

// Writer encodes RESP2 values onto a buffered stream. It does not flush on
// its own: the server flushes once per batch of pipelined commands, which
// turns N replies into one write syscall.
type Writer struct {
	bw  *bufio.Writer
	num [24]byte
}

// NewWriter wraps w. If w is already a *bufio.Writer it is used directly.
func NewWriter(w io.Writer) *Writer {
	bw, ok := w.(*bufio.Writer)
	if !ok {
		bw = bufio.NewWriterSize(w, 16<<10)
	}
	return &Writer{bw: bw}
}

// Flush writes buffered data to the underlying writer. bufio.Writer loops
// over short writes, so a partial write from the kernel is retried until all
// bytes are sent or an error (e.g. a write deadline) occurs.
func (w *Writer) Flush() error { return w.bw.Flush() }

// Buffered returns the number of bytes waiting to be flushed.
func (w *Writer) Buffered() int { return w.bw.Buffered() }

// WriteReply encodes r, recursing into arrays.
func (w *Writer) WriteReply(r Reply) error {
	switch r.Kind {
	case KindSimple:
		return w.writeLine('+', sanitize(r.Str))
	case KindError:
		return w.writeLine('-', sanitize(r.Str))
	case KindInteger:
		return w.writeInt(':', r.Int)
	case KindBulk:
		return w.WriteBulk(r.Bulk)
	case KindNullBulk:
		_, err := w.bw.WriteString("$-1\r\n")
		return err
	case KindNullArray:
		_, err := w.bw.WriteString("*-1\r\n")
		return err
	case KindArray:
		if err := w.writeInt('*', int64(len(r.Array))); err != nil {
			return err
		}
		for _, item := range r.Array {
			if err := w.WriteReply(item); err != nil {
				return err
			}
		}
		return nil
	}
	return w.writeLine('-', "ERR internal error: unknown reply kind")
}

// WriteBulk writes a bulk string.
func (w *Writer) WriteBulk(b []byte) error {
	if err := w.writeInt('$', int64(len(b))); err != nil {
		return err
	}
	if _, err := w.bw.Write(b); err != nil {
		return err
	}
	_, err := w.bw.WriteString("\r\n")
	return err
}

// WriteCommand writes args as a RESP array of bulk strings: the format
// clients send and the format of the append-only file.
func (w *Writer) WriteCommand(args ...[]byte) error {
	if err := w.writeInt('*', int64(len(args))); err != nil {
		return err
	}
	for _, a := range args {
		if err := w.WriteBulk(a); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) writeLine(prefix byte, s string) error {
	if err := w.bw.WriteByte(prefix); err != nil {
		return err
	}
	if _, err := w.bw.WriteString(s); err != nil {
		return err
	}
	_, err := w.bw.WriteString("\r\n")
	return err
}

func (w *Writer) writeInt(prefix byte, n int64) error {
	b := append(w.num[:0], prefix)
	b = strconv.AppendInt(b, n, 10)
	b = append(b, '\r', '\n')
	_, err := w.bw.Write(b)
	return err
}

// sanitize keeps simple strings and errors on one line. A CR or LF inside
// them would let the payload forge extra replies.
func sanitize(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, s)
}

// AppendCommand encodes args as a RESP array of bulk strings and appends the
// bytes to dst. It allocates nothing beyond growing dst, which makes it
// suitable for building append-only-file records under a lock.
func AppendCommand(dst []byte, args ...[]byte) []byte {
	dst = append(dst, '*')
	dst = strconv.AppendInt(dst, int64(len(args)), 10)
	dst = append(dst, '\r', '\n')
	for _, a := range args {
		dst = append(dst, '$')
		dst = strconv.AppendInt(dst, int64(len(a)), 10)
		dst = append(dst, '\r', '\n')
		dst = append(dst, a...)
		dst = append(dst, '\r', '\n')
	}
	return dst
}

// ReadReply parses one RESP2 reply. It is used on the client side: by tests,
// the benchmark tool and the smoke-test client. Nested arrays are supported
// up to a fixed depth to keep a hostile server from exhausting the stack.
func (r *Reader) ReadReply() (Reply, error) {
	return r.readReply(0)
}

func (r *Reader) readReply(depth int) (Reply, error) {
	if depth > 16 {
		return Reply{}, protoErr("reply nested too deeply")
	}
	line, err := r.readLine(r.limits.MaxInlineBytes)
	if err != nil {
		return Reply{}, err
	}
	if len(line) == 0 {
		return Reply{}, protoErr("empty reply line")
	}
	body := line[1:]
	switch line[0] {
	case '+':
		return Simple(string(body)), nil
	case '-':
		return Err(string(body)), nil
	case ':':
		n, ok := parseInt(body)
		if !ok {
			return Reply{}, protoErr("invalid integer reply")
		}
		return Int(n), nil
	case '$':
		n, ok := parseInt(body)
		if !ok || n < -1 || n > r.limits.MaxBulkBytes {
			return Reply{}, protoErr("invalid bulk length")
		}
		if n == -1 {
			return NullBulk(), nil
		}
		b, err := r.readBulkBody(n)
		if err != nil {
			return Reply{}, err
		}
		return Bulk(b), nil
	case '*':
		n, ok := parseInt(body)
		if !ok || n < -1 || n > int64(r.limits.MaxArgs)*4 {
			return Reply{}, protoErr("invalid array length")
		}
		if n == -1 {
			return NullArray(), nil
		}
		items := make([]Reply, 0, n)
		for i := int64(0); i < n; i++ {
			item, err := r.readReply(depth + 1)
			if err != nil {
				return Reply{}, unexpected(err)
			}
			items = append(items, item)
		}
		return Array(items...), nil
	}
	return Reply{}, protoErr("unknown reply type '%c'", line[0])
}
