// Package diskcache provides a small on-disk blob cache for data that is
// expensive to fetch and safe to lose. WaxTap uses it for YouTube's player JS
// (base.js), which is a few MiB and only changes when YouTube rotates players.
//
// The cache is deliberately fail-soft. Disk errors are logged and treated as
// misses or skipped writes; callers never receive them. A read-only or full
// filesystem just means the resolver fetches from the network again.
//
// Entries are individual SHA-256(key) files under Dir/v<schema>. Writes use a
// sibling temp file and rename, files are mode 0600, and eviction removes the
// least-recently-used entries once the size cap is exceeded. Stores are safe for
// concurrent use inside one process; atomic renames keep cross-process readers
// from seeing torn writes.
package diskcache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// defaultMaxBytes caps retained bytes. base.js is a few MiB, so this holds
	// roughly twenty distinct player versions before eviction begins.
	defaultMaxBytes = 64 << 20
	// defaultTTL bounds how long an unused entry survives. Player URLs encode the
	// player version, so an entry's content never changes; the TTL only reclaims
	// space from players that stopped being requested.
	defaultTTL = 30 * 24 * time.Hour
	// tmpPrefix marks in-progress writes. Names beginning with a dot are skipped
	// when scanning for entries, so a temp file is never read or evicted as one.
	tmpPrefix = ".tmp-"
)

// Options configures a Store. New fills zero values with defaults.
type Options struct {
	// Dir is the base cache directory. Entries live under Dir/v<SchemaVersion>.
	// An empty Dir yields a disabled store whose operations are no-ops.
	Dir string
	// MaxBytes caps total retained bytes (0 => default). A non-positive value
	// after defaulting disables eviction.
	MaxBytes int64
	// TTL is how long an entry survives without being read (0 => default).
	TTL time.Duration
	// SchemaVersion namespaces entries; bumping it hides every older entry.
	SchemaVersion int
	// Logger receives best-effort debug logs. If nil, logging is discarded.
	Logger *slog.Logger
}

// Store is a size-capped, schema-versioned, on-disk blob cache. The zero value is
// not usable; construct one with New.
type Store struct {
	dir      string // schema-versioned directory holding entry files ("" = disabled)
	maxBytes int64
	ttl      time.Duration
	log      *slog.Logger

	mu  sync.Mutex       // serializes writes and eviction scans within the process
	now func() time.Time // injectable clock for tests
}

// New returns a Store with defaults applied. It never fails: directory creation
// is deferred to the first write and is itself best-effort, so a Store backed by
// an unwritable location simply behaves as a perpetual miss.
func New(opts Options) *Store {
	if opts.MaxBytes == 0 {
		opts.MaxBytes = defaultMaxBytes
	}
	if opts.TTL <= 0 {
		opts.TTL = defaultTTL
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	dir := opts.Dir
	if dir != "" {
		dir = filepath.Join(dir, "v"+strconv.Itoa(opts.SchemaVersion))
	}
	return &Store{dir: dir, maxBytes: opts.MaxBytes, ttl: opts.TTL, log: log, now: time.Now}
}

// Get returns the cached bytes for key if present and unexpired. A miss (absent,
// expired, or unreadable) returns ok=false. Reading refreshes the entry's
// recency for LRU eviction.
func (s *Store) Get(key string) ([]byte, bool) {
	if s.dir == "" {
		return nil, false
	}
	path := s.pathFor(key)
	info, err := os.Stat(path)
	if err != nil {
		return nil, false // miss, including not-exist
	}
	if s.now().Sub(info.ModTime()) > s.ttl {
		s.remove(path) // expired; reclaim it
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	now := s.now()
	_ = os.Chtimes(path, now, now) // touch for LRU recency; best-effort
	return data, true
}

// Put stores data under key, evicting least-recently-used entries if the total
// exceeds MaxBytes. All failures are logged at debug and dropped: the cache is an
// optimization, never a source of caller errors.
func (s *Store) Put(key string, data []byte) {
	if s.dir == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		s.log.Debug("diskcache: create dir failed", "dir", s.dir, "err", err)
		return
	}
	path := s.pathFor(key)
	if err := s.writeAtomic(path, data); err != nil {
		s.log.Debug("diskcache: write failed", "err", err)
		return
	}
	now := s.now()
	_ = os.Chtimes(path, now, now) // drive mtime from the (injectable) clock
	s.evictLocked()
}

// writeAtomic writes data to a sibling temp file and renames it into place, so a
// concurrent reader never observes a partial write. The temp file is removed if
// the rename does not complete.
func (s *Store) writeAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(s.dir, tmpPrefix+"*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	// os.CreateTemp already makes the file 0600 on Unix; set it explicitly so the
	// guarantee does not depend on the platform's default.
	if err = tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
}

// evictLocked removes oldest-first until retained bytes fit MaxBytes. The caller
// holds s.mu.
func (s *Store) evictLocked() {
	if s.maxBytes <= 0 {
		return
	}
	dirents, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	type item struct {
		path string
		size int64
		mod  time.Time
	}
	var items []item
	var total int64
	for _, de := range dirents {
		if de.IsDir() || strings.HasPrefix(de.Name(), ".") {
			continue // skip subdirs and in-progress temp files
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		items = append(items, item{filepath.Join(s.dir, de.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	if total <= s.maxBytes {
		return
	}
	slices.SortFunc(items, func(a, b item) int { return a.mod.Compare(b.mod) })
	for _, it := range items {
		if total <= s.maxBytes {
			break
		}
		if err := os.Remove(it.path); err == nil || errors.Is(err, fs.ErrNotExist) {
			total -= it.size
		}
	}
}

// pathFor maps a key to its entry path. Keys (player URLs) are hashed so any
// characters are filesystem-safe and entry names are fixed-length.
func (s *Store) pathFor(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:]))
}

// remove deletes path, ignoring a missing file.
func (s *Store) remove(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Debug("diskcache: remove failed", "err", err)
	}
}
