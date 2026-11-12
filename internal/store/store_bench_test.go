package store

import (
	"strconv"
	"testing"
)

func benchKeys(n int) [][]byte {
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = []byte("key:" + strconv.Itoa(i))
	}
	return keys
}

func BenchmarkStoreGet(b *testing.B) {
	s := New(nil)
	keys := benchKeys(10000)
	for _, k := range keys {
		s.Update(func(tx *Tx) { tx.Set(k, []byte("value"), 0) })
	}
	i := 0
	for b.Loop() {
		s.View(func(tx *ReadTx) { tx.Get(keys[i%len(keys)]) })
		i++
	}
}

func BenchmarkStoreSet(b *testing.B) {
	s := New(nil)
	keys := benchKeys(10000)
	v := []byte("value")
	i := 0
	for b.Loop() {
		s.Update(func(tx *Tx) { tx.Set(keys[i%len(keys)], v, 0) })
		i++
	}
}

func BenchmarkStoreSetWithTTL(b *testing.B) {
	s := New(nil)
	keys := benchKeys(10000)
	v := []byte("value")
	i := 0
	for b.Loop() {
		s.Update(func(tx *Tx) { tx.Set(keys[i%len(keys)], v, tx.Now()+int64(60000+i%1000)) })
		i++
	}
}

// 90% reads / 10% writes from all CPUs: measures single-lock contention.
func BenchmarkStoreMixedParallel(b *testing.B) {
	s := New(nil)
	keys := benchKeys(10000)
	v := []byte("value")
	for _, k := range keys {
		s.Update(func(tx *Tx) { tx.Set(k, v, 0) })
	}
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			k := keys[(i*7919)%len(keys)]
			if i%10 == 0 {
				s.Update(func(tx *Tx) { tx.Set(k, v, 0) })
			} else {
				s.View(func(tx *ReadTx) { tx.Get(k) })
			}
			i++
		}
	})
}
