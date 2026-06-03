// Package cache provides a thread-safe LRU cache with TTL expiry, schema
// versioning, and singleflight de-duplication of concurrent loads.
//
// WaxTap can run many concurrent extractions that need the same player data, so
// the cache keeps multiple entries and collapses concurrent loads for the same
// key into one loader call.
//
// Schema versioning lets a format change invalidate every stale entry at once:
// bump SchemaVersion and old entries are treated as misses.
package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Options configures a Store. Zero values get sane defaults at NewStore time.
type Options struct {
	MaxEntries    int           // 0 => default
	TTL           time.Duration // 0 => default
	SchemaVersion int           // entries from other versions are misses
}

const (
	defaultMaxEntries = 256
	defaultTTL        = 6 * time.Hour
)

// Store is a generic LRU+TTL cache safe for concurrent use.
type Store[V any] struct {
	mu      sync.Mutex
	opts    Options
	ll      *list.List               // front = most recently used
	items   map[string]*list.Element // key -> *list.Element(*entry[V])
	flights map[string]*flight[V]    // in-flight loads, for singleflight
	now     func() time.Time         // injectable clock for tests
}

type entry[V any] struct {
	key     string
	val     V
	expires time.Time
	schema  int
}

// flight is one in-flight load shared by all waiters for a key.
type flight[V any] struct {
	done chan struct{}
	val  V
	err  error
}

// NewStore returns a Store with defaults applied.
func NewStore[V any](opts Options) *Store[V] {
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = defaultMaxEntries
	}
	if opts.TTL <= 0 {
		opts.TTL = defaultTTL
	}
	return &Store[V]{
		opts:    opts,
		ll:      list.New(),
		items:   make(map[string]*list.Element),
		flights: make(map[string]*flight[V]),
		now:     time.Now,
	}
}

// Get returns the cached value for key if present, unexpired, and from the
// current schema version.
func (s *Store[V]) Get(key string) (V, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(key)
}

func (s *Store[V]) getLocked(key string) (V, bool) {
	var zero V
	el, ok := s.items[key]
	if !ok {
		return zero, false
	}
	en := el.Value.(*entry[V])
	if en.schema != s.opts.SchemaVersion || !s.now().Before(en.expires) {
		s.removeElement(el)
		return zero, false
	}
	s.ll.MoveToFront(el)
	return en.val, true
}

// Put stores val under key with the configured TTL, evicting the least-recently
// used entry if at capacity.
func (s *Store[V]) Put(key string, val V) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putLocked(key, val)
}

func (s *Store[V]) putLocked(key string, val V) {
	expires := s.now().Add(s.opts.TTL)
	if el, ok := s.items[key]; ok {
		en := el.Value.(*entry[V])
		en.val, en.expires, en.schema = val, expires, s.opts.SchemaVersion
		s.ll.MoveToFront(el)
		return
	}
	el := s.ll.PushFront(&entry[V]{key: key, val: val, expires: expires, schema: s.opts.SchemaVersion})
	s.items[key] = el
	for s.ll.Len() > s.opts.MaxEntries {
		s.removeElement(s.ll.Back())
	}
}

func (s *Store[V]) removeElement(el *list.Element) {
	if el == nil {
		return
	}
	s.ll.Remove(el)
	delete(s.items, el.Value.(*entry[V]).key)
}

// GetOrLoad returns the cached value for key, or loads it exactly once across
// concurrent callers (singleflight) and caches the result. The loader's error is
// returned to all waiters and is not cached.
func (s *Store[V]) GetOrLoad(ctx context.Context, key string, load func(context.Context) (V, error)) (V, error) {
	s.mu.Lock()
	if v, ok := s.getLocked(key); ok {
		s.mu.Unlock()
		return v, nil
	}
	if fl, ok := s.flights[key]; ok {
		// Someone else is already loading this key; wait for them.
		s.mu.Unlock()
		return waitFlight(ctx, fl)
	}
	fl := &flight[V]{done: make(chan struct{})}
	s.flights[key] = fl
	s.mu.Unlock()

	fl.val, fl.err = load(ctx)

	s.mu.Lock()
	delete(s.flights, key)
	if fl.err == nil {
		s.putLocked(key, fl.val)
	}
	s.mu.Unlock()

	close(fl.done)
	return fl.val, fl.err
}

func waitFlight[V any](ctx context.Context, fl *flight[V]) (V, error) {
	select {
	case <-fl.done:
		return fl.val, fl.err
	case <-ctx.Done():
		var zero V
		return zero, ctx.Err()
	}
}

// Len returns the number of live entries (including any expired-but-not-yet-
// evicted ones).
func (s *Store[V]) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ll.Len()
}
