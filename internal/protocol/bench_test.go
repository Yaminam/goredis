package protocol

import (
	"bytes"
	"io"
	"testing"
)

func BenchmarkReadCommandSet(b *testing.B) {
	cmd := AppendCommand(nil, []byte("SET"), []byte("key:12345"), bytes.Repeat([]byte("x"), 64))
	data := bytes.Repeat(cmd, 1000)
	b.SetBytes(int64(len(cmd)))
	r := bytes.NewReader(data)
	pr := NewReader(r, DefaultLimits())
	for b.Loop() {
		if _, err := pr.ReadCommand(); err == io.EOF {
			r.Reset(data)
			pr = NewReader(r, DefaultLimits())
		}
	}
}

func BenchmarkWriteBulkReply(b *testing.B) {
	w := NewWriter(io.Discard)
	v := bytes.Repeat([]byte("x"), 64)
	for b.Loop() {
		_ = w.WriteReply(Bulk(v))
	}
	_ = w.Flush()
}
