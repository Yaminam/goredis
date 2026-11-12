package persistence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"goredis/internal/command"
	"goredis/internal/protocol"
	"goredis/internal/store"
)

type fakeClock struct{ ms atomic.Int64 }

func (c *fakeClock) Now() time.Time { return time.UnixMilli(c.ms.Load()) }

// instance is one "process lifetime": a store, an engine and an open AOF.
type instance struct {
	st   *store.Store
	eng  *command.Engine
	aof  *AOF
	load LoadResult
	sess *command.Session
}

func open(t *testing.T, path string, clock *fakeClock, mod func(*Options)) *instance {
	t.Helper()
	inst, err := tryOpen(path, clock, mod)
	if err != nil {
		t.Fatalf("open AOF: %v", err)
	}
	return inst
}

func tryOpen(path string, clock *fakeClock, mod func(*Options)) (*instance, error) {
	var now store.Clock
	if clock != nil {
		now = clock.Now
	}
	st := store.New(now)
	replayEng := command.New(st, command.Options{})
	opts := Options{Path: path, Fsync: FsyncAlways, LoadTruncated: true, RetryInterval: 20 * time.Millisecond}
	if mod != nil {
		mod(&opts)
	}
	st.SetLoading(true)
	aof, res, err := Open(opts, replayEng.Replay)
	st.SetLoading(false)
	if err != nil {
		return nil, err
	}
	aof.SetSnapshotFunc(func(begin func()) []store.Item {
		var items []store.Item
		st.View(func(tx *store.ReadTx) {
			items = tx.Snapshot()
			begin()
		})
		return items
	})
	return &instance{
		st:   st,
		eng:  command.New(st, command.Options{AOF: aof}),
		aof:  aof,
		load: res,
		sess: command.NewSession(),
	}, nil
}

func (in *instance) do(cmd ...string) string {
	args := make([][]byte, len(cmd))
	for i, c := range cmd {
		args[i] = []byte(c)
	}
	return in.eng.Execute(in.sess, args).String()
}

func (in *instance) close(t *testing.T) {
	t.Helper()
	if err := in.aof.Close(); err != nil {
		t.Fatalf("close AOF: %v", err)
	}
}

func expect(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestRestartRecovery(t *testing.T) {
	for _, policy := range []FsyncPolicy{FsyncAlways, FsyncEverySec, FsyncNo} {
		t.Run(policy.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "appendonly.aof")
			a := open(t, path, nil, func(o *Options) { o.Fsync = policy })
			a.do("SET", "keep", "1")
			a.do("SET", "gone", "2")
			a.do("DEL", "gone")
			a.do("SET", "binary", "a\r\nb\x00c")
			a.do("SET", "counter", "1")
			a.do("SET", "counter", "2")
			a.do("SET", "nx", "first", "NX")
			a.do("SET", "nx", "second", "NX") // not applied, must not be logged
			a.close(t)

			b := open(t, path, nil, nil)
			defer b.close(t)
			expect(t, b.do("GET", "keep"), `"1"`)
			expect(t, b.do("GET", "gone"), "(nil)")
			expect(t, b.do("GET", "binary"), fmt.Sprintf("%q", "a\r\nb\x00c"))
			expect(t, b.do("GET", "counter"), `"2"`)
			expect(t, b.do("GET", "nx"), `"first"`)
			expect(t, b.do("DBSIZE"), ":4")
			if b.load.Commands != 7 { // the failed SET NX is not logged
				t.Fatalf("replayed %d records, want 7", b.load.Commands)
			}
		})
	}
}

// Relative TTLs are logged as absolute times, so a key set with EX 10
// expires 10 s after it was set, no matter how long the server was down.
func TestExpirationAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appendonly.aof")
	clock := &fakeClock{}
	clock.ms.Store(1_700_000_000_000)

	a := open(t, path, clock, nil)
	a.do("SET", "short", "v", "EX", "10")
	a.do("SET", "long", "v", "EX", "1000")
	a.do("SET", "persisted", "v", "EX", "10")
	a.do("PERSIST", "persisted")
	a.do("SET", "pexp", "v")
	a.do("PEXPIRE", "pexp", "20000")
	a.close(t)

	clock.ms.Add(15_000) // "downtime" of 15 s
	b := open(t, path, clock, nil)
	defer b.close(t)
	expect(t, b.do("GET", "short"), "(nil)")
	expect(t, b.do("TTL", "long"), ":985")
	expect(t, b.do("TTL", "persisted"), ":-1")
	expect(t, b.do("PTTL", "pexp"), ":5000")
	expect(t, b.do("DBSIZE"), ":3")
}

func TestFlushDBSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appendonly.aof")
	a := open(t, path, nil, nil)
	a.do("SET", "a", "1")
	a.do("SET", "b", "2")
	a.do("FLUSHDB")
	a.do("SET", "c", "3")
	a.close(t)

	b := open(t, path, nil, nil)
	defer b.close(t)
	expect(t, b.do("DBSIZE"), ":1")
	expect(t, b.do("GET", "c"), `"3"`)
}

// Cut a valid AOF at every possible byte offset. Each prefix must either
// load cleanly (cut at a record boundary) or be truncated back to the last
// complete record, and the data must match exactly the complete records.
func TestTruncatedFileAtEveryOffset(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "full.aof")
	a := open(t, full, nil, nil)
	a.do("SET", "k1", "v1")
	a.do("SET", "k2", "value-two")
	a.do("DEL", "k1")
	a.close(t)
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	// Record boundaries, found by encoding the same records.
	b1 := len(protocol.AppendCommand(nil, []byte("SET"), []byte("k1"), []byte("v1")))
	b2 := b1 + len(protocol.AppendCommand(nil, []byte("SET"), []byte("k2"), []byte("value-two")))

	for cut := 0; cut <= len(data); cut++ {
		path := filepath.Join(dir, fmt.Sprintf("cut%d.aof", cut))
		if err := os.WriteFile(path, data[:cut], 0o600); err != nil {
			t.Fatal(err)
		}
		in := open(t, path, nil, nil)
		var want string
		switch {
		case cut < b1:
			want = ":0"
		case cut < b2:
			want = ":1" // k1 only
		case cut < len(data):
			want = ":2" // k1 and k2; DEL incomplete
		default:
			want = ":1" // k2 only
		}
		if got := in.do("DBSIZE"); got != want {
			t.Fatalf("cut at %d: DBSIZE %s, want %s", cut, got, want)
		}
		boundary := cut == 0 || cut == b1 || cut == b2 || cut == len(data)
		if boundary != (in.load.TruncatedBytes == 0) {
			t.Fatalf("cut at %d: truncated %d bytes", cut, in.load.TruncatedBytes)
		}
		// New writes after a truncation must land on a clean boundary.
		in.do("SET", "after", "x")
		in.close(t)
		again := open(t, path, nil, nil)
		if got := again.do("GET", "after"); got != `"x"` {
			t.Fatalf("cut at %d: write after truncation lost: %s", cut, got)
		}
		again.close(t)
	}
}

func TestTruncatedFileRefusedWhenNotAllowed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appendonly.aof")
	content := []byte("*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$5\r\nhel")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := tryOpen(path, nil, func(o *Options) { o.LoadTruncated = false })
	if err == nil || !strings.Contains(err.Error(), "incomplete record") {
		t.Fatalf("got %v, want incomplete-record error", err)
	}
	// The file is untouched.
	if fi, _ := os.Stat(path); fi.Size() != int64(len(content)) {
		t.Fatalf("file modified: size %d", fi.Size())
	}
}

func TestCorruptRecordRefusesStartup(t *testing.T) {
	good := string(protocol.AppendCommand(nil, []byte("SET"), []byte("a"), []byte("1")))
	cases := map[string]string{
		"garbage mid-file":     good + "this is not resp\r\n" + good,
		"bad length mid-file":  good + "*2\r\n$x\r\n" + good,
		"unknown command":      good + string(protocol.AppendCommand(nil, []byte("GET"), []byte("a"))),
		"wrong arity":          string(protocol.AppendCommand(nil, []byte("SET"), []byte("a"))),
		"bad expire timestamp": string(protocol.AppendCommand(nil, []byte("SET"), []byte("a"), []byte("1"), []byte("PXAT"), []byte("soon"))),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "appendonly.aof")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := tryOpen(path, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "refusing to start") {
				t.Fatalf("got %v, want refusal", err)
			}
		})
	}
}

func TestScanReportsOffsets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appendonly.aof")
	good := protocol.AppendCommand(nil, []byte("SET"), []byte("a"), []byte("1"))
	if err := os.WriteFile(path, append(append([]byte{}, good...), "junk\r\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(path, func([][]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.Problem != ProblemCorrupt || res.ValidOffset != int64(len(good)) || res.Records != 1 {
		t.Fatalf("got %+v", res)
	}
}

func TestSecondProcessIsLockedOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appendonly.aof")
	a := open(t, path, nil, nil)
	_, err := tryOpen(path, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "held by another process") {
		t.Fatalf("second open: got %v, want lock error", err)
	}
	a.close(t)
	b := open(t, path, nil, nil) // released on close
	b.close(t)
}

// faultyFile simulates a disk that fills up or fails fsync.
type faultyFile struct {
	mu        sync.Mutex
	f         *os.File
	failWrite bool // ENOSPC after writing half of the data
	failSync  bool
}

func (ff *faultyFile) Write(p []byte) (int, error) {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if ff.failWrite {
		n, _ := ff.f.Write(p[:len(p)/2])
		return n, syscall.ENOSPC
	}
	return ff.f.Write(p)
}
func (ff *faultyFile) Sync() error {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if ff.failSync {
		return syscall.EIO
	}
	return ff.f.Sync()
}
func (ff *faultyFile) Close() error { return ff.f.Close() }
func (ff *faultyFile) set(write, sync bool) {
	ff.mu.Lock()
	ff.failWrite, ff.failSync = write, sync
	ff.mu.Unlock()
}

func faulty(holder **faultyFile) func(*Options) {
	return func(o *Options) {
		o.OpenAppend = func(path string) (File, error) {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return nil, err
			}
			ff := &faultyFile{f: f}
			*holder = ff
			return ff, nil
		}
	}
}

// A full disk: the write that hits it is reported, later writes are refused
// with MISCONF, reads keep working, and once space frees up the background
// retry writes the buffered tail and the file is complete and valid.
func TestDiskFullRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appendonly.aof")
	var ff *faultyFile
	a := open(t, path, nil, faulty(&ff))
	expect(t, a.do("SET", "before", "1"), "+OK")

	ff.set(true, false)
	got := a.do("SET", "during", "2")
	if !strings.Contains(got, "applied in memory but is not durable") || !strings.Contains(got, "no space left") {
		t.Fatalf("write during disk-full: %s", got)
	}
	if got := a.do("SET", "refused", "3"); !strings.HasPrefix(got, "-MISCONF") {
		t.Fatalf("write while unhealthy: %s", got)
	}
	expect(t, a.do("GET", "before"), `"1"`) // reads still work
	if st := a.aof.Stats(); st.Healthy || st.WriteErrors == 0 || st.PendingBytes == 0 {
		t.Fatalf("stats during failure: %+v", st)
	}

	ff.set(false, false) // space freed
	deadline := time.Now().Add(2 * time.Second)
	for a.aof.Err() != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := a.aof.Err(); err != nil {
		t.Fatalf("AOF did not recover: %v", err)
	}
	expect(t, a.do("SET", "after", "4"), "+OK")
	a.close(t)

	b := open(t, path, nil, nil)
	defer b.close(t)
	if b.load.TruncatedBytes != 0 {
		t.Fatalf("recovered file needed truncation (%d bytes): half-written record was not completed", b.load.TruncatedBytes)
	}
	expect(t, b.do("GET", "before"), `"1"`)
	expect(t, b.do("GET", "during"), `"2"`)
	expect(t, b.do("GET", "refused"), "(nil)")
	expect(t, b.do("GET", "after"), `"4"`)
}

// fsync failure is sticky until a rewrite produces a complete, synced file.
func TestFsyncFailureIsStickyUntilRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appendonly.aof")
	var ff *faultyFile
	a := open(t, path, nil, faulty(&ff))
	a.do("SET", "a", "1")

	ff.set(false, true)
	if got := a.do("SET", "b", "2"); !strings.Contains(got, "fsync") {
		t.Fatalf("got %s", got)
	}
	ff.set(false, false) // disk "recovers", but we can't trust it
	time.Sleep(100 * time.Millisecond)
	if got := a.do("SET", "c", "3"); !strings.HasPrefix(got, "-MISCONF") {
		t.Fatalf("fsync error not sticky: %s", got)
	}
	if err := a.aof.Rewrite(); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	expect(t, a.do("SET", "c", "3"), "+OK")
	a.close(t)

	b := open(t, path, nil, nil)
	defer b.close(t)
	expect(t, b.do("DBSIZE"), ":3")
}

func TestRewriteCompactsAndPreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appendonly.aof")
	clock := &fakeClock{}
	clock.ms.Store(1_700_000_000_000)
	a := open(t, path, clock, func(o *Options) { o.Fsync = FsyncEverySec })
	for i := 0; i < 500; i++ {
		a.do("SET", "counter", fmt.Sprint(i))
	}
	a.do("SET", "ttl", "v", "EX", "100")
	a.do("SET", "deleted", "v")
	a.do("DEL", "deleted")
	before := a.aof.Stats().Size

	expect(t, a.do("BGREWRITEAOF"), "+Background append only file rewriting started")
	a.aof.rewriteWG.Wait()
	st := a.aof.Stats()
	if st.RewritesOK != 1 || st.Size >= before/10 {
		t.Fatalf("rewrite: ok=%d size %d -> %d", st.RewritesOK, before, st.Size)
	}
	a.do("SET", "post", "rewrite")
	a.close(t)

	b := open(t, path, clock, nil)
	defer b.close(t)
	expect(t, b.do("GET", "counter"), `"499"`)
	expect(t, b.do("TTL", "ttl"), ":100")
	expect(t, b.do("GET", "deleted"), "(nil)")
	expect(t, b.do("GET", "post"), `"rewrite"`)
	expect(t, b.do("DBSIZE"), ":3")
	if b.load.Commands != 3 {
		t.Fatalf("compacted file has %d records, want 3", b.load.Commands)
	}
}

// Writers keep running while a rewrite is in progress; nothing they write
// may be lost, and the final file must reproduce the exact final state.
func TestRewriteUnderConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appendonly.aof")
	a := open(t, path, nil, func(o *Options) { o.Fsync = FsyncNo })
	for i := 0; i < 2000; i++ {
		a.do("SET", fmt.Sprint("seed", i), "v")
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var writes atomic.Int64
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess := command.NewSession()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				key := []byte(fmt.Sprintf("w%d:%d", w, i%50))
				a.eng.Execute(sess, [][]byte{[]byte("SET"), key, []byte(fmt.Sprint(i))})
				if i%7 == 0 {
					a.eng.Execute(sess, [][]byte{[]byte("DEL"), key})
				}
				writes.Add(1)
			}
		}()
	}
	for r := 0; r < 3; r++ {
		time.Sleep(20 * time.Millisecond)
		if err := a.aof.Rewrite(); err != nil {
			t.Fatalf("rewrite %d: %v", r, err)
		}
	}
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()

	var want []store.Item
	a.st.View(func(tx *store.ReadTx) { want = tx.Snapshot() })
	a.close(t)

	b := open(t, path, nil, nil)
	defer b.close(t)
	var got []store.Item
	b.st.View(func(tx *store.ReadTx) { got = tx.Snapshot() })
	if len(got) != len(want) {
		t.Fatalf("after restart %d keys, before %d (writes during test: %d)", len(got), len(want), writes.Load())
	}
	idx := make(map[string]string, len(got))
	for _, it := range got {
		idx[it.Key] = string(it.Value)
	}
	for _, it := range want {
		if idx[it.Key] != string(it.Value) {
			t.Fatalf("key %s: restart %q, before %q", it.Key, idx[it.Key], it.Value)
		}
	}
}

func TestParseFsyncPolicy(t *testing.T) {
	for _, s := range []string{"always", "everysec", "no"} {
		p, err := ParseFsyncPolicy(s)
		if err != nil || p.String() != s {
			t.Fatalf("%s: %v %v", s, p, err)
		}
	}
	if _, err := ParseFsyncPolicy("sometimes"); err == nil {
		t.Fatal("accepted invalid policy")
	}
}

func TestRewriteAlreadyRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appendonly.aof")
	a := open(t, path, nil, nil)
	defer a.close(t)
	block := make(chan struct{})
	a.aof.SetSnapshotFunc(func(begin func()) []store.Item { <-block; begin(); return nil })
	if err := a.aof.StartRewrite(); err != nil {
		t.Fatal(err)
	}
	if err := a.aof.StartRewrite(); !errors.Is(err, ErrRewriteInProgress) {
		t.Fatalf("second rewrite: %v", err)
	}
	close(block)
	a.aof.rewriteWG.Wait()
}

// slowSyncFile makes fsync slow and counts calls, so the test can check that
// concurrent writers share fsyncs (group commit) instead of queuing one each.
type slowSyncFile struct {
	f     *os.File
	syncs atomic.Int64
}

func (s *slowSyncFile) Write(p []byte) (int, error) { return s.f.Write(p) }
func (s *slowSyncFile) Close() error                { return s.f.Close() }
func (s *slowSyncFile) Sync() error {
	s.syncs.Add(1)
	time.Sleep(2 * time.Millisecond)
	return s.f.Sync()
}

func TestGroupCommitSharesFsyncs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appendonly.aof")
	var file *slowSyncFile
	a := open(t, path, nil, func(o *Options) {
		o.Fsync = FsyncAlways
		o.OpenAppend = func(p string) (File, error) {
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			file = &slowSyncFile{f: f}
			return file, err
		}
	})
	const writers, each = 32, 20
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess := command.NewSession()
			for i := 0; i < each; i++ {
				rep := a.eng.Execute(sess, [][]byte{[]byte("SET"), []byte(fmt.Sprintf("k%d:%d", w, i)), []byte("v")})
				if rep.IsError() {
					t.Error(rep.Str)
					return
				}
			}
		}()
	}
	wg.Wait()
	writes, syncs := int64(writers*each), file.syncs.Load()
	t.Logf("%d acknowledged writes, %d fsyncs", writes, syncs)
	if syncs > writes/4 {
		t.Fatalf("%d fsyncs for %d writes: group commit is not batching", syncs, writes)
	}
	a.close(t)
	b := open(t, path, nil, nil)
	defer b.close(t)
	expect(t, b.do("DBSIZE"), fmt.Sprintf(":%d", writes))
}
