package config

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goredis/internal/persistence"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	c, err := Load(nil, envMap(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "127.0.0.1:6379" || c.AdminAddr != "127.0.0.1:9379" || !c.AOFEnabled ||
		c.AOFFsync != persistence.FsyncEverySec || !c.ProtectedMode || c.Password != "" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestEnvThenFlagsPrecedence(t *testing.T) {
	env := envMap(map[string]string{
		"GOREDIS_ADDR":              "127.0.0.1:7000",
		"GOREDIS_AOF_FSYNC":         "always",
		"GOREDIS_MAXMEMORY":         "200mb",
		"GOREDIS_IDLE_TIMEOUT":      "30s",
		"GOREDIS_LOG_LEVEL":         "debug",
		"GOREDIS_DISABLED_COMMANDS": "flushdb, bgrewriteaof",
		"GOREDIS_AOF_ENABLED":       "false",
	})
	c, err := Load([]string{"-addr", "127.0.0.1:7001", "-aof-fsync", "no"}, env, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "127.0.0.1:7001" || c.AOFFsync != persistence.FsyncNo {
		t.Fatalf("flags did not override env: %s %s", c.Addr, c.AOFFsync)
	}
	if c.MaxMemory != 200<<20 || c.IdleTimeout != 30*time.Second || c.LogLevel != slog.LevelDebug || c.AOFEnabled {
		t.Fatalf("env not applied: %+v", c)
	}
	if strings.Join(c.DisabledCommands, ",") != "FLUSHDB,BGREWRITEAOF" {
		t.Fatalf("disabled commands: %v", c.DisabledCommands)
	}
}

func TestInvalidValuesAreReported(t *testing.T) {
	cases := []struct {
		args []string
		env  map[string]string
		want string
	}{
		{nil, map[string]string{"GOREDIS_AOF_FSYNC": "sometimes"}, "GOREDIS_AOF_FSYNC"},
		{nil, map[string]string{"GOREDIS_MAX_CLIENTS": "lots"}, "GOREDIS_MAX_CLIENTS"},
		{[]string{"-idle-timeout", "5"}, nil, "invalid duration"},
		{[]string{"-log-format", "xml"}, nil, "invalid value"},
		{[]string{"-max-clients", "0"}, nil, "max-clients"},
		{[]string{"-addr", "nonsense"}, nil, "addr"},
		{[]string{"extra"}, nil, "unexpected arguments"},
	}
	for _, tc := range cases {
		_, err := Load(tc.args, envMap(tc.env), io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v %v: got %v, want error containing %q", tc.args, tc.env, err, tc.want)
		}
	}
}

func TestProtectedMode(t *testing.T) {
	_, err := Load([]string{"-addr", "0.0.0.0:6379"}, envMap(nil), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "protected mode") {
		t.Fatalf("public bind without password: %v", err)
	}
	if _, err := Load([]string{"-addr", "0.0.0.0:6379"}, envMap(map[string]string{"GOREDIS_REQUIREPASS": "pw"}), io.Discard); err != nil {
		t.Fatalf("public bind with password: %v", err)
	}
	if _, err := Load([]string{"-addr", ":6379", "-protected-mode=false"}, envMap(nil), io.Discard); err != nil {
		t.Fatalf("explicit opt-out: %v", err)
	}
	for addr, want := range map[string]bool{
		"127.0.0.1:1": true, "localhost:1": true, "[::1]:1": true,
		"0.0.0.0:1": false, ":1": false, "10.0.0.5:1": false,
	} {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v", addr, got)
		}
	}
}

func TestPasswordFileAndNoLeak(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load([]string{"-requirepass-file", path}, envMap(map[string]string{"GOREDIS_REQUIREPASS": "from-env"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Password != "from-file" {
		t.Fatalf("password = %q; the file should win and its trailing newline be trimmed", c.Password)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("config", "config", c)
	if strings.Contains(buf.String(), "from-file") || !strings.Contains(buf.String(), `"auth_enabled":true`) {
		t.Fatalf("log output leaks the password or hides auth state: %s", buf.String())
	}
}

func TestParseBytes(t *testing.T) {
	good := map[string]int64{"0": 0, "1024": 1024, "1kb": 1024, "200MB": 200 << 20, "2gb": 2 << 30, "5b": 5}
	for in, want := range good {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "-1", "lots", "1tb", "99999999999gb"} {
		if _, err := ParseBytes(in); err == nil {
			t.Errorf("ParseBytes(%q) accepted", in)
		}
	}
}
