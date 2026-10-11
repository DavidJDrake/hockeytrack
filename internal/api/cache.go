package api

import (
	"errors"
	"sync"
	"time"
)

// memo is a small TTL cache with per-key single flight: concurrent misses
// on one key share one load. A warm Lambda answers most requests from it
// without touching S3. Misses that return errNotFound are remembered
// briefly so an unknown season is not re-listed on every request.
type memo struct {
	mu      sync.Mutex
	entries map[string]*entry
	now     func() time.Time
}

type entry struct {
	mu      sync.Mutex
	val     any
	err     error
	expires time.Time
}

const negativeTTL = 30 * time.Second

func newMemo(now func() time.Time) *memo {
	return &memo{entries: map[string]*entry{}, now: now}
}

// get returns the cached value for key, loading it when absent or expired.
func (m *memo) get(key string, ttl time.Duration, load func() (any, error)) (any, error) {
	m.mu.Lock()
	e, ok := m.entries[key]
	if !ok {
		e = &entry{}
		m.entries[key] = e
	}
	m.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	now := m.now()
	if !e.expires.IsZero() && now.Before(e.expires) {
		return e.val, e.err
	}
	val, err := load()
	switch {
	case err == nil:
		e.val, e.err, e.expires = val, nil, now.Add(ttl)
	case errors.Is(err, errNotFound):
		e.val, e.err, e.expires = nil, err, now.Add(min(ttl, negativeTTL))
	default:
		// A transport failure is not cached: the next request retries.
		e.val, e.err, e.expires = nil, nil, time.Time{}
	}
	return val, err
}

// forget drops one key, for tests and for a write path that knows better.
func (m *memo) forget(key string) {
	m.mu.Lock()
	delete(m.entries, key)
	m.mu.Unlock()
}
