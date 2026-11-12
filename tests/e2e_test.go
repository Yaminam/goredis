// Package tests holds end-to-end tests: each test starts a complete goredis
// instance (config, AOF, TCP server, admin HTTP) on random ports and talks
// to it over real sockets, exactly as an external client would.
package tests

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"goredis/internal/app"
	"goredis/internal/config"
	"goredis/internal/protocol"
)

func testConfig(t *testing.T, dir string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Addr = "127.0.0.1:0"
	cfg.AdminAddr = "127.0.0.1:0"
	cfg.AOFPath = filepath.Join(dir, "appendonly.aof")
	cfg.AOFFsync = 1 // always
	cfg.ShutdownTimeout = 5 * time.Second
	return cfg
}

func startApp(t *testing.T, cfg config.Config) *app.App {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := app.New(cfg, log, "test")
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	a.Start()
	t.Cleanup(func() { stopApp(t, a) })
	return a
}

func stopApp(t *testing.T, a *app.App) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

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

func (c *client) do(t *testing.T, args ...string) string {
	t.Helper()
	b := make([][]byte, len(args))
	for i, a := range args {
		b[i] = []byte(a)
	}
	if _, err := c.conn.Write(protocol.AppendCommand(nil, b...)); err != nil {
		t.Fatal(err)
	}
	rep, err := c.r.ReadReply()
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return rep.String()
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestEndToEndPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, dir)

	a := startApp(t, cfg)
	c := dial(t, a.Addr())
	for _, step := range [][]string{
		{"SET", "user:1", "alice"},
		{"SET", "session", "xyz", "EX", "3600"},
		{"SET", "temp", "gone-soon", "PX", "50"},
		{"SET", "deleted", "x"},
		{"DEL", "deleted"},
	} {
		if got := c.do(t, step...); got != "+OK" && got != ":1" {
			t.Fatalf("%v: %s", step, got)
		}
	}
	stopApp(t, a)
	time.Sleep(100 * time.Millisecond) // let "temp" expire while the server is down

	b := startApp(t, cfg)
	c2 := dial(t, b.Addr())
	checks := map[string]string{
		"GET user:1":  `"alice"`,
		"GET temp":    "(nil)",
		"GET deleted": "(nil)",
		"DBSIZE":      ":2",
	}
	for cmd, want := range checks {
		if got := c2.do(t, strings.Fields(cmd)...); got != want {
			t.Errorf("%s after restart: got %s, want %s", cmd, got, want)
		}
	}
	if got := c2.do(t, "TTL", "session"); got != ":3600" && got != ":3599" {
		t.Errorf("TTL session after restart: %s", got)
	}
}

func TestEndToEndHealthAndMetrics(t *testing.T) {
	a := startApp(t, testConfig(t, t.TempDir()))
	admin := "http://" + a.AdminAddr()

	if code, body := httpGet(t, admin+"/readyz"); code != 200 || !strings.Contains(body, "ready") {
		t.Fatalf("/readyz: %d %s", code, body)
	}
	if code, body := httpGet(t, admin+"/healthz"); code != 200 || !strings.Contains(body, `"aof_healthy":true`) {
		t.Fatalf("/healthz: %d %s", code, body)
	}

	c := dial(t, a.Addr())
	c.do(t, "SET", "k", "v")
	c.do(t, "GET", "k")
	c.do(t, "GET")
	c.do(t, "this-command-does-not-exist")

	_, metrics := httpGet(t, admin+"/metrics")
	for _, want := range []string{
		`goredis_commands_total{command="set"} 1`,
		`goredis_commands_total{command="get"} 2`,
		`goredis_command_errors_total{command="get"} 1`,
		`goredis_commands_total{command="unknown"} 1`,
		`goredis_command_duration_seconds_bucket{command="set",le=`,
		"goredis_connected_clients 1",
		"goredis_keys 1",
		"goredis_aof_enabled 1",
		"goredis_aof_healthy 1",
		"goredis_aof_recovery_success 1",
		`goredis_aof_rewrites_total{result="success"} 0`,
		`goredis_build_info{goversion="go`,
		"goredis_uptime_seconds",
		"process_resident_memory_bytes",
		"go_goroutines",
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if strings.Contains(metrics, "this-command-does-not-exist") {
		t.Error("client input leaked into a metric label")
	}

	info := c.do(t, "INFO", "persistence")
	if !strings.Contains(info, "aof_last_write_status:ok") || !strings.Contains(info, "aof_fsync:always") {
		t.Errorf("INFO persistence: %s", info)
	}
}

func TestEndToEndReadinessFlipsOnShutdown(t *testing.T) {
	a := startApp(t, testConfig(t, t.TempDir()))
	if !a.Ready() {
		t.Fatal("not ready after Start")
	}
	stopApp(t, a)
	if a.Ready() {
		t.Fatal("still ready after Shutdown")
	}
}

func TestEndToEndAuth(t *testing.T) {
	cfg := testConfig(t, t.TempDir())
	cfg.Password = "correct horse"
	cfg.DisabledCommands = []string{"FLUSHDB"}
	a := startApp(t, cfg)
	c := dial(t, a.Addr())
	if got := c.do(t, "SET", "k", "v"); got != "-NOAUTH Authentication required." {
		t.Fatalf("unauthenticated SET: %s", got)
	}
	if got := c.do(t, "AUTH", "correct horse"); got != "+OK" {
		t.Fatalf("AUTH: %s", got)
	}
	if got := c.do(t, "SET", "k", "v"); got != "+OK" {
		t.Fatalf("SET after AUTH: %s", got)
	}
	if got := c.do(t, "FLUSHDB"); !strings.Contains(got, "disabled by server configuration") {
		t.Fatalf("FLUSHDB: %s", got)
	}
	// A fresh connection starts unauthenticated.
	if got := dial(t, a.Addr()).do(t, "GET", "k"); got != "-NOAUTH Authentication required." {
		t.Fatalf("new connection: %s", got)
	}
}

func TestEndToEndSecondInstanceRefused(t *testing.T) {
	dir := t.TempDir()
	startApp(t, testConfig(t, dir))
	_, err := app.New(testConfig(t, dir), slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	if err == nil || !strings.Contains(err.Error(), "held by another process") {
		t.Fatalf("second instance on the same data dir: %v", err)
	}
}

func TestEndToEndRewriteThenRestart(t *testing.T) {
	cfg := testConfig(t, t.TempDir())
	a := startApp(t, cfg)
	c := dial(t, a.Addr())
	for i := 0; i < 200; i++ {
		c.do(t, "SET", "hot", strings.Repeat("v", i))
	}
	if got := c.do(t, "BGREWRITEAOF"); got != "+Background append only file rewriting started" {
		t.Fatalf("BGREWRITEAOF: %s", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(c.do(t, "INFO", "persistence"), "aof_rewrites_ok:1") {
		if time.Now().After(deadline) {
			t.Fatal("rewrite did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.do(t, "SET", "after", "rewrite")
	stopApp(t, a)

	b := startApp(t, cfg)
	c2 := dial(t, b.Addr())
	if got := c2.do(t, "STRLEN", "hot"); !strings.Contains(got, "unknown command") {
		t.Logf("STRLEN unexpectedly supported: %s", got)
	}
	if got := c2.do(t, "GET", "after"); got != `"rewrite"` {
		t.Fatalf("GET after: %s", got)
	}
	if got := c2.do(t, "DBSIZE"); got != ":2" {
		t.Fatalf("DBSIZE: %s", got)
	}
}

// Many clients writing concurrently through the full stack, then a restart:
// every acknowledged write must be present.
func TestEndToEndConcurrentWritersDurable(t *testing.T) {
	cfg := testConfig(t, t.TempDir())
	cfg.AOFFsync = 0 // everysec: acknowledged writes survive a process restart
	a := startApp(t, cfg)
	const clients, perClient = 16, 100
	var wg sync.WaitGroup
	for w := 0; w < clients; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := dial(t, a.Addr())
			for i := 0; i < perClient; i++ {
				key := "c" + string(rune('a'+w)) + ":" + strings.Repeat("x", i%5) + string(rune('0'+i%10)) + "-" + time.Duration(i).String()
				if got := c.do(t, "SET", key, "v"); got != "+OK" {
					t.Errorf("SET: %s", got)
					return
				}
			}
		}()
	}
	wg.Wait()
	before := dial(t, a.Addr()).do(t, "DBSIZE")
	stopApp(t, a)

	b := startApp(t, cfg)
	if after := dial(t, b.Addr()).do(t, "DBSIZE"); after != before || after != ":1600" {
		t.Fatalf("DBSIZE before restart %s, after %s, want :1600", before, after)
	}
}
