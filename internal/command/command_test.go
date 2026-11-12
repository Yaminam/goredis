package command

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goredis/internal/protocol"
	"goredis/internal/store"
)

type fakeClock struct{ ms atomic.Int64 }

func (c *fakeClock) Now() time.Time          { return time.UnixMilli(c.ms.Load()) }
func (c *fakeClock) Advance(d time.Duration) { c.ms.Add(d.Milliseconds()) }

const t0 = 1_700_000_000_000 // fixed start time in Unix ms

// fakeAOF records Append calls and can be made to fail.
type fakeAOF struct {
	mu       sync.Mutex
	records  []string
	err      error // returned by Err
	waitErr  error // returned by Wait
	rewrites int
}

func (f *fakeAOF) Err() error { f.mu.Lock(); defer f.mu.Unlock(); return f.err }
func (f *fakeAOF) Append(args ...[]byte) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = string(a)
	}
	f.records = append(f.records, strings.Join(parts, " "))
	return uint64(len(f.records))
}
func (f *fakeAOF) Wait(uint64) error   { return f.waitErr }
func (f *fakeAOF) StartRewrite() error { f.rewrites++; return nil }
func (f *fakeAOF) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.records
	f.records = nil
	return r
}

type env struct {
	e     *Engine
	clock *fakeClock
	aof   *fakeAOF
	sess  *Session
}

func newEnv(t *testing.T, opts Options) *env {
	t.Helper()
	clock := &fakeClock{}
	clock.ms.Store(t0)
	aof := &fakeAOF{}
	if opts.AOF == nil {
		opts.AOF = aof
	}
	return &env{e: New(store.New(clock.Now), opts), clock: clock, aof: aof, sess: NewSession()}
}

func (v *env) run(cmd ...string) protocol.Reply {
	args := make([][]byte, len(cmd))
	for i, c := range cmd {
		args[i] = []byte(c)
	}
	return v.e.Execute(v.sess, args)
}

// is runs a command and checks its rendered reply.
func (v *env) is(t *testing.T, want string, cmd ...string) {
	t.Helper()
	if got := v.run(cmd...).String(); got != want {
		t.Fatalf("%v: got %s, want %s", cmd, got, want)
	}
}

func (v *env) logged(t *testing.T, want ...string) {
	t.Helper()
	got := v.aof.take()
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("AOF records:\n got  %q\n want %q", got, want)
	}
}

func TestPingEcho(t *testing.T) {
	v := newEnv(t, Options{})
	v.is(t, "+PONG", "PING")
	v.is(t, `"hi"`, "ping", "hi")
	v.is(t, "-ERR wrong number of arguments for 'ping' command", "PING", "a", "b")
	v.is(t, `"x y"`, "ECHO", "x y")
	v.is(t, "-ERR wrong number of arguments for 'echo' command", "ECHO")
	v.logged(t)
}

func TestSetGetBasics(t *testing.T) {
	v := newEnv(t, Options{})
	v.is(t, "(nil)", "GET", "k")
	v.is(t, "+OK", "SET", "k", "v")
	v.is(t, `"v"`, "GET", "k")
	v.is(t, "+OK", "set", "k", "")
	v.is(t, `""`, "get", "k")
	v.logged(t, "SET k v", "SET k ")
	v.is(t, "-ERR wrong number of arguments for 'set' command", "SET", "k")
	v.is(t, "-ERR wrong number of arguments for 'get' command", "GET", "a", "b")
}

func TestSetExpiryOptions(t *testing.T) {
	v := newEnv(t, Options{})
	v.is(t, "+OK", "SET", "a", "1", "EX", "10")
	v.is(t, ":10", "TTL", "a")
	v.is(t, ":10000", "PTTL", "a")
	v.is(t, "+OK", "SET", "b", "1", "px", "1500")
	v.is(t, ":1500", "PTTL", "b")
	v.is(t, "+OK", "SET", "c", "1", "EXAT", "1700000100")
	v.is(t, ":100", "TTL", "c")
	v.is(t, "+OK", "SET", "d", "1", "PXAT", "1700000000250")
	v.is(t, ":250", "PTTL", "d")
	// The AOF always gets absolute times, whatever form the client used.
	v.logged(t,
		"SET a 1 PXAT 1700000010000",
		"SET b 1 PXAT 1700000001500",
		"SET c 1 PXAT 1700000100000",
		"SET d 1 PXAT 1700000000250",
	)

	v.clock.Advance(1500 * time.Millisecond)
	v.is(t, `"1"`, "GET", "b") // exactly at expiry: still alive (expires once strictly past)
	v.clock.Advance(time.Millisecond)
	v.is(t, "(nil)", "GET", "b")
	v.is(t, `"1"`, "GET", "a")
	v.is(t, ":-2", "TTL", "b")
}

func TestSetKeepTTLAndPlainSetClearsTTL(t *testing.T) {
	v := newEnv(t, Options{})
	v.run("SET", "k", "v", "EX", "100")
	v.is(t, "+OK", "SET", "k", "v2", "KEEPTTL")
	v.is(t, ":100", "TTL", "k")
	v.is(t, "+OK", "SET", "k", "v3")
	v.is(t, ":-1", "TTL", "k")
	v.is(t, "+OK", "SET", "new", "v", "KEEPTTL") // no previous TTL: none kept
	v.is(t, ":-1", "TTL", "new")
	v.logged(t, "SET k v PXAT 1700000100000", "SET k v2 PXAT 1700000100000", "SET k v3", "SET new v")
}

func TestSetNXXXGet(t *testing.T) {
	v := newEnv(t, Options{})
	v.is(t, "(nil)", "SET", "k", "1", "XX")
	v.is(t, "(nil)", "GET", "k")
	v.is(t, "+OK", "SET", "k", "1", "NX")
	v.is(t, "(nil)", "SET", "k", "2", "NX")
	v.is(t, `"1"`, "GET", "k")
	v.is(t, "+OK", "SET", "k", "3", "XX")
	v.is(t, `"3"`, "SET", "k", "4", "GET")
	v.is(t, `"4"`, "SET", "k", "5", "NX", "GET") // NX fails: returns old value, no change
	v.is(t, `"4"`, "GET", "k")
	v.is(t, "(nil)", "SET", "fresh", "x", "GET")
	// Only successful SETs are logged.
	v.logged(t, "SET k 1", "SET k 3", "SET k 4", "SET fresh x")
}

func TestSetOptionErrors(t *testing.T) {
	v := newEnv(t, Options{})
	cases := map[string][]string{
		"-ERR syntax error":                             {"SET", "k", "v", "NX", "XX"},
		"-ERR syntax error ":                            {"SET", "k", "v", "EX", "1", "PX", "1"},
		"-ERR syntax error  ":                           {"SET", "k", "v", "EX"},
		"-ERR syntax error   ":                          {"SET", "k", "v", "BOGUS"},
		"-ERR syntax error    ":                         {"SET", "k", "v", "KEEPTTL", "EX", "5"},
		"-ERR value is not an integer or out of range":  {"SET", "k", "v", "EX", "ten"},
		"-ERR invalid expire time in 'set' command":     {"SET", "k", "v", "EX", "0"},
		"-ERR invalid expire time in 'set' command ":    {"SET", "k", "v", "PX", "-5"},
		"-ERR invalid expire time in 'set' command  ":   {"SET", "k", "v", "EX", "9223372036854775807"},
		"-ERR invalid expire time in 'set' command   ":  {"SET", "k", "v", "PX", "9223372036854775807"},
		"-ERR value is not an integer or out of range ": {"SET", "k", "v", "PX", "99999999999999999999"},
	}
	for want, cmd := range cases {
		v.is(t, strings.TrimRight(want, " "), cmd...)
	}
	v.is(t, "(nil)", "GET", "k")
	v.logged(t)
}

func TestSetWithPastAbsoluteTimeDeletes(t *testing.T) {
	v := newEnv(t, Options{})
	v.run("SET", "k", "old")
	v.is(t, "+OK", "SET", "k", "new", "PXAT", "1000")
	v.is(t, "(nil)", "GET", "k")
	v.is(t, ":0", "DBSIZE")
}

func TestDelExists(t *testing.T) {
	v := newEnv(t, Options{})
	v.run("SET", "a", "1")
	v.run("SET", "b", "1")
	v.aof.take()
	v.is(t, ":3", "EXISTS", "a", "b", "a", "missing")
	v.is(t, ":2", "DEL", "a", "b", "missing")
	v.is(t, ":0", "DEL", "a")
	v.is(t, ":0", "EXISTS", "a")
	v.logged(t, "DEL a b") // only keys actually deleted, and nothing for a no-op DEL
}

func TestExpireFamily(t *testing.T) {
	v := newEnv(t, Options{})
	v.is(t, ":0", "EXPIRE", "missing", "10")
	v.run("SET", "k", "v")
	v.aof.take()

	v.is(t, ":1", "EXPIRE", "k", "10")
	v.is(t, ":10", "TTL", "k")
	v.is(t, ":1", "PEXPIRE", "k", "2500")
	v.is(t, ":2500", "PTTL", "k")
	v.is(t, ":3", "TTL", "k") // 2.5s rounds to 3, as in Redis
	v.is(t, ":1", "EXPIREAT", "k", "1700000050")
	v.is(t, ":50", "TTL", "k")
	v.is(t, ":1", "PEXPIREAT", "k", "1700000060000")
	v.is(t, ":60", "TTL", "k")
	v.logged(t,
		"PEXPIREAT k 1700000010000",
		"PEXPIREAT k 1700000002500",
		"PEXPIREAT k 1700000050000",
		"PEXPIREAT k 1700000060000",
	)

	// A non-positive relative TTL deletes the key, and is logged as DEL.
	v.is(t, ":1", "EXPIRE", "k", "-1")
	v.is(t, ":-2", "TTL", "k")
	v.logged(t, "DEL k")

	v.is(t, "-ERR value is not an integer or out of range", "EXPIRE", "k", "abc")
	v.is(t, "-ERR invalid expire time in 'expire' command", "EXPIRE", "k", "9223372036854775807")
	v.is(t, "-ERR invalid expire time in 'pexpire' command", "PEXPIRE", "k", "9223372036854775807")
}

func TestExpireOptions(t *testing.T) {
	v := newEnv(t, Options{})
	v.run("SET", "k", "v")
	v.is(t, ":0", "EXPIRE", "k", "100", "XX") // no TTL yet
	v.is(t, ":0", "EXPIRE", "k", "100", "GT") // no TTL counts as infinite
	v.is(t, ":1", "EXPIRE", "k", "100", "NX")
	v.is(t, ":0", "EXPIRE", "k", "200", "NX")
	v.is(t, ":0", "EXPIRE", "k", "50", "GT")
	v.is(t, ":1", "EXPIRE", "k", "200", "GT")
	v.is(t, ":0", "EXPIRE", "k", "300", "LT")
	v.is(t, ":1", "EXPIRE", "k", "150", "LT")
	v.is(t, ":1", "EXPIRE", "k", "120", "XX")
	v.is(t, ":120", "TTL", "k")
	v.run("PERSIST", "k")
	v.is(t, ":1", "EXPIRE", "k", "100", "LT") // infinite > 100
	v.is(t, "-ERR NX and XX, GT or LT options at the same time are not compatible", "EXPIRE", "k", "1", "NX", "GT")
	v.is(t, "-ERR GT and LT options at the same time are not compatible", "EXPIRE", "k", "1", "GT", "LT")
	v.is(t, "-ERR Unsupported option FOO", "EXPIRE", "k", "1", "FOO")
}

func TestTTLAndPersist(t *testing.T) {
	v := newEnv(t, Options{})
	v.is(t, ":-2", "TTL", "nope")
	v.is(t, ":-2", "PTTL", "nope")
	v.run("SET", "k", "v")
	v.is(t, ":-1", "TTL", "k")
	v.is(t, ":0", "PERSIST", "k")
	v.is(t, ":0", "PERSIST", "nope")
	v.run("SET", "k", "v", "PX", "999")
	v.is(t, ":1", "TTL", "k") // 999ms rounds to 1s
	v.clock.Advance(600 * time.Millisecond)
	v.is(t, ":0", "TTL", "k") // 399ms rounds to 0
	v.is(t, ":399", "PTTL", "k")
	v.aof.take()
	v.is(t, ":1", "PERSIST", "k")
	v.is(t, ":-1", "TTL", "k")
	v.logged(t, "PERSIST k")
	v.clock.Advance(time.Hour)
	v.is(t, `"v"`, "GET", "k")
}

func TestDBSizeExcludesExpired(t *testing.T) {
	v := newEnv(t, Options{})
	v.run("SET", "a", "1")
	v.run("SET", "b", "1", "PX", "100")
	v.run("SET", "c", "1", "PX", "200")
	v.is(t, ":3", "DBSIZE")
	v.clock.Advance(150 * time.Millisecond)
	v.is(t, ":2", "DBSIZE")
	v.is(t, ":1", "EXISTS", "a", "b")
	v.clock.Advance(time.Second)
	v.is(t, ":1", "DBSIZE")
}

func TestFlushDB(t *testing.T) {
	v := newEnv(t, Options{})
	v.run("SET", "a", "1")
	v.run("SET", "b", "1")
	v.aof.take()
	v.is(t, "+OK", "FLUSHDB")
	v.is(t, ":0", "DBSIZE")
	v.is(t, "+OK", "FLUSHDB", "ASYNC")
	v.is(t, "+OK", "flushdb", "sync")
	v.is(t, "-ERR syntax error", "FLUSHDB", "NOW")
	v.logged(t, "FLUSHDB", "FLUSHDB", "FLUSHDB")
}

func TestDisabledCommands(t *testing.T) {
	v := newEnv(t, Options{DisabledCommands: []string{"FLUSHDB", " bgrewriteaof "}})
	v.run("SET", "a", "1")
	v.is(t, "-ERR command 'FLUSHDB' is disabled by server configuration", "FLUSHDB")
	v.is(t, "-ERR command 'BGREWRITEAOF' is disabled by server configuration", "BGREWRITEAOF")
	v.is(t, ":1", "DBSIZE")
}

func TestAuth(t *testing.T) {
	v := newEnv(t, Options{Password: "s3cret"})
	v.is(t, "-NOAUTH Authentication required.", "GET", "k")
	v.is(t, "-NOAUTH Authentication required.", "PING")
	v.is(t, "-NOAUTH Authentication required.", "NOSUCHCMD") // don't reveal the command table
	v.is(t, "-WRONGPASS invalid username-password pair or user is disabled.", "AUTH", "nope")
	v.is(t, "-WRONGPASS invalid username-password pair or user is disabled.", "AUTH", "admin", "s3cret")
	v.is(t, "-NOAUTH Authentication required.", "GET", "k")
	v.is(t, "+OK", "AUTH", "s3cret")
	v.is(t, "(nil)", "GET", "k")
	v.is(t, "+OK", "AUTH", "default", "s3cret")

	// Sessions are independent.
	other := NewSession()
	if got := v.e.Execute(other, [][]byte{[]byte("GET"), []byte("k")}); got.String() != "-NOAUTH Authentication required." {
		t.Fatalf("second session not isolated: %s", got)
	}

	noPw := newEnv(t, Options{})
	got := noPw.run("AUTH", "x")
	if !got.IsError() || !strings.Contains(got.Str, "without any password configured") {
		t.Fatalf("AUTH without password configured: %s", got)
	}
}

func TestQuitAndSelect(t *testing.T) {
	v := newEnv(t, Options{})
	v.is(t, "+OK", "SELECT", "0")
	v.is(t, "-ERR DB index is out of range", "SELECT", "1")
	v.is(t, "-ERR value is not an integer or out of range", "SELECT", "x")
	if v.sess.Quit {
		t.Fatal("Quit set before QUIT")
	}
	v.is(t, "+OK", "QUIT")
	if !v.sess.Quit {
		t.Fatal("QUIT did not mark the session")
	}
}

func TestMaxMemory(t *testing.T) {
	v := newEnv(t, Options{MaxMemory: 1000})
	big := strings.Repeat("x", 600)
	v.is(t, "+OK", "SET", "a", big)
	v.is(t, "+OK", "SET", "b", big) // usage was under the limit when checked
	v.is(t, "-OOM command not allowed when used memory > 'maxmemory'.", "SET", "c", "1")
	v.is(t, ":1", "DEL", "a") // deletes are always allowed and free memory
	v.is(t, "+OK", "SET", "c", "1")
}

func TestPersistenceFailureHandling(t *testing.T) {
	aof := &fakeAOF{}
	v := newEnv(t, Options{AOF: aof})

	aof.err = errors.New("no space left on device")
	v.is(t, "-MISCONF Errors writing to the AOF file: no space left on device", "SET", "k", "v")
	v.is(t, "(nil)", "GET", "k") // refused before applying anything
	v.is(t, "+PONG", "PING")     // reads keep working
	aof.err = nil

	aof.waitErr = errors.New("disk on fire")
	got := v.run("SET", "k", "v")
	if !got.IsError() || !strings.Contains(got.Str, "applied in memory but is not durable") {
		t.Fatalf("got %s", got)
	}
	v.is(t, `"v"`, "GET", "k") // honest: the change did happen in memory
}

func TestBgRewriteAOF(t *testing.T) {
	v := newEnv(t, Options{})
	v.is(t, "+Background append only file rewriting started", "BGREWRITEAOF")
	if v.aof.rewrites != 1 {
		t.Fatalf("rewrites = %d", v.aof.rewrites)
	}
	noAOF := New(store.New(nil), Options{})
	if got := noAOF.Execute(NewSession(), [][]byte{[]byte("BGREWRITEAOF")}); !got.IsError() {
		t.Fatalf("BGREWRITEAOF without AOF: %s", got)
	}
}

func TestInfo(t *testing.T) {
	v := newEnv(t, Options{Version: "1.2.3", MaxMemory: 2048, InfoSections: map[string]func() []InfoField{
		"clients": func() []InfoField { return []InfoField{{"connected_clients", "7"}} },
	}})
	v.run("SET", "a", "1")
	all := string(v.run("INFO").Bulk)
	for _, want := range []string{
		"# Server\r\n", "goredis_version:1.2.3\r\n", "# Clients\r\n", "connected_clients:7\r\n",
		"# Memory\r\n", "maxmemory:2048\r\n", "# Stats\r\n", "total_commands_processed:",
		"# Keyspace\r\n", "db0:keys=1\r\n",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("INFO missing %q:\n%s", want, all)
		}
	}
	one := string(v.run("INFO", "keyspace").Bulk)
	if strings.Contains(one, "# Server") || !strings.Contains(one, "db0:keys=1") {
		t.Fatalf("INFO keyspace:\n%s", one)
	}
}

func TestCommandIntrospection(t *testing.T) {
	v := newEnv(t, Options{})
	count := v.run("COMMAND", "COUNT")
	all := v.run("COMMAND")
	if count.Int != int64(len(all.Array)) || count.Int < 20 {
		t.Fatalf("COMMAND COUNT %d, COMMAND has %d entries", count.Int, len(all.Array))
	}
	v.is(t, `[["get", :2, [+readonly, +fast], :1, :1, :1], (nil array)]`, "COMMAND", "INFO", "get", "nope")
	v.is(t, "[]", "COMMAND", "DOCS")
	if got := v.run("COMMAND", "LIST"); len(got.Array) != len(all.Array) {
		t.Fatalf("COMMAND LIST has %d entries", len(got.Array))
	}
}

func TestReplay(t *testing.T) {
	v := newEnv(t, Options{Password: "pw", DisabledCommands: []string{"FLUSHDB"}, MaxMemory: 1})
	replay := func(cmd ...string) error {
		args := make([][]byte, len(cmd))
		for i, c := range cmd {
			args[i] = []byte(c)
		}
		return v.e.Replay(args)
	}
	// Replay bypasses auth, disabled commands and maxmemory, and logs nothing.
	for _, rec := range [][]string{
		{"SET", "a", "1"},
		{"SET", "b", "2", "PXAT", "1700000100000"},
		{"FLUSHDB"},
		{"SET", "c", "3"},
		{"PEXPIREAT", "c", "1700000200000"},
		{"PERSIST", "c"},
		{"SET", "d", "4"},
		{"DEL", "d"},
	} {
		if err := replay(rec...); err != nil {
			t.Fatalf("replay %v: %v", rec, err)
		}
	}
	v.logged(t)
	v.is(t, "+OK", "AUTH", "pw")
	v.is(t, ":1", "DBSIZE")
	v.is(t, ":-1", "TTL", "c")

	for _, bad := range [][]string{{"GET", "a"}, {"EXPIRE", "a", "10"}, {"NOPE"}, {"SET", "a"}, {"SET", "a", "1", "PXAT", "x"}} {
		if err := replay(bad...); err == nil {
			t.Errorf("replay %v: accepted, want error", bad)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	v := newEnv(t, Options{})
	got := v.run("NOPE", "a", "b")
	if !got.IsError() || !strings.HasPrefix(got.Str, "ERR unknown command 'NOPE'") || !strings.Contains(got.Str, "'a' 'b'") {
		t.Fatalf("got %s", got)
	}
	if long := v.run(strings.Repeat("x", 10000)); len(long.Str) > 200 {
		t.Fatalf("unknown-command error echoes %d bytes; want it truncated", len(long.Str))
	}
}

type recorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *recorder) ObserveCommand(name string, _ time.Duration, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := name
	if failed {
		s += "!"
	}
	r.seen = append(r.seen, s)
}

func TestRecorderLabelsAreBounded(t *testing.T) {
	rec := &recorder{}
	v := newEnv(t, Options{Recorder: rec})
	v.run("SET", "k", "v")
	v.run("GET", "k")
	v.run("GET")
	v.run("random-client-input-123")
	want := "set get get! unknown!"
	if got := strings.Join(rec.seen, " "); got != want {
		t.Fatalf("recorded %q, want %q", got, want)
	}
}
