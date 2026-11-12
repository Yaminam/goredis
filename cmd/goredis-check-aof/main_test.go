package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goredis/internal/protocol"
)

func TestCheckAndFix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "appendonly.aof")
	good := protocol.AppendCommand(nil, []byte("SET"), []byte("a"), []byte("1"))
	good = protocol.AppendCommand(good, []byte("SET"), []byte("b"), []byte("2"))
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{path}, &out, &errb); code != 0 || !strings.Contains(out.String(), "status:        OK") {
		t.Fatalf("clean file: code %d\n%s%s", code, out.String(), errb.String())
	}

	damaged := append(append([]byte{}, good...), "*2\r\n$3\r\nDEL\r\n$1\r\n"...)
	if err := os.WriteFile(path, damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := run([]string{path}, &out, &errb); code != 1 || !strings.Contains(out.String(), "truncated") {
		t.Fatalf("damaged file: code %d\n%s", code, out.String())
	}
	out.Reset()
	if code := run([]string{"-fix", path}, &out, &errb); code != 0 {
		t.Fatalf("fix: code %d\n%s%s", code, out.String(), errb.String())
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, good) {
		t.Fatalf("after fix: %q", got)
	}
	backups, _ := filepath.Glob(path + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("backups: %v", backups)
	}
	if b, _ := os.ReadFile(backups[0]); !bytes.Equal(b, damaged) {
		t.Fatal("backup does not match the original damaged file")
	}
}
