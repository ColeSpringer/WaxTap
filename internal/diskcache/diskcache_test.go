package diskcache

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// newClocked returns a Store with a controllable clock for deterministic TTL and
// recency tests.
func newClocked(t *testing.T, opts Options) (*Store, *fakeClock) {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	s := New(opts)
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s.now = clk.now
	return s, clk
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestPutGetRoundTrip(t *testing.T) {
	s, _ := newClocked(t, Options{SchemaVersion: 1})
	want := []byte("base.js source bytes")
	s.Put("https://youtube.com/s/player/abc/base.js", want)

	got, ok := s.Get("https://youtube.com/s/player/abc/base.js")
	if !ok {
		t.Fatal("expected hit")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestGetMissOnAbsentKey(t *testing.T) {
	s, _ := newClocked(t, Options{SchemaVersion: 1})
	if _, ok := s.Get("nope"); ok {
		t.Fatal("expected miss")
	}
}

func TestTTLExpiry(t *testing.T) {
	s, clk := newClocked(t, Options{SchemaVersion: 1, TTL: time.Hour})
	s.Put("k", []byte("v"))

	clk.advance(time.Hour + time.Second)
	if _, ok := s.Get("k"); ok {
		t.Fatal("expected expired miss")
	}
	// The expired entry should have been reclaimed.
	if entries, _ := os.ReadDir(s.dir); len(entries) != 0 {
		t.Fatalf("expired entry not removed: %v", entries)
	}
}

func TestReadRefreshesRecency(t *testing.T) {
	s, clk := newClocked(t, Options{SchemaVersion: 1, TTL: time.Hour})
	s.Put("k", []byte("v"))

	// Just before expiry, a read should renew the TTL window.
	clk.advance(50 * time.Minute)
	if _, ok := s.Get("k"); !ok {
		t.Fatal("expected hit before expiry")
	}
	clk.advance(50 * time.Minute) // 100m since Put, but only 50m since the read
	if _, ok := s.Get("k"); !ok {
		t.Fatal("read should have refreshed recency, keeping the entry alive")
	}
}

func TestSchemaVersionIsolation(t *testing.T) {
	dir := t.TempDir()
	v1 := New(Options{Dir: dir, SchemaVersion: 1})
	v1.Put("k", []byte("from v1"))

	v2 := New(Options{Dir: dir, SchemaVersion: 2})
	if _, ok := v2.Get("k"); ok {
		t.Fatal("v2 must not see v1 entries")
	}
	// v1 still has its entry.
	if _, ok := v1.Get("k"); !ok {
		t.Fatal("v1 lost its own entry")
	}
}

func TestSizeCapEviction(t *testing.T) {
	// Cap at 30 bytes; three 20-byte entries cannot coexist.
	s, clk := newClocked(t, Options{SchemaVersion: 1, MaxBytes: 30})
	blob := bytes.Repeat([]byte("x"), 20)

	s.Put("a", blob)
	clk.advance(time.Second)
	s.Put("b", blob)
	clk.advance(time.Second)
	s.Put("c", blob) // total would be 60 > 30; oldest entries are evicted

	if _, ok := s.Get("c"); !ok {
		t.Fatal("newest entry c should survive")
	}
	if _, ok := s.Get("a"); ok {
		t.Fatal("oldest entry a should have been evicted")
	}
}

func TestEvictionHonorsReadRecency(t *testing.T) {
	s, clk := newClocked(t, Options{SchemaVersion: 1, MaxBytes: 50})
	blob := bytes.Repeat([]byte("x"), 20)

	s.Put("a", blob)
	clk.advance(time.Second)
	s.Put("b", blob)
	clk.advance(time.Second)

	// Touch "a" so it is now more recently used than "b".
	if _, ok := s.Get("a"); !ok {
		t.Fatal("a should be present")
	}
	clk.advance(time.Second)

	s.Put("c", blob) // 60 > 50; the least-recently-used ("b") should go
	if _, ok := s.Get("a"); !ok {
		t.Fatal("a was read recently and should survive")
	}
	if _, ok := s.Get("b"); ok {
		t.Fatal("b was least-recently-used and should have been evicted")
	}
}

func TestFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	s, _ := newClocked(t, Options{SchemaVersion: 1})
	s.Put("k", []byte("v"))
	info, err := os.Stat(s.pathFor("k"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("entry perms = %o, want 600", perm)
	}
}

func TestDisabledStoreIsNoOp(t *testing.T) {
	s := New(Options{Dir: ""}) // empty Dir => disabled
	s.Put("k", []byte("v"))    // must not panic
	if _, ok := s.Get("k"); ok {
		t.Fatal("disabled store must always miss")
	}
}

func TestPutFailSoftOnUnwritableDir(t *testing.T) {
	// Point the cache at a path whose parent is a regular file, so MkdirAll fails.
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := newClocked(t, Options{Dir: filepath.Join(file, "cache"), SchemaVersion: 1})
	s.Put("k", []byte("v")) // must not panic or error
	if _, ok := s.Get("k"); ok {
		t.Fatal("write to an unwritable dir should leave a clean miss")
	}
}

func TestTempFilesAreNotEntries(t *testing.T) {
	s, _ := newClocked(t, Options{SchemaVersion: 1})
	s.Put("k", []byte("v"))
	// Drop a stray temp file; it must be ignored by reads and eviction scans.
	if err := os.WriteFile(filepath.Join(s.dir, tmpPrefix+"stray"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("k"); !ok {
		t.Fatal("real entry should still be readable alongside a temp file")
	}
}

func TestConcurrentPutGet(t *testing.T) {
	s, _ := newClocked(t, Options{SchemaVersion: 1})
	const workers = 16
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "key" + string(rune('a'+i%4))
			s.Put(key, bytes.Repeat([]byte("y"), 100))
			s.Get(key)
		}(i)
	}
	wg.Wait()
}
