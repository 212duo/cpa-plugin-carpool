package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

var errMonitorBusy = errors.New("monitor is busy or reconfigured")

// Cache identities contain a verified key hash, never a plaintext key or a
// caller-supplied identity. Database and configuration generations are explicit.
type monitorCacheKey struct {
	database                             *monitorDatabase
	revision, keyHash, rangeName, status string
	limit                                int
}

type monitorCacheEntry struct {
	data          []byte
	expires, used time.Time
}

type monitorFlight struct {
	done chan struct{}
	data []byte
	err  error
}

// byteCache owns immutable copies; no response shares mutable bytes with a
// different caller. Both memory and concurrent cache misses are bounded.
type monitorByteCache struct {
	mu                          sync.Mutex
	entries                     map[monitorCacheKey]monitorCacheEntry
	flights                     map[monitorCacheKey]*monitorFlight
	bytes, maxBytes, maxEntries int
	ttl                         time.Duration
	now                         func() time.Time
	capExpiry                   func([]byte) time.Time
	closed                      bool
}

func newMonitorByteCache(ttl time.Duration, maxEntries, maxBytes int) *monitorByteCache {
	return &monitorByteCache{entries: make(map[monitorCacheKey]monitorCacheEntry), flights: make(map[monitorCacheKey]*monitorFlight), ttl: ttl, maxEntries: maxEntries, maxBytes: maxBytes, now: time.Now}
}

func (c *monitorByteCache) get(ctx context.Context, key monitorCacheKey, load func() ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errMonitorBusy
	}
	now := c.now()
	if entry, ok := c.entries[key]; ok && now.Before(entry.expires) {
		entry.used = now
		c.entries[key] = entry
		data := append([]byte(nil), entry.data...)
		c.mu.Unlock()
		return data, nil
	}
	if flight := c.flights[key]; flight != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-flight.done:
			return append([]byte(nil), flight.data...), flight.err
		}
	}
	if len(c.flights) >= 128 {
		c.mu.Unlock()
		return nil, errMonitorBusy
	}
	flight := &monitorFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.mu.Unlock()
	data, err := load()
	if ctx.Err() != nil {
		data = nil
		err = ctx.Err()
	}
	c.mu.Lock()
	if c.closed {
		data = nil
		err = errMonitorBusy
	}
	if err == nil && len(data) <= c.maxBytes {
		now = c.now()
		expires := now.Add(c.ttl)
		if c.capExpiry != nil {
			if cap := c.capExpiry(data); cap.Before(expires) {
				expires = cap
			}
		}
		for k, e := range c.entries {
			if !now.Before(e.expires) || k == key {
				delete(c.entries, k)
				c.bytes -= len(e.data)
			}
		}
		for len(c.entries) >= c.maxEntries || c.bytes+len(data) > c.maxBytes {
			var oldest monitorCacheKey
			var at time.Time
			for k, e := range c.entries {
				if at.IsZero() || e.used.Before(at) {
					oldest = k
					at = e.used
				}
			}
			if at.IsZero() {
				break
			}
			c.bytes -= len(c.entries[oldest].data)
			delete(c.entries, oldest)
		}
		if now.Before(expires) {
			c.entries[key] = monitorCacheEntry{data: append([]byte(nil), data...), expires: expires, used: now}
			c.bytes += len(data)
		}
	}
	flight.data = append([]byte(nil), data...)
	flight.err = err
	delete(c.flights, key)
	close(flight.done)
	c.mu.Unlock()
	return data, err
}

func (c *monitorByteCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.entries = make(map[monitorCacheKey]monitorCacheEntry)
	c.bytes = 0
	// Running loaders observe closed before publishing, then wake their waiters.
}
