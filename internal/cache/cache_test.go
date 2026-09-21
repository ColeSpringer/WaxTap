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

// A follower never reports a cancellation it did not make. The leader's
// context ending is the leader's failure; a follower whose own context is
// live loads the value itself under that context.
func TestGetOrLoadFollowerDoesNotInheritTheLeadersCancellation(t *testing.T) {
	s := NewStore[string](Options{MaxEntries: 4, TTL: time.Minute})

	leaderIn := make(chan struct{})
	release := make(chan struct{})
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	var calls atomic.Int32

	load := func(ctx context.Context) (string, error) {
		n := calls.Add(1)
		if n == 1 {
			close(leaderIn)
			<-release
			return "", ctx.Err()
		}
		return "second answer", nil
	}

	leaderDone := make(chan error, 1)
	go func() {
		_, err := s.GetOrLoad(leaderCtx, "k", load)
		leaderDone <- err
	}()
	<-leaderIn

	followerDone := make(chan struct {
		v   string
		err error
	}, 1)
	go func() {
		v, err := s.GetOrLoad(context.Background(), "k", load)
		followerDone <- struct {
			v   string
			err error
		}{v, err}
	}()
	// Give the follower time to attach to the flight before it ends.
	time.Sleep(20 * time.Millisecond)
	cancelLeader()
	close(release)

	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Errorf("leader err = %v, want context.Canceled: it is the one that was canceled", err)
	}
	got := <-followerDone
	if got.err != nil {
		t.Fatalf("follower err = %v, want the loader's second answer", got.err)
	}
	if got.v != "second answer" {
		t.Errorf("follower value = %q, want the loader's second answer", got.v)
	}
}

// A follower whose own context is done still gets its own error at once,
// rather than waiting out the leader.
func TestGetOrLoadFollowerWithADeadContextFailsImmediately(t *testing.T) {
	s := NewStore[string](Options{MaxEntries: 4, TTL: time.Minute})
	leaderIn := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = s.GetOrLoad(context.Background(), "k", func(context.Context) (string, error) {
			close(leaderIn)
			<-release
			return "leader answer", nil
		})
	}()
	<-leaderIn
	defer close(release)

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.GetOrLoad(dead, "k", func(context.Context) (string, error) {
		t.Error("a follower with a dead context must not run the loader")
		return "", nil
	}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the follower's own context.Canceled", err)
	}
}

// A follower steps in exactly once. An unbounded chain of followers each
// inheriting the next one's cancellation is the thundering herd the
// singleflight exists to prevent, so a second inherited error is the answer.
func TestGetOrLoadFollowerRetriesOnlyOnce(t *testing.T) {
	s := NewStore[string](Options{MaxEntries: 4, TTL: time.Minute})
	var calls atomic.Int32
	leaderIn := make(chan struct{})
	release := make(chan struct{})
	leaderCtx, cancelLeader := context.WithCancel(context.Background())

	// Every call fails with a context error, so a recursing follower would
	// never stop.
	load := func(ctx context.Context) (string, error) {
		if calls.Add(1) == 1 {
			close(leaderIn)
			<-release
		}
		return "", context.DeadlineExceeded
	}
	go func() { _, _ = s.GetOrLoad(leaderCtx, "k", load) }()
	<-leaderIn

	done := make(chan error, 1)
	go func() {
		_, err := s.GetOrLoad(context.Background(), "k", load)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancelLeader()
	close(release)

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("follower err = %v, want the second attempt's own failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the follower never returned; the retry is recursing")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("loader ran %d times, want 2: the leader's and the follower's one retry", n)
	}
}
