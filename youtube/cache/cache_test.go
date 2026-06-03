package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStore_GetPut(t *testing.T) {
	s := NewStore[int](Options{MaxEntries: 4, TTL: time.Hour})
	if _, ok := s.Get("x"); ok {
		t.Fatal("unexpected hit on empty store")
	}
	s.Put("x", 42)
	if v, ok := s.Get("x"); !ok || v != 42 {
		t.Fatalf("Get = (%d, %v), want (42, true)", v, ok)
	}
}

func TestStore_TTLExpiry(t *testing.T) {
	s := NewStore[int](Options{MaxEntries: 4, TTL: time.Minute})
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }

	s.Put("x", 1)
	if _, ok := s.Get("x"); !ok {
		t.Fatal("entry should be present before TTL")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := s.Get("x"); ok {
		t.Fatal("entry should be expired after TTL")
	}
	if s.Len() != 0 {
		t.Fatalf("expired entry not evicted on read: len = %d", s.Len())
	}
}

func TestStore_LRUEviction(t *testing.T) {
	s := NewStore[int](Options{MaxEntries: 2, TTL: time.Hour})
	s.Put("a", 1)
	s.Put("b", 2)
	if _, ok := s.Get("a"); !ok { // touch a -> a is most-recently-used
		t.Fatal("a missing")
	}
	s.Put("c", 3) // capacity 2 -> evicts least-recently-used (b)

	if _, ok := s.Get("b"); ok {
		t.Error("b should have been evicted")
	}
	if _, ok := s.Get("a"); !ok {
		t.Error("a should remain")
	}
	if _, ok := s.Get("c"); !ok {
		t.Error("c should remain")
	}
}

func TestStore_SingleflightLoadsOnce(t *testing.T) {
	s := NewStore[int](Options{MaxEntries: 4, TTL: time.Hour})
	var loads atomic.Int32
	release := make(chan struct{})
	load := func(context.Context) (int, error) {
		loads.Add(1)
		<-release // hold the single loader until all callers have coalesced
		return 7, nil
	}

	const n = 20
	var wg sync.WaitGroup
	results := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := s.GetOrLoad(context.Background(), "k", load)
			if err != nil {
				t.Errorf("GetOrLoad: %v", err)
			}
			results[i] = v
		}(i)
	}

	time.Sleep(25 * time.Millisecond) // let callers pile onto the in-flight load
	close(release)
	wg.Wait()

	if got := loads.Load(); got != 1 {
		t.Fatalf("loader ran %d times, want 1", got)
	}
	for i, v := range results {
		if v != 7 {
			t.Fatalf("results[%d] = %d, want 7", i, v)
		}
	}
	if v, ok := s.Get("k"); !ok || v != 7 {
		t.Fatalf("value not cached after load: (%d, %v)", v, ok)
	}
}

func TestStore_GetOrLoadErrorNotCached(t *testing.T) {
	s := NewStore[int](Options{MaxEntries: 4, TTL: time.Hour})
	boom := errors.New("boom")

	_, err := s.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		return 0, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if _, ok := s.Get("k"); ok {
		t.Fatal("an errored load must not be cached")
	}

	v, err := s.GetOrLoad(context.Background(), "k", func(context.Context) (int, error) {
		return 9, nil
	})
	if err != nil || v != 9 {
		t.Fatalf("GetOrLoad after error = (%d, %v), want (9, nil)", v, err)
	}
	if v, ok := s.Get("k"); !ok || v != 9 {
		t.Fatal("value should be cached after a successful load")
	}
}
