// Package persistence implements the append-only file (AOF): every write
// command is recorded as a RESP array, and the file is replayed at startup to
// rebuild the dataset.
//
// # Ordering
//
// Append is called while the store's write lock is held, so records enter the
// in-memory buffer in exactly the order changes were applied. Append never
// touches the disk; it only copies bytes into a buffer.
//
// # Group commit
//
// After releasing the store lock, a client calls Wait(seq). The first waiter
// to take writeMu writes the whole pending buffer (its own record and everyone
// else's) in one write(2) call, and fsyncs if the policy is "always". The
// other waiters find their record already covered and return. Many concurrent
// clients therefore share one syscall.
//
// # Durability guarantees (what an acknowledged write means)
//
//	always:   written and fsynced before the reply. Survives a process crash
//	          and an OS crash or power loss (if the disk honours fsync).
//	everysec: written to the OS before the reply; fsynced once per second.
//	          Survives a process crash. An OS crash or power loss can lose
//	          about the last second of acknowledged writes.
//	no:       written to the OS before the reply; the OS decides when to
//	          flush (typically within 30 s on Linux). Survives a process
//	          crash only.
//
// # Failures
//
// A failed write keeps the unwritten bytes in memory and marks the AOF
// unhealthy; write commands are refused with MISCONF while it is unhealthy,
// and a background loop retries every second. Once the retry succeeds the AOF
// is healthy again. A failed fsync is sticky: after fsync reports an error,
// the kernel may already have dropped the dirty pages, so retrying fsync
// proves nothing. Health returns only after BGREWRITEAOF writes a complete,
// fsynced new file from memory (or after a restart).
package persistence

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"goredis/internal/protocol"
	"goredis/internal/store"
)

// FsyncPolicy controls when appended data is fsynced.
type FsyncPolicy int

const (
	FsyncEverySec FsyncPolicy = iota
	FsyncAlways
	FsyncNo
)

func (p FsyncPolicy) String() string {
	switch p {
	case FsyncAlways:
		return "always"
	case FsyncNo:
		return "no"
	}
	return "everysec"
}

// ParseFsyncPolicy parses "always", "everysec" or "no".
func ParseFsyncPolicy(s string) (FsyncPolicy, error) {
	switch s {
	case "always":
		return FsyncAlways, nil
	case "everysec":
		return FsyncEverySec, nil
	case "no":
		return FsyncNo, nil
	}
	return 0, fmt.Errorf("invalid fsync policy %q (want always, everysec or no)", s)
}

// File is the subset of *os.File the AOF writes through. Tests substitute a
// faulty implementation to simulate a full disk or a failing fsync.
type File interface {
	io.Writer
	Sync() error
	Close() error
}

// Options configures an AOF.
type Options struct {
	Path  string
	Fsync FsyncPolicy
	// LoadTruncated accepts a file whose last record is incomplete (e.g. the
	// process died mid-write) by truncating that record. A malformed record
	// anywhere else always refuses startup.
	LoadTruncated bool
	Logger        *slog.Logger
	// ObserveFsync, if set, receives the duration of every fsync.
	ObserveFsync func(time.Duration)
	// OpenAppend opens the file for appending. Tests override it.
	OpenAppend func(path string) (File, error)
	// RetryInterval is how often background retries and everysec fsyncs
	// run. Zero means one second.
	RetryInterval time.Duration
}

// SnapshotFunc returns every live key. It must call begin while holding the
// store's read lock, after taking the snapshot, so that no write can slip in
// between the snapshot and the start of rewrite buffering.
type SnapshotFunc func(begin func()) []store.Item

// AOF is an append-only file writer. It implements command.Logger.
type AOF struct {
	opts Options
	log  *slog.Logger
	lock *fileLock

	// writeMu serialises everything that touches the file handle: writes,
	// fsyncs and the swap at the end of a rewrite. Lock order: writeMu,
	// then mu. Store lock (held by Append's caller), then mu.
	writeMu sync.Mutex

	mu         sync.Mutex
	f          File
	buf        []byte // appended but not yet written
	seq        uint64 // last appended record
	writtenSeq uint64 // last record handed to the OS
	syncedSeq  uint64 // last record fsynced
	size       int64
	writeErr   error // transient; cleared by a successful write
	syncErr    error // sticky; cleared only by a successful rewrite
	rewriting  bool
	rewriteBuf []byte
	snapshot   SnapshotFunc
	rewriteErr error // result of the most recent rewrite
	closed     bool

	writeErrors   atomic.Uint64
	fsyncErrors   atomic.Uint64
	rewritesOK    atomic.Uint64
	rewritesFail  atomic.Uint64
	rewriteWG     sync.WaitGroup
	stop          chan struct{}
	loopDone      chan struct{}
	lastRewriteAt atomic.Int64
}

// LoadResult describes what happened when the AOF was replayed at startup.
type LoadResult struct {
	Commands       int
	Bytes          int64
	TruncatedBytes int64
	Duration       time.Duration
}

// Open locks the file, replays it through apply, truncates an incomplete
// final record if allowed, and opens it for appending.
func Open(opts Options, apply func(args [][]byte) error) (*AOF, LoadResult, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.OpenAppend == nil {
		opts.OpenAppend = openAppend
	}
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = time.Second
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o750); err != nil {
		return nil, LoadResult{}, fmt.Errorf("create AOF directory: %w", err)
	}
	lock, err := acquireLock(opts.Path + ".lock")
	if err != nil {
		return nil, LoadResult{}, err
	}
	res, err := load(opts.Path, apply, opts.LoadTruncated, opts.Logger)
	if err != nil {
		lock.release()
		return nil, res, err
	}
	f, err := opts.OpenAppend(opts.Path)
	if err != nil {
		lock.release()
		return nil, res, fmt.Errorf("open AOF for append: %w", err)
	}
	a := &AOF{
		opts:     opts,
		log:      opts.Logger,
		lock:     lock,
		f:        f,
		size:     res.Bytes,
		stop:     make(chan struct{}),
		loopDone: make(chan struct{}),
	}
	go a.loop()
	return a, res, nil
}

func openAppend(path string) (File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// SetSnapshotFunc provides the dataset snapshot used by BGREWRITEAOF.
func (a *AOF) SetSnapshotFunc(fn SnapshotFunc) {
	a.mu.Lock()
	a.snapshot = fn
	a.mu.Unlock()
}

// Append buffers one record. It is called under the store's write lock and
// never blocks on I/O.
func (a *AOF) Append(args ...[]byte) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.buf = protocol.AppendCommand(a.buf, args...)
	if a.rewriting {
		a.rewriteBuf = protocol.AppendCommand(a.rewriteBuf, args...)
	}
	a.seq++
	return a.seq
}

// Wait blocks until record seq is persisted as the fsync policy promises.
func (a *AOF) Wait(seq uint64) error {
	for {
		if a.persisted(seq) {
			return nil
		}
		a.writeMu.Lock()
		// Re-check now that we hold the file: while we waited for writeMu,
		// the previous holder may already have written (and fsynced) our
		// record along with its own. That is the whole point of group
		// commit; without this check every waiter would issue its own
		// write and fsync, one after another.
		if a.persisted(seq) {
			a.writeMu.Unlock()
			return nil
		}
		err := a.flushLocked(a.opts.Fsync == FsyncAlways)
		a.writeMu.Unlock()
		if err != nil {
			return err
		}
	}
}

// persisted reports whether record seq is as durable as the policy promises.
func (a *AOF) persisted(seq uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.opts.Fsync == FsyncAlways {
		return a.syncedSeq >= seq
	}
	return a.writtenSeq >= seq
}

// flushLocked writes the pending buffer and optionally fsyncs. The caller
// holds writeMu.
func (a *AOF) flushLocked(doSync bool) error {
	a.mu.Lock()
	data, upto, f := a.buf, a.seq, a.f
	a.buf = nil
	a.mu.Unlock()

	if len(data) > 0 {
		n, err := f.Write(data)
		a.mu.Lock()
		a.size += int64(n)
		if err != nil {
			// Keep the unwritten tail, ahead of anything appended since.
			rest := make([]byte, 0, len(data)-n+len(a.buf))
			rest = append(rest, data[n:]...)
			a.buf = append(rest, a.buf...)
			if a.writeErr == nil {
				a.log.Error("AOF write failed; write commands are refused until a retry succeeds", "err", err, "pending_bytes", len(a.buf))
			}
			a.writeErr = err
			a.mu.Unlock()
			a.writeErrors.Add(1)
			return fmt.Errorf("write AOF: %w", err)
		}
		if a.writeErr != nil {
			a.log.Info("AOF write recovered")
		}
		a.writeErr = nil
		a.writtenSeq = upto
		a.mu.Unlock()
	} else {
		a.mu.Lock()
		a.writeErr = nil
		a.writtenSeq = max(a.writtenSeq, upto)
		a.mu.Unlock()
	}

	if !doSync {
		return nil
	}
	a.mu.Lock()
	written, synced, syncErr := a.writtenSeq, a.syncedSeq, a.syncErr
	a.mu.Unlock()
	if syncErr != nil {
		return syncErr
	}
	if synced >= written {
		return nil
	}
	start := time.Now()
	err := f.Sync()
	if a.opts.ObserveFsync != nil {
		a.opts.ObserveFsync(time.Since(start))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.fsyncErrors.Add(1)
		a.syncErr = fmt.Errorf("fsync AOF (writes refused until BGREWRITEAOF succeeds or restart): %w", err)
		a.log.Error("AOF fsync failed; durability of recent writes is unknown", "err", err)
		return a.syncErr
	}
	a.syncedSeq = written
	return nil
}

// loop retries failed writes and performs the once-per-second fsync.
func (a *AOF) loop() {
	defer close(a.loopDone)
	t := time.NewTicker(a.opts.RetryInterval)
	defer t.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-t.C:
		}
		a.mu.Lock()
		pending := len(a.buf) > 0
		a.mu.Unlock()
		doSync := a.opts.Fsync == FsyncEverySec
		if !pending && !doSync {
			continue
		}
		a.writeMu.Lock()
		_ = a.flushLocked(doSync) // errors are recorded and logged inside
		a.writeMu.Unlock()
	}
}

// Err returns a non-nil error while persistence is failing.
func (a *AOF) Err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.syncErr != nil {
		return a.syncErr
	}
	return a.writeErr
}

// Stats is a point-in-time view of AOF state for metrics and INFO.
type Stats struct {
	Policy          FsyncPolicy
	Size            int64
	Healthy         bool
	LastError       string
	PendingBytes    int
	WriteErrors     uint64
	FsyncErrors     uint64
	RewritesOK      uint64
	RewritesFailed  uint64
	RewriteInFlight bool
	LastRewrite     time.Time
}

// Stats returns current counters.
func (a *AOF) Stats() Stats {
	a.mu.Lock()
	s := Stats{
		Policy:          a.opts.Fsync,
		Size:            a.size,
		PendingBytes:    len(a.buf),
		RewriteInFlight: a.rewriting,
	}
	err := a.syncErr
	if err == nil {
		err = a.writeErr
	}
	a.mu.Unlock()
	s.Healthy = err == nil
	if err != nil {
		s.LastError = err.Error()
	}
	s.WriteErrors = a.writeErrors.Load()
	s.FsyncErrors = a.fsyncErrors.Load()
	s.RewritesOK = a.rewritesOK.Load()
	s.RewritesFailed = a.rewritesFail.Load()
	if ts := a.lastRewriteAt.Load(); ts != 0 {
		s.LastRewrite = time.UnixMilli(ts)
	}
	return s
}

// ErrRewriteInProgress is returned when BGREWRITEAOF is already running.
var ErrRewriteInProgress = errors.New("Background append only file rewriting already in progress")

// StartRewrite compacts the AOF in the background: it writes the current
// dataset as one SET per key to a temporary file, appends the records that
// arrived meanwhile, fsyncs, and atomically renames it over the old file.
func (a *AOF) StartRewrite() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return errors.New("AOF is closed")
	}
	if a.rewriting {
		a.mu.Unlock()
		return ErrRewriteInProgress
	}
	snap := a.snapshot
	if snap == nil {
		a.mu.Unlock()
		return errors.New("AOF rewrite is not configured")
	}
	a.rewriting = true // reserve; begin() resets the buffer
	a.rewriteBuf = nil
	a.rewriteWG.Add(1)
	a.mu.Unlock()

	go func() {
		defer a.rewriteWG.Done()
		start := time.Now()
		err := a.rewrite(snap)
		a.mu.Lock()
		a.rewriteErr = err
		a.mu.Unlock()
		if err != nil {
			a.rewritesFail.Add(1)
			a.log.Error("AOF rewrite failed", "err", err)
			return
		}
		a.rewritesOK.Add(1)
		a.lastRewriteAt.Store(time.Now().UnixMilli())
		a.log.Info("AOF rewrite complete", "duration", time.Since(start), "size_bytes", a.Stats().Size)
	}()
	return nil
}

// Rewrite runs a rewrite synchronously. It is used by tests and tools.
func (a *AOF) Rewrite() error {
	if err := a.StartRewrite(); err != nil {
		return err
	}
	a.rewriteWG.Wait()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rewriteErr
}

func (a *AOF) rewrite(snap SnapshotFunc) (err error) {
	tmpPath := a.opts.Path + ".rewrite.tmp"
	defer func() {
		if err != nil {
			a.mu.Lock()
			a.rewriting = false
			a.rewriteBuf = nil
			a.mu.Unlock()
			_ = os.Remove(tmpPath)
		}
	}()

	items := snap(func() {
		// Called under the store read lock: no Append can run now.
		a.mu.Lock()
		a.rewriteBuf = nil
		a.mu.Unlock()
	})

	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	closeTmp := true
	defer func() {
		if closeTmp {
			_ = tmp.Close()
		}
	}()
	bw := bufio.NewWriterSize(tmp, 256<<10)
	var rec []byte
	for _, it := range items {
		if it.ExpireAt != 0 {
			rec = protocol.AppendCommand(rec[:0], []byte("SET"), []byte(it.Key), it.Value, []byte("PXAT"), strconv.AppendInt(nil, it.ExpireAt, 10))
		} else {
			rec = protocol.AppendCommand(rec[:0], []byte("SET"), []byte(it.Key), it.Value)
		}
		if _, err := bw.Write(rec); err != nil {
			return fmt.Errorf("write temp file: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync temp file: %w", err)
	}

	// Finalize. Holding mu blocks Append (and so store writers) for the
	// short time it takes to write the records that arrived during the
	// rewrite, fsync, and rename.
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, err := tmp.Write(a.rewriteBuf); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync temp file: %w", err)
	}
	closeTmp = false
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// Windows cannot rename over an open file, so close the old handle
	// first. If the rename fails, reopen the old file and carry on.
	_ = a.f.Close()
	if err := os.Rename(tmpPath, a.opts.Path); err != nil {
		f, openErr := a.opts.OpenAppend(a.opts.Path)
		if openErr != nil {
			a.writeErr = fmt.Errorf("reopen AOF after failed rewrite: %w", openErr)
		} else {
			a.f = f
		}
		return fmt.Errorf("rename rewritten AOF: %w", err)
	}
	syncDir(filepath.Dir(a.opts.Path))
	f, err := a.opts.OpenAppend(a.opts.Path)
	if err != nil {
		a.writeErr = fmt.Errorf("reopen AOF after rewrite: %w", err)
		a.rewriting = false
		a.rewriteBuf = nil
		return a.writeErr
	}
	a.f = f
	if fi, err := os.Stat(a.opts.Path); err == nil {
		a.size = fi.Size()
	}
	// Everything appended so far is in the new, fsynced file.
	a.buf = nil
	a.writtenSeq, a.syncedSeq = a.seq, a.seq
	a.writeErr, a.syncErr = nil, nil
	a.rewriting = false
	a.rewriteBuf = nil
	return nil
}

// syncDir fsyncs a directory so a rename survives a crash. Windows does not
// support fsync on directories; NTFS journals the rename itself.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// Close flushes and fsyncs pending data (regardless of policy), waits for a
// running rewrite, and releases the file and its lock.
func (a *AOF) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	a.mu.Unlock()

	a.rewriteWG.Wait()
	close(a.stop)
	<-a.loopDone

	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	err := a.flushLocked(true)
	if cerr := a.f.Close(); err == nil {
		err = cerr
	}
	a.lock.release()
	return err
}
