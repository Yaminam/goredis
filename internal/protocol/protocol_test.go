package protocol

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
)

func reader(s string) *Reader {
	return NewReader(strings.NewReader(s), DefaultLimits())
}

func argsToStrings(args [][]byte) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = string(a)
	}
	return out
}

func TestReadCommandValid(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"ping", "*1\r\n$4\r\nPING\r\n", []string{"PING"}},
		{"set", "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$5\r\nhello\r\n", []string{"SET", "k", "hello"}},
		{"empty bulk", "*2\r\n$4\r\nECHO\r\n$0\r\n\r\n", []string{"ECHO", ""}},
		{"binary safe", "*2\r\n$4\r\nECHO\r\n$4\r\na\r\nb\r\n", []string{"ECHO", "a\r\nb"}},
		{"inline", "PING\r\n", []string{"PING"}},
		{"inline bare LF", "SET k v\n", []string{"SET", "k", "v"}},
		{"inline extra spaces", "  GET \t key  \r\n", []string{"GET", "key"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := reader(tt.in).ReadCommand()
			if err != nil {
				t.Fatalf("ReadCommand: %v", err)
			}
			if g := argsToStrings(got); strings.Join(g, "|") != strings.Join(tt.want, "|") {
				t.Fatalf("got %q, want %q", g, tt.want)
			}
		})
	}
}

func TestReadCommandEmptyIsSkipped(t *testing.T) {
	for _, in := range []string{"*0\r\n", "*-1\r\n", "\r\n", "   \n"} {
		got, err := reader(in).ReadCommand()
		if err != nil || got != nil {
			t.Errorf("%q: got (%q, %v), want (nil, nil)", in, got, err)
		}
	}
}

// One TCP read is not one command: feed the parser a single byte at a time.
func TestReadCommandFragmented(t *testing.T) {
	in := "*3\r\n$3\r\nSET\r\n$3\r\nkey\r\n$11\r\nhello world\r\n"
	r := NewReader(iotest.OneByteReader(strings.NewReader(in)), DefaultLimits())
	got, err := r.ReadCommand()
	if err != nil {
		t.Fatal(err)
	}
	if g := strings.Join(argsToStrings(got), "|"); g != "SET|key|hello world" {
		t.Fatalf("got %q", g)
	}
}

// Many commands in one read (pipelining) must come out one at a time.
func TestReadCommandPipelined(t *testing.T) {
	in := "*1\r\n$4\r\nPING\r\n" +
		"*3\r\n$3\r\nSET\r\n$1\r\na\r\n$1\r\n1\r\n" +
		"GET a\r\n" +
		"*2\r\n$3\r\nGET\r\n$1\r\na\r\n"
	r := reader(in)
	var got []string
	for {
		args, err := r.ReadCommand()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, strings.Join(argsToStrings(args), " "))
	}
	want := []string{"PING", "SET a 1", "GET a", "GET a"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestReadCommandIncomplete(t *testing.T) {
	full := "*3\r\n$3\r\nSET\r\n$3\r\nkey\r\n$5\r\nvalue\r\n"
	// Every strict prefix of a valid command must report an unexpected EOF,
	// never a parsed command and never a clean EOF.
	for i := 1; i < len(full); i++ {
		_, err := reader(full[:i]).ReadCommand()
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("prefix %d %q: got %v, want io.ErrUnexpectedEOF", i, full[:i], err)
		}
	}
	if _, err := reader("").ReadCommand(); !errors.Is(err, io.EOF) {
		t.Errorf("empty stream: got %v, want io.EOF", err)
	}
}

func TestReadCommandMalformed(t *testing.T) {
	tests := map[string]string{
		"non-numeric array len": "*x\r\n",
		"plus sign array len":   "*+1\r\n",
		"missing dollar":        "*1\r\n+PING\r\n",
		"empty header line":     "*1\r\n\r\n",
		"negative bulk len":     "*1\r\n$-5\r\n",
		"non-numeric bulk len":  "*1\r\n$abc\r\n",
		"bad bulk terminator":   "*1\r\n$4\r\nPINGxx",
		"overflowing length":    "*99999999999999999999999\r\n",
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := reader(in).ReadCommand()
			if !IsProtocolError(err) {
				t.Fatalf("got %v, want protocol error", err)
			}
		})
	}
}

func TestReadCommandLimits(t *testing.T) {
	lim := Limits{MaxBulkBytes: 10, MaxArgs: 3, MaxInlineBytes: 16, MaxRequestBytes: 15}
	tests := map[string]string{
		"too many args":        "*4\r\n",
		"bulk too large":       "*1\r\n$11\r\n",
		"request too large":    "*2\r\n$8\r\n12345678\r\n$8\r\n",
		"inline too long":      strings.Repeat("A", 100) + "\r\n",
		"inline too many args": "a b c d\r\n",
		"header line too long": "*" + strings.Repeat("1", 100) + "\r\n",
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NewReader(strings.NewReader(in), lim).ReadCommand()
			if !IsProtocolError(err) {
				t.Fatalf("got %v, want protocol error", err)
			}
		})
	}
	// Exactly at the limit is fine.
	in := "*1\r\n$10\r\n0123456789\r\n"
	if _, err := NewReader(strings.NewReader(in), lim).ReadCommand(); err != nil {
		t.Fatalf("at-limit request rejected: %v", err)
	}
}

// A client that announces a huge bulk string and then stalls must not make
// the server allocate the announced size up front.
func TestReadBulkAllocatesIncrementally(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxBulkBytes = 1 << 30
	lim.MaxRequestBytes = 1 << 30
	in := "*1\r\n$1000000000\r\n" + strings.Repeat("x", 10)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := NewReader(strings.NewReader(in), lim).ReadCommand()
	runtime.ReadMemStats(&after)

	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v, want io.ErrUnexpectedEOF", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("allocated %d bytes for a 10-byte body; want < 1 MiB", allocated)
	}
}

func TestWriterEncoding(t *testing.T) {
	tests := []struct {
		reply Reply
		want  string
	}{
		{OK, "+OK\r\n"},
		{Err("ERR boom"), "-ERR boom\r\n"},
		{Err("ERR line1\r\nline2"), "-ERR line1  line2\r\n"},
		{Int(-42), ":-42\r\n"},
		{BulkString("hi"), "$2\r\nhi\r\n"},
		{Bulk([]byte{}), "$0\r\n\r\n"},
		{NullBulk(), "$-1\r\n"},
		{NullArray(), "*-1\r\n"},
		{Array(Int(1), BulkString("a"), Array()), "*3\r\n:1\r\n$1\r\na\r\n*0\r\n"},
	}
	for _, tt := range tests {
		var buf bytes.Buffer
		w := NewWriter(&buf)
		if err := w.WriteReply(tt.reply); err != nil {
			t.Fatal(err)
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if buf.String() != tt.want {
			t.Errorf("%s: got %q, want %q", tt.reply, buf.String(), tt.want)
		}
	}
}

func TestAppendCommandMatchesWriter(t *testing.T) {
	args := [][]byte{[]byte("SET"), []byte("k"), []byte("v\r\nwith crlf"), {}}
	var buf bytes.Buffer
	w := NewWriter(&buf)
	_ = w.WriteCommand(args...)
	_ = w.Flush()
	if got := AppendCommand(nil, args...); !bytes.Equal(got, buf.Bytes()) {
		t.Fatalf("AppendCommand %q != WriteCommand %q", got, buf.Bytes())
	}
	// And the parser reads it back unchanged.
	parsed, err := NewReader(bytes.NewReader(buf.Bytes()), DefaultLimits()).ReadCommand()
	if err != nil {
		t.Fatal(err)
	}
	for i := range args {
		if !bytes.Equal(parsed[i], args[i]) {
			t.Fatalf("arg %d: got %q, want %q", i, parsed[i], args[i])
		}
	}
}

func TestReplyRoundTrip(t *testing.T) {
	replies := []Reply{
		OK, Err("ERR x"), Int(7), BulkString("bulk"), NullBulk(), NullArray(),
		Array(Simple("a"), Array(Int(1), NullBulk()), BulkString("")),
	}
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for _, r := range replies {
		_ = w.WriteReply(r)
	}
	_ = w.Flush()
	r := NewReader(iotest.HalfReader(&buf), DefaultLimits())
	for _, want := range replies {
		got, err := r.ReadReply()
		if err != nil {
			t.Fatal(err)
		}
		if got.String() != want.String() {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

func TestParseInt(t *testing.T) {
	good := map[string]int64{"0": 0, "-1": -1, "123": 123, "9223372036854775807": 1<<63 - 1}
	for in, want := range good {
		if got, ok := ParseInt([]byte(in)); !ok || got != want {
			t.Errorf("ParseInt(%q) = %d, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "-", "+1", " 1", "1 ", "1.5", "9223372036854775808", "abc"} {
		if _, ok := ParseInt([]byte(in)); ok {
			t.Errorf("ParseInt(%q) accepted", in)
		}
	}
}

// FuzzReadCommand checks that no input can make the parser panic or return
// arguments that violate the configured limits.
func FuzzReadCommand(f *testing.F) {
	f.Add([]byte("*1\r\n$4\r\nPING\r\n"))
	f.Add([]byte("*2\r\n$3\r\nGET\r\n$1\r\nk\r\n"))
	f.Add([]byte("SET a b\r\n"))
	f.Add([]byte("*1\r\n$-1\r\n"))
	f.Add([]byte("*3\r\n$3\r\nSET\r\n"))
	lim := Limits{MaxBulkBytes: 1024, MaxArgs: 16, MaxInlineBytes: 256, MaxRequestBytes: 4096}
	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewReader(bufio.NewReaderSize(bytes.NewReader(data), 16), lim)
		for i := 0; i < 64; i++ {
			args, err := r.ReadCommand()
			if err != nil {
				return
			}
			if len(args) > lim.MaxArgs {
				t.Fatalf("got %d args, limit %d", len(args), lim.MaxArgs)
			}
			for _, a := range args {
				if int64(len(a)) > lim.MaxBulkBytes && int64(len(a)) > int64(lim.MaxInlineBytes) {
					t.Fatalf("arg of %d bytes exceeds limits", len(a))
				}
			}
		}
	})
}
