// Package cache provides a per-file diagnostics cache with freshness tracking.
// It supports blocking waits for fresh diagnostics and is safe for concurrent use.
package cache

import (
	"context"
	"sync"
	"time"
)

// Diagnostic mirrors the essential fields from the LSP Diagnostic type.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity *int   `json:"severity,omitempty"`
	Source   string `json:"source,omitempty"`
	Code     string `json:"code,omitempty"`
	Tags     []int  `json:"tags,omitempty"`
	Message  string `json:"message"`
}

// Range represents a text range in a document.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Position represents a line/character position in a document.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type entry struct {
	sentVersion          int
	diagnosticsVersion   *int
	diagnosticsTimestamp time.Time
	lastChangeTimestamp  time.Time
	diagnostics          []Diagnostic

	// changeSeq is incremented on each RecordChange. updateSeq is set to
	// the current changeSeq on each Update. An entry is only fresh when
	// updateSeq >= changeSeq, ensuring that an Update received *before*
	// RecordChange is not mistaken for a response to that change.
	changeSeq uint64
	updateSeq uint64

	// broadcast is closed on each Update to wake waiters, then recreated.
	broadcast chan struct{}
}

func (e *entry) isFresh() bool {
	// No change ever recorded: always fresh.
	if e.changeSeq == 0 {
		return true
	}
	// An Update must have arrived after the most recent RecordChange.
	if e.updateSeq < e.changeSeq {
		return false
	}
	// Version-based freshness.
	if e.diagnosticsVersion != nil && e.sentVersion > 0 {
		return *e.diagnosticsVersion >= e.sentVersion
	}
	// Timestamp-based freshness.
	return !e.diagnosticsTimestamp.Before(e.lastChangeTimestamp)
}

// Cache stores per-file LSP diagnostics with freshness tracking.
type Cache struct {
	mu      sync.Mutex
	entries map[string]*entry
}

// New creates an empty diagnostics cache.
func New() *Cache {
	return &Cache{
		entries: make(map[string]*entry),
	}
}

func (c *Cache) getOrCreate(uri string) *entry {
	e, ok := c.entries[uri]
	if !ok {
		e = &entry{
			broadcast: make(chan struct{}),
		}
		c.entries[uri] = e
	}
	return e
}

// Update stores diagnostics for a file. Called when publishDiagnostics
// arrives (push) or when a pull response is received.
func (c *Cache) Update(uri string, version *int, diagnostics []Diagnostic) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e := c.getOrCreate(uri)
	e.diagnosticsVersion = version
	e.diagnosticsTimestamp = time.Now()
	e.diagnostics = diagnostics
	e.updateSeq = e.changeSeq

	// Broadcast: close the current channel to wake all waiters, then
	// replace it with a fresh one for future waits.
	close(e.broadcast)
	e.broadcast = make(chan struct{})
}

// RecordChange records that a didOpen/didChange was sent for a file at the
// given version. Any Update received before this call will not satisfy
// freshness — only a subsequent Update will.
func (c *Cache) RecordChange(uri string, version int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e := c.getOrCreate(uri)
	e.sentVersion = version
	e.lastChangeTimestamp = time.Now()
	e.changeSeq++
}

// Get returns cached diagnostics for a file. Does not block.
func (c *Cache) Get(uri string) (diagnostics []Diagnostic, stale bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[uri]
	if !ok {
		return nil, false
	}
	return e.diagnostics, !e.isFresh()
}

// Remove deletes the cache entry for a file URI. Called when a watched file
// is deleted from disk.
func (c *Cache) Remove(uri string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, uri)
}

// CacheStats holds summary statistics about the cache contents.
type CacheStats struct {
	TotalFiles int
	TotalDiags int
}

// Stats returns aggregate statistics about the cache.
func (c *Cache) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()

	stats := CacheStats{TotalFiles: len(c.entries)}
	for _, entry := range c.entries {
		stats.TotalDiags += len(entry.diagnostics)
	}
	return stats
}

// WaitForFresh blocks until fresh diagnostics are available for the file,
// or the context is cancelled (including timeout).
func (c *Cache) WaitForFresh(ctx context.Context, uri string) ([]Diagnostic, error) {
	for {
		c.mu.Lock()
		e := c.getOrCreate(uri)
		if e.isFresh() {
			diags := e.diagnostics
			c.mu.Unlock()
			return diags, nil
		}
		// Grab the broadcast channel before releasing the lock so we don't
		// miss a notification that fires between Unlock and the select.
		ch := e.broadcast
		c.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ch:
			// An Update happened; loop back and re-check freshness.
		}
	}
}
