package diskcache

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// The first write marks the cache directory, so a clean can tell WaxTap's
// cache from a directory that merely shares its name.
func TestPutWritesTheCacheTag(t *testing.T) {
	base := t.TempDir()
	s := New(Options{Dir: filepath.Join(base, "players"), SchemaVersion: 1})
	tag := filepath.Join(base, "players", TagFile)
	if _, err := os.Stat(tag); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a Store that has written nothing left %s: %v", TagFile, err)
	}
	s.Put("k", []byte("v"))
	body, err := os.ReadFile(tag)
	if err != nil {
		t.Fatalf("read the tag: %v", err)
	}
	if !strings.HasPrefix(string(body), TagSignature) {
		t.Errorf("tag = %q, want it to lead with the specification's signature", body)
	}
}

// Clean removes WaxTap's own entries and nothing else.
func TestClean(t *testing.T) {
	t.Run("a tagged cache goes, and the base with it", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "cache")
		New(Options{Dir: filepath.Join(base, "players"), SchemaVersion: 1}).Put("k", []byte("v"))
		removed, err := Clean(base)
		if err != nil || !removed {
			t.Fatalf("Clean = %v, %v, want true, nil", removed, err)
		}
		if _, serr := os.Stat(base); !errors.Is(serr, fs.ErrNotExist) {
			t.Errorf("base survived: %v", serr)
		}
	})

	t.Run("a pre-tag layout is still recognized", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "cache")
		if err := os.MkdirAll(filepath.Join(base, "players", "v1"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "players", "v1", "abc"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		removed, err := Clean(base)
		if err != nil || !removed {
			t.Fatalf("Clean = %v, %v, want true, nil", removed, err)
		}
	})

	t.Run("a foreign players directory is left alone", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "cache")
		if err := os.MkdirAll(filepath.Join(base, "players"), 0o755); err != nil {
			t.Fatal(err)
		}
		keep := filepath.Join(base, "players", "README")
		if err := os.WriteFile(keep, []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
		removed, err := Clean(base)
		if err != nil || removed {
			t.Fatalf("Clean = %v, %v, want false, nil", removed, err)
		}
		if _, serr := os.Stat(keep); serr != nil {
			t.Errorf("Clean removed a file it did not write: %v", serr)
		}
	})

	t.Run("a base holding other things keeps it", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "cache")
		New(Options{Dir: filepath.Join(base, "players"), SchemaVersion: 1}).Put("k", []byte("v"))
		other := filepath.Join(base, "notes.txt")
		if err := os.WriteFile(other, []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Clean(base); err != nil {
			t.Fatal(err)
		}
		if _, serr := os.Stat(other); serr != nil {
			t.Errorf("Clean removed a file beside the cache: %v", serr)
		}
	})

	t.Run("a regular file is an error", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "precious.txt")
		if err := os.WriteFile(file, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Clean(file); err == nil {
			t.Error("Clean on a regular file = nil, want an error")
		}
		if _, serr := os.Stat(file); serr != nil {
			t.Errorf("Clean removed a regular file: %v", serr)
		}
	})

	t.Run("a missing base is nothing to do", func(t *testing.T) {
		removed, err := Clean(filepath.Join(t.TempDir(), "absent"))
		if err != nil || removed {
			t.Fatalf("Clean = %v, %v, want false, nil", removed, err)
		}
	})
}

func TestDescribe(t *testing.T) {
	base := filepath.Join(t.TempDir(), "cache")
	if exists, dir, pop := Describe(base); exists || dir || pop {
		t.Errorf("Describe(absent) = %v, %v, %v, want all false", exists, dir, pop)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if exists, dir, pop := Describe(base); !exists || !dir || pop {
		t.Errorf("Describe(empty dir) = %v, %v, %v, want true, true, false", exists, dir, pop)
	}
	New(Options{Dir: filepath.Join(base, "players"), SchemaVersion: 1}).Put("k", []byte("v"))
	if exists, dir, pop := Describe(base); !exists || !dir || !pop {
		t.Errorf("Describe(populated) = %v, %v, %v, want all true", exists, dir, pop)
	}
}

// CACHEDIR.TAG is a shared convention, so the file's name says only that
// something caches there. Clean reads the writer line under the signature, so
// another tool's cache under a directory called players is left alone.
func TestCleanReadsTheTagRatherThanItsName(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"waxtap's own", tagBody, true},
		{"another tool's", TagSignature + "# created by some other cache\n", false},
		{"the signature alone", TagSignature, false},
		{"an empty file", "", false},
		{"not a tag at all", "hello\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := filepath.Join(t.TempDir(), "cache")
			players := filepath.Join(base, "players")
			if err := os.MkdirAll(players, 0o755); err != nil {
				t.Fatal(err)
			}
			// A file that is not a schema directory, so only the tag can
			// make this look like WaxTap's.
			keep := filepath.Join(players, "someone-elses-entry")
			if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(players, TagFile), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			removed, err := Clean(base)
			if err != nil {
				t.Fatal(err)
			}
			if removed != tc.want {
				t.Errorf("Clean = %v, want %v", removed, tc.want)
			}
			_, serr := os.Stat(keep)
			if gone := errors.Is(serr, fs.ErrNotExist); gone != tc.want {
				t.Errorf("the foreign entry removed = %v, want %v", gone, tc.want)
			}
		})
	}
}

// A base reached through a symlink is left alone: os.Remove would unlink the
// link itself whatever the target holds, so a user who points --cache-dir at a
// directory through a link would lose the link and keep the directory.
func TestCleanLeavesASymlinkedBaseAlone(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	New(Options{Dir: filepath.Join(real, "players"), SchemaVersion: 1}).Put("k", []byte("v"))
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink: %v", err)
	}

	removed, err := Clean(link)
	if err != nil || !removed {
		t.Fatalf("Clean = %v, %v, want true, nil", removed, err)
	}
	if _, serr := os.Stat(filepath.Join(real, "players")); !errors.Is(serr, fs.ErrNotExist) {
		t.Error("the entries were not removed through the link")
	}
	if fi, lerr := os.Lstat(link); lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link is gone (%v); removing it would leave the directory behind", lerr)
	}
}
