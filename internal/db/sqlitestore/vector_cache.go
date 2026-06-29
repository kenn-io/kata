package sqlitestore

import (
	"fmt"
	"strings"
	"sync"
)

// cachedVec is one issue's normalized vector held in memory. It carries only
// the issue id and the vector, never any visibility or row data.
type cachedVec struct {
	issueID int64
	vec     []float32
}

// cacheEntry is the cached vector set for one (projectID, fingerprint) plus the
// freshness probe it was loaded under.
type cacheEntry struct {
	count      int64
	maxUpdated string
	vecs       []cachedVec
}

// vectorCache holds normalized vectors per (projectID, fingerprint) for the
// brute-force vector search leg. It never holds visibility or row data:
// SearchVector resolves candidate ids against the live issues table on every
// call. Freshness is validated per query with a (count, maxUpdated) probe, so a
// model swap (new fingerprint) lands in a fresh entry and the old one is
// abandoned.
type vectorCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
}

func newVectorCache() *vectorCache {
	return &vectorCache{entries: map[string]*cacheEntry{}}
}

func cacheKey(projectID int64, fingerprint string) string {
	return fmt.Sprintf("%d:%s", projectID, fingerprint)
}

// invalidate drops every cached entry for the given fingerprint, across all
// projects. UpsertIssueEmbedding calls this after a write so the next query
// reloads vectors; the per-query freshness probe is the backstop for any entry
// this misses.
func (c *vectorCache) invalidate(fingerprint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	suffix := ":" + fingerprint
	for k := range c.entries {
		if strings.HasSuffix(k, suffix) {
			delete(c.entries, k)
		}
	}
}

// get returns cached vectors when the probe matches; ok is false otherwise.
func (c *vectorCache) get(projectID int64, fingerprint string, count int64, maxUpdated string) ([]cachedVec, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[cacheKey(projectID, fingerprint)]
	if !ok || e.count != count || e.maxUpdated != maxUpdated {
		return nil, false
	}
	return e.vecs, true
}

func (c *vectorCache) put(projectID int64, fingerprint string, count int64, maxUpdated string, vecs []cachedVec) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[cacheKey(projectID, fingerprint)] = &cacheEntry{count: count, maxUpdated: maxUpdated, vecs: vecs}
}
