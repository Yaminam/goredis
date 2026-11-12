package store

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for deterministic TTL tests.
type fakeClock struct{ ms atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.ms.Store(1_700_000_000_000)
	return c
}
func (c *fakeClock) Now() time.Time          { return time.UnixMilli(c.ms.Load()) }
func (c *fakeClock) Advance(d time.Duration) { c.ms.Add(d.Milliseconds()) }
func (c *fakeClock) Ms() int64               { return c.ms.Load() }

func get(s *Store, key string) (string, bool) {
	var v Value
	var ok bool
	s.View(func(tx *ReadTx) { v, ok = tx.Get([]byte(key)) })
	return string(v.Data), ok
}

func set(s *Store, key, value string, expireAt int64) {
	s.Update(func(tx *Tx) { tx.Set([]byte(key), []byte(value), expireAt) })
}

func TestSetGetDelete(t *testing.T) {
	s := New(nil)
	set(s, "a", "1", 0)
	if v, ok := get(s, "a"); !ok || v != "1" {
		t.Fatalf("get a = %q, %v", v, ok)
	}
	set(s, "a", "2", 0)
	if v, _ := get(s, "a"); v != "2" {
		t.Fatalf("overwrite: got %q", v)
	}
	var deleted, again bool
	s.Update(func(tx *Tx) {
		deleted = tx.Delete([]byte("a"))
		again = tx.Delete([]byte("a"))
	})
	if !deleted || again {
		t.Fatalf("delete = %v, second delete = %v", deleted, again)
	}
	if _, ok := get(s, "a"); ok {
		t.Fatal("key still present after delete")
	}
	if got := s.Stats().DeletedKeys; got != 1 {
		t.Fatalf("DeletedKeys = %d, want 1", got)
	}
}

func TestExpiryIsExactAndInvisible(t *testing.T) {
	c := newFakeClock()
	s := New(c.Now)
	set(s, "k", "v", c.Ms()+1000)

	c.Advance(1000 * time.Millisecond) // now == expireAt: still alive (Redis rule: expired when now > expireAt)
	if _, ok := get(s, "k"); !ok {
		t.Fatal("key expired at exactly its expiry time; want alive until strictly after")
	}
	c.Advance(time.Millisecond)
	if _, ok := get(s, "k"); ok {
		t.Fatal("expired key still readable")
	}
	if n := s.Len(); n != 0 {
		t.Fatalf("Len = %d, want 0 (expired keys must not be counted)", n)
	}
	var exists bool
	s.View(func(tx *ReadTx) { exists = tx.Exists([]byte("k")) })
	if exists {
		t.Fatal("Exists reported an expired key")
	}
}

func TestWritesDoNotResurrectExpiredKeys(t *testing.T) {
	c := newFakeClock()
	s := New(c.Now)
	set(s, "k", "v", c.Ms()+10)
	c.Advance(time.Second)

	var setExp, persisted, deleted bool
	s.Update(func(tx *Tx) {
		setExp = tx.SetExpireAt([]byte("k"), tx.Now()+100_000)
		persisted = tx.Persist([]byte("k"))
		deleted = tx.Delete([]byte("k"))
	})
	if setExp || persisted || deleted {
		t.Fatalf("operations on expired key succeeded: expire=%v persist=%v delete=%v", setExp, persisted, deleted)
	}
	if got := s.Stats().ExpiredKeys; got != 1 {
		t.Fatalf("ExpiredKeys = %d, want 1", got)
	}
}

func TestSetClearsTTLAndPastExpiryDeletes(t *testing.T) {
	c := newFakeClock()
	s := New(c.Now)
	set(s, "k", "v", c.Ms()+10)
	set(s, "k", "v2", 0) // plain SET removes the TTL
	c.Advance(time.Hour)
	if v, ok := get(s, "k"); !ok || v != "v2" {
		t.Fatalf("plain Set did not clear TTL: %q %v", v, ok)
	}

	set(s, "k", "v3", c.Ms()-1) // expiry in the past deletes the key
	if _, ok := get(s, "k"); ok {
		t.Fatal("Set with past expiry left the key readable")
	}
	if n := s.Len(); n != 0 {
		t.Fatalf("Len = %d, want 0", n)
	}
}

func TestSetExpireAtAndPersist(t *testing.T) {
	c := newFakeClock()
	s := New(c.Now)
	set(s, "k", "v", 0)

	var persistedNoTTL bool
	s.Update(func(tx *Tx) { persistedNoTTL = tx.Persist([]byte("k")) })
	if persistedNoTTL {
		t.Fatal("Persist on a key without TTL returned true")
	}

	s.Update(func(tx *Tx) { tx.SetExpireAt([]byte("k"), tx.Now()+500) })
	var v Value
	s.View(func(tx *ReadTx) { v, _ = tx.Get([]byte("k")) })
	if v.ExpireAt != c.Ms()+500 {
		t.Fatalf("ExpireAt = %d, want %d", v.ExpireAt, c.Ms()+500)
	}

	var persisted bool
	s.Update(func(tx *Tx) { persisted = tx.Persist([]byte("k")) })
	if !persisted {
		t.Fatal("Persist returned false for a key with TTL")
	}
	c.Advance(time.Hour)
	if _, ok := get(s, "k"); !ok {
		t.Fatal("key expired after Persist")
	}

	var existed bool
	s.Update(func(tx *Tx) { existed = tx.SetExpireAt([]byte("k"), tx.Now()) })
	if !existed {
		t.Fatal("SetExpireAt(now) returned false")
	}
	if _, ok := get(s, "k"); ok {
		t.Fatal("SetExpireAt with a non-future time must delete the key")
	}
}

func TestDeleteExpiredBatches(t *testing.T) {
	c := newFakeClock()
	s := New(c.Now)
	for i := 0; i < 1000; i++ {
		set(s, fmt.Sprint("tmp", i), "v", c.Ms()+int64(i%10)+1)
	}
	for i := 0; i < 50; i++ {
		set(s, fmt.Sprint("keep", i), "v", 0)
	}
	c.Advance(time.Second)

	if n := s.Len(); n != 50 {
		t.Fatalf("Len before reclaim = %d, want 50", n)
	}
	if got := s.DeleteExpired(300); got != 300 {
		t.Fatalf("first batch = %d, want 300", got)
	}
	total := 300
	for {
		n := s.DeleteExpired(300)
		total += n
		if n < 300 {
			break
		}
	}
	if total != 1000 {
		t.Fatalf("reclaimed %d, want 1000", total)
	}
	if got := s.Stats().ExpiredKeys; got != 1000 {
		t.Fatalf("ExpiredKeys = %d, want 1000", got)
	}
	if s.Len() != 50 || len(s.data) != 50 || len(s.expiry) != 0 {
		t.Fatalf("after reclaim: Len=%d map=%d heap=%d", s.Len(), len(s.data), len(s.expiry))
	}
}

func TestRunExpiryStopsOnCancel(t *testing.T) {
	s := New(nil)
	set(s, "k", "v", time.Now().UnixMilli()+20)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.RunExpiry(ctx, 5*time.Millisecond, 100)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for s.Stats().ExpiredKeys == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunExpiry did not stop after cancel")
	}
	if s.Stats().ExpiredKeys != 1 {
		t.Fatal("sweeper did not reclaim the expired key")
	}
}

func TestUsedBytesAccounting(t *testing.T) {
	s := New(nil)
	set(s, "abc", "12345", 0)
	want := int64(3+5) + entryOverhead
	if got := s.Stats().UsedBytes; got != want {
		t.Fatalf("UsedBytes = %d, want %d", got, want)
	}
	set(s, "abc", "1", 0)
	if got := s.Stats().UsedBytes; got != int64(3+1)+entryOverhead {
		t.Fatalf("after overwrite UsedBytes = %d", got)
	}
	s.Update(func(tx *Tx) { tx.Delete([]byte("abc")) })
	if got := s.Stats().UsedBytes; got != 0 {
		t.Fatalf("after delete UsedBytes = %d, want 0", got)
	}
}

func TestFlushAndSnapshot(t *testing.T) {
	c := newFakeClock()
	s := New(c.Now)
	set(s, "a", "1", 0)
	set(s, "b", "2", c.Ms()+5000)
	set(s, "gone", "3", c.Ms()+1)
	c.Advance(10 * time.Millisecond)

	var items []Item
	s.View(func(tx *ReadTx) { items = tx.Snapshot() })
	if len(items) != 2 {
		t.Fatalf("snapshot has %d items, want 2 (expired keys excluded): %+v", len(items), items)
	}

	var flushed int
	s.Update(func(tx *Tx) { flushed = tx.Flush() })
	if flushed != 2 || s.Len() != 0 || s.Stats().UsedBytes != 0 {
		t.Fatalf("Flush removed %d, Len=%d, Used=%d", flushed, s.Len(), s.Stats().UsedBytes)
	}
}

// TestHeapInvariantRandomized drives random operations and checks after each
// one that the heap is a valid min-heap with correct back-pointers and that
// Len agrees with a brute-force count.
func TestHeapInvariantRandomized(t *testing.T) {
	c := newFakeClock()
	s := New(c.Now)
	r := rand.New(rand.NewPCG(1, 2))
	for step := 0; step < 20000; step++ {
		key := []byte(fmt.Sprint("k", r.IntN(200)))
		s.Update(func(tx *Tx) {
			switch r.IntN(6) {
			case 0, 1:
				var exp int64
				if r.IntN(2) == 0 {
					exp = tx.Now() + int64(r.IntN(2000)) - 200
				}
				tx.Set(key, []byte("v"), exp)
			case 2:
				tx.Delete(key)
			case 3:
				tx.SetExpireAt(key, tx.Now()+int64(r.IntN(2000))-100)
			case 4:
				tx.Persist(key)
			case 5:
				tx.Get(key)
			}
		})
		if r.IntN(10) == 0 {
			c.Advance(time.Duration(r.IntN(300)) * time.Millisecond)
		}
		if r.IntN(50) == 0 {
			s.DeleteExpired(r.IntN(20) + 1)
		}
		checkInvariants(t, s)
	}
}

func checkInvariants(t *testing.T, s *Store) {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.clock().UnixMilli()
	volatile, live := 0, 0
	var bytes int64
	for k, e := range s.data {
		if e.key != k {
			t.Fatalf("entry key %q stored under %q", e.key, k)
		}
		bytes += e.size()
		if e.expireAt != 0 {
			volatile++
			if e.heapIdx < 0 || e.heapIdx >= len(s.expiry) || s.expiry[e.heapIdx] != e {
				t.Fatalf("volatile key %q has bad heap index %d", k, e.heapIdx)
			}
		} else if e.heapIdx != -1 {
			t.Fatalf("persistent key %q is in the heap", k)
		}
		if !e.expiredAt(now) {
			live++
		}
	}
	if volatile != len(s.expiry) {
		t.Fatalf("heap has %d entries, %d volatile keys", len(s.expiry), volatile)
	}
	for i := 1; i < len(s.expiry); i++ {
		if s.expiry[(i-1)/2].expireAt > s.expiry[i].expireAt {
			t.Fatalf("heap order violated at %d", i)
		}
	}
	tx := &ReadTx{s: s, now: now}
	if got := tx.Len(); got != live {
		t.Fatalf("Len = %d, brute force = %d", got, live)
	}
	if bytes != s.usedBytes {
		t.Fatalf("usedBytes = %d, recomputed %d", s.usedBytes, bytes)
	}
}

// TestConcurrentAccess hammers the store from many goroutines. It asserts
// little by itself; its job is to give `go test -race` real interleavings.
func TestConcurrentAccess(t *testing.T) {
	s := New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.RunExpiry(ctx, time.Millisecond, 64)

	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w), 7))
			for i := 0; i < 3000; i++ {
				key := []byte(fmt.Sprint("k", r.IntN(64)))
				switch r.IntN(5) {
				case 0:
					s.Update(func(tx *Tx) { tx.Set(key, []byte("v"), tx.Now()+int64(r.IntN(5))) })
				case 1:
					s.Update(func(tx *Tx) { tx.Set(key, []byte("v"), 0) })
				case 2:
					s.Update(func(tx *Tx) { tx.Delete(key) })
				case 3:
					s.View(func(tx *ReadTx) { tx.Get(key) })
				case 4:
					_ = s.Len()
					_ = s.Stats()
				}
			}
		}()
	}
	wg.Wait()
	checkInvariants(t, s)
}

func TestLoadingModeIgnoresExpiry(t *testing.T) {
	c := newFakeClock()
	s := New(c.Now)
	s.SetLoading(true)
	set(s, "k", "v", c.Ms()-1000) // already past, as in a replayed record
	var persisted bool
	s.Update(func(tx *Tx) { persisted = tx.Persist([]byte("k")) })
	s.SetLoading(false)
	if !persisted {
		t.Fatal("PERSIST during loading did not see the key")
	}
	if v, ok := get(s, "k"); !ok || v != "v" {
		t.Fatalf("key lost after load: %q %v", v, ok)
	}

	s.SetLoading(true)
	set(s, "gone", "v", c.Ms()-1000)
	s.SetLoading(false)
	if _, ok := get(s, "gone"); ok {
		t.Fatal("expired key visible after loading ended")
	}
	if n := s.Len(); n != 1 {
		t.Fatalf("Len = %d, want 1", n)
	}
}
