// Package store is the in-memory key-value engine. It knows nothing about
// networking, RESP or persistence.
//
// # Concurrency and atomicity
//
// All data is guarded by a single sync.RWMutex. Callers run a whole command
// inside one transaction (View for reads, Update for writes), so every command
// is atomic with respect to every other command: a SET NX cannot interleave
// with another SET on the same key, and DEL k1 k2 removes both keys at one
// point in time. Transactions must stay short and must never do network or
// disk I/O while holding the lock.
//
// A single lock is the simplest correct design. It is a known scalability
// limit on many-core machines with write-heavy workloads; sharding the map is
// the documented next step if benchmarks show lock contention.
//
// # Expiration
//
// Expiry times are absolute Unix milliseconds. A key is expired once the
// current time is past its expiry time (the same rule Redis uses). Expired
// keys are invisible to every read immediately. Their memory is reclaimed by
// (a) write transactions that touch them and (b) a single background sweeper
// goroutine that pops expired entries from a min-heap in small batches.
//
// # Value ownership
//
// Stored value slices are never modified after Set. Readers may therefore
// keep a returned value after the transaction ends (for example while the
// server encodes the reply) without copying it.
package store

import (
	"container/heap"
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// entryOverhead approximates the per-key bookkeeping cost (map bucket share,
// entry struct, slice and string headers). UsedBytes is an estimate meant for
// trend lines and maxmemory checks, not an exact accounting of the Go heap.
const entryOverhead = 96

// Clock returns the current time. Tests inject a fake clock.
type Clock func() time.Time

type entry struct {
	key      string
	value    []byte
	expireAt int64 // Unix ms; 0 means no expiry
	heapIdx  int   // position in the expiry heap, or -1
}

func (e *entry) size() int64 {
	return int64(len(e.key)+len(e.value)) + entryOverhead
}

// Store is a concurrency-safe in-memory key-value map with TTL support.
type Store struct {
	mu        sync.RWMutex
	data      map[string]*entry
	expiry    expiryHeap
	usedBytes int64 // guarded by mu

	clock Clock
	// loading disables expiry checks while the AOF is replayed (see
	// SetLoading).
	loading atomic.Bool

	// Counters are atomic so metrics can read them without taking the lock.
	expired    atomic.Uint64
	deleted    atomic.Uint64
	usedMirror atomic.Int64
}

// New returns an empty store. A nil clock means time.Now.
func New(clock Clock) *Store {
	if clock == nil {
		clock = time.Now
	}
	return &Store{data: make(map[string]*entry), clock: clock}
}

// Value is what a lookup returns.
type Value struct {
	Data     []byte
	ExpireAt int64 // Unix ms, 0 if the key has no expiry
}

// Item is one key in a snapshot.
type Item struct {
	Key      string
	Value    []byte
	ExpireAt int64
}

// Stats is a point-in-time view of store counters.
type Stats struct {
	ExpiredKeys uint64 // keys removed because their TTL passed
	DeletedKeys uint64 // keys removed by DEL, FLUSHDB or a past expiry time
	UsedBytes   int64  // estimated dataset size
}

// Stats returns counters without taking the store lock.
func (s *Store) Stats() Stats {
	return Stats{
		ExpiredKeys: s.expired.Load(),
		DeletedKeys: s.deleted.Load(),
		UsedBytes:   s.usedMirror.Load(),
	}
}

// SetLoading switches replay mode on or off. While loading, transactions run
// with a timestamp before every possible expiry, so no key is treated as
// expired. The append-only file only contains operations that took effect, so
// replaying them without expiry checks rebuilds exactly the state at
// shutdown; expiry is then evaluated against the real clock. Without this, a
// key set with a TTL and later PERSISTed would be dropped by a replay that
// happens after the original TTL has passed.
func (s *Store) SetLoading(on bool) { s.loading.Store(on) }

func (s *Store) now() int64 {
	if s.loading.Load() {
		return math.MinInt64
	}
	return s.clock().UnixMilli()
}

// View runs fn with a read-only transaction under a shared lock.
func (s *Store) View(fn func(tx *ReadTx)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(&ReadTx{s: s, now: s.now()})
}

// Update runs fn with a read-write transaction under the exclusive lock.
func (s *Store) Update(fn func(tx *Tx)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&Tx{ReadTx{s: s, now: s.now()}})
	s.usedMirror.Store(s.usedBytes)
}

// Len returns the number of live (non-expired) keys.
func (s *Store) Len() int {
	n := 0
	s.View(func(tx *ReadTx) { n = tx.Len() })
	return n
}

// ReadTx is a read-only view of the store. Its methods must only be called
// inside the View or Update callback that provided it.
type ReadTx struct {
	s   *Store
	now int64
}

// Now returns the transaction's timestamp in Unix ms. Every method in one
// transaction uses this same instant, so a command sees a consistent clock.
func (tx *ReadTx) Now() int64 { return tx.now }

func (tx *ReadTx) live(key []byte) (*entry, bool) {
	e, ok := tx.s.data[string(key)]
	if !ok || e.expiredAt(tx.now) {
		return nil, false
	}
	return e, true
}

func (e *entry) expiredAt(now int64) bool {
	return e.expireAt != 0 && now > e.expireAt
}

// Get returns the value of a live key.
func (tx *ReadTx) Get(key []byte) (Value, bool) {
	e, ok := tx.live(key)
	if !ok {
		return Value{}, false
	}
	return Value{Data: e.value, ExpireAt: e.expireAt}, true
}

// Exists reports whether key is live.
func (tx *ReadTx) Exists(key []byte) bool {
	_, ok := tx.live(key)
	return ok
}

// Len returns the number of live keys. Expired-but-not-yet-reclaimed keys are
// excluded by walking only the part of the expiry heap that is already due,
// so the cost is proportional to the number of expired keys, not all keys.
func (tx *ReadTx) Len() int {
	return len(tx.s.data) - tx.s.expiry.countDue(tx.now)
}

// Snapshot copies all live keys. Values are shared, not copied (see the
// package documentation on value ownership).
func (tx *ReadTx) Snapshot() []Item {
	items := make([]Item, 0, len(tx.s.data))
	for k, e := range tx.s.data {
		if e.expiredAt(tx.now) {
			continue
		}
		items = append(items, Item{Key: k, Value: e.value, ExpireAt: e.expireAt})
	}
	return items
}

// UsedBytes returns the estimated dataset size.
func (tx *ReadTx) UsedBytes() int64 { return tx.s.usedBytes }

// Tx is a read-write transaction.
type Tx struct {
	ReadTx
}

// reclaim removes key if it exists but is expired, so writes never resurrect
// or extend an expired key.
func (tx *Tx) reclaim(key []byte) {
	if e, ok := tx.s.data[string(key)]; ok && e.expiredAt(tx.now) {
		tx.s.remove(e)
		tx.s.expired.Add(1)
	}
}

// Set stores value under key with an absolute expiry (Unix ms, 0 = none),
// replacing any previous value and TTL. An expiry that is not in the future
// deletes the key instead, because the value would never be visible.
// The store takes ownership of value; callers must not modify it afterwards.
func (tx *Tx) Set(key, value []byte, expireAt int64) {
	tx.reclaim(key)
	s := tx.s
	if expireAt != 0 && expireAt <= tx.now {
		if e, ok := s.data[string(key)]; ok {
			s.remove(e)
			s.deleted.Add(1)
		}
		return
	}
	if e, ok := s.data[string(key)]; ok {
		s.usedBytes -= e.size()
		e.value = value
		s.usedBytes += e.size()
		s.setExpiry(e, expireAt)
		return
	}
	e := &entry{key: string(key), value: value, heapIdx: -1}
	s.data[e.key] = e
	s.usedBytes += e.size()
	s.setExpiry(e, expireAt)
}

// Delete removes a live key and reports whether it existed.
func (tx *Tx) Delete(key []byte) bool {
	tx.reclaim(key)
	e, ok := tx.s.data[string(key)]
	if !ok {
		return false
	}
	tx.s.remove(e)
	tx.s.deleted.Add(1)
	return true
}

// SetExpireAt sets an absolute expiry on a live key. An expiry that is not in
// the future deletes the key (matching Redis). It reports whether the key
// existed.
func (tx *Tx) SetExpireAt(key []byte, expireAt int64) bool {
	tx.reclaim(key)
	e, ok := tx.s.data[string(key)]
	if !ok {
		return false
	}
	if expireAt <= tx.now {
		tx.s.remove(e)
		tx.s.deleted.Add(1)
		return true
	}
	tx.s.setExpiry(e, expireAt)
	return true
}

// Persist removes the expiry of a live key. It reports whether an expiry was
// removed.
func (tx *Tx) Persist(key []byte) bool {
	tx.reclaim(key)
	e, ok := tx.s.data[string(key)]
	if !ok || e.expireAt == 0 {
		return false
	}
	tx.s.setExpiry(e, 0)
	return true
}

// Flush removes every key and returns how many live keys were removed.
func (tx *Tx) Flush() int {
	n := tx.Len()
	s := tx.s
	s.data = make(map[string]*entry)
	s.expiry = nil
	s.usedBytes = 0
	s.deleted.Add(uint64(n))
	return n
}

// remove deletes e from the map and the heap. Caller holds the write lock.
func (s *Store) remove(e *entry) {
	delete(s.data, e.key)
	if e.heapIdx >= 0 {
		heap.Remove(&s.expiry, e.heapIdx)
	}
	s.usedBytes -= e.size()
}

// setExpiry updates e's expiry and its position in the heap.
func (s *Store) setExpiry(e *entry, expireAt int64) {
	e.expireAt = expireAt
	switch {
	case expireAt == 0 && e.heapIdx >= 0:
		heap.Remove(&s.expiry, e.heapIdx)
	case expireAt != 0 && e.heapIdx >= 0:
		heap.Fix(&s.expiry, e.heapIdx)
	case expireAt != 0:
		heap.Push(&s.expiry, e)
	}
}

// DeleteExpired reclaims up to max expired keys and returns how many were
// removed. It holds the write lock only for one batch.
func (s *Store) DeleteExpired(max int) int {
	n := 0
	s.Update(func(tx *Tx) {
		for n < max && len(s.expiry) > 0 && s.expiry[0].expiredAt(tx.now) {
			s.remove(s.expiry[0])
			n++
		}
	})
	s.expired.Add(uint64(n))
	return n
}

// RunExpiry reclaims expired keys every interval until ctx is cancelled. It
// is a single goroutine regardless of key count. Each pass works in batches
// so that a mass expiry never holds the write lock for long.
func (s *Store) RunExpiry(ctx context.Context, interval time.Duration, batch int) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for s.DeleteExpired(batch) == batch {
			// A full batch means more keys may be due; release the lock
			// between batches so client commands can run.
			if ctx.Err() != nil {
				return
			}
		}
	}
}

// expiryHeap is a min-heap of entries ordered by expiry time. Each entry
// records its own index so updates and removals are O(log n).
type expiryHeap []*entry

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].expireAt < h[j].expireAt }
func (h expiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIdx = i
	h[j].heapIdx = j
}

func (h *expiryHeap) Push(x any) {
	e := x.(*entry)
	e.heapIdx = len(*h)
	*h = append(*h, e)
}

func (h *expiryHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.heapIdx = -1
	*h = old[:n-1]
	return e
}

// countDue counts entries whose expiry has passed. Because a parent never
// expires later than its children, the walk stops descending at the first
// entry that is not yet due.
func (h expiryHeap) countDue(now int64) int {
	if len(h) == 0 || !h[0].expiredAt(now) {
		return 0
	}
	count := 0
	stack := []int{0}
	for len(stack) > 0 {
		i := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if i >= len(h) || !h[i].expiredAt(now) {
			continue
		}
		count++
		stack = append(stack, 2*i+1, 2*i+2)
	}
	return count
}
