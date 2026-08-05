package daemon

import (
	"encoding/json"
	"slices"
	"sync"
	"time"

	"go.kenn.io/kata/internal/db"
)

const (
	uiSnapshotAuthorityCacheTTL      = 5 * time.Second
	uiSnapshotAuthorityCacheCapacity = 128
)

type uiSnapshotAuthorityCacheEntry struct {
	data      db.UISnapshotData
	expiresAt time.Time
	sequence  uint64
}

type uiSnapshotCache struct {
	mu       sync.Mutex
	entries  map[string]uiSnapshotAuthorityCacheEntry
	clock    func() time.Time
	clone    func(db.UISnapshotData) db.UISnapshotData
	sequence uint64
}

func newUISnapshotAuthorityCache() *uiSnapshotCache {
	return newUISnapshotCache(cloneUISnapshotAuthority)
}

func newUISnapshotEnrichmentCache() *uiSnapshotCache {
	return newUISnapshotCache(cloneUISnapshotEnrichment)
}

func newUISnapshotCache(clone func(db.UISnapshotData) db.UISnapshotData) *uiSnapshotCache {
	return &uiSnapshotCache{
		entries: make(map[string]uiSnapshotAuthorityCacheEntry),
		clock:   time.Now,
		clone:   clone,
	}
}

func (c *uiSnapshotCache) get(key string) (db.UISnapshotData, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return db.UISnapshotData{}, false
	}
	if !c.clock().Before(entry.expiresAt) {
		delete(c.entries, key)
		return db.UISnapshotData{}, false
	}
	return c.clone(entry.data), true
}

func (c *uiSnapshotCache) put(key string, data db.UISnapshotData) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	for cachedKey, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, cachedKey)
		}
	}
	if _, exists := c.entries[key]; !exists && len(c.entries) >= uiSnapshotAuthorityCacheCapacity {
		var oldestKey string
		var oldestSequence uint64
		for cachedKey, entry := range c.entries {
			if oldestKey == "" || entry.sequence < oldestSequence {
				oldestKey = cachedKey
				oldestSequence = entry.sequence
			}
		}
		delete(c.entries, oldestKey)
	}
	c.sequence++
	c.entries[key] = uiSnapshotAuthorityCacheEntry{
		data:      c.clone(data),
		expiresAt: now.Add(uiSnapshotAuthorityCacheTTL),
		sequence:  c.sequence,
	}
}

func uiSnapshotAuthorityKey(intent normalizedUISnapshotIntent) (string, error) {
	intent.SelectedIssueUID = ""
	intent.IncludeGraph = false
	intent.IncludeHistory = false
	return uiSnapshotKey(intent)
}

func projectUISnapshotAuthority(data db.UISnapshotData, projectUID string) db.UISnapshotData {
	targetProjectID := int64(0)
	for _, project := range data.Projects {
		if project.Project.UID == projectUID {
			targetProjectID = project.Project.ID
			break
		}
	}
	issues := make([]db.UIIssue, 0)
	issueUIDs := make(map[string]struct{})
	for _, issue := range data.Issues {
		if issue.ProjectUID != projectUID && (targetProjectID == 0 || issue.ProjectID != targetProjectID) {
			continue
		}
		issues = append(issues, issue)
		issueUIDs[issue.UID] = struct{}{}
	}
	links := make([]db.UILink, 0)
	for _, link := range data.CollectionLinks {
		_, fromVisible := issueUIDs[link.FromIssueUID]
		_, toVisible := issueUIDs[link.ToIssueUID]
		if fromVisible || toVisible {
			links = append(links, link)
		}
	}
	return db.UISnapshotData{
		Cursor:          data.Cursor,
		Projects:        slices.Clone(data.Projects),
		Issues:          issues,
		CollectionLinks: links,
	}
}

func uiSnapshotKey(intent normalizedUISnapshotIntent) (string, error) {
	encoded, err := json.Marshal(intent)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func cloneUISnapshotEnrichment(data db.UISnapshotData) db.UISnapshotData {
	cloned := db.UISnapshotData{
		Cursor:          data.Cursor,
		SelectedState:   data.SelectedState,
		AuthorityReused: false,
	}
	cloned.Comments = slices.Clone(data.Comments)
	cloned.SelectedLabels = slices.Clone(data.SelectedLabels)
	cloned.SelectedLinks = slices.Clone(data.SelectedLinks)
	cloned.Recurrences = slices.Clone(data.Recurrences)
	cloned.History = slices.Clone(data.History)
	cloned.GraphIssues = slices.Clone(data.GraphIssues)
	cloned.GraphLinks = slices.Clone(data.GraphLinks)
	cloned.GraphEdges = slices.Clone(data.GraphEdges)
	cloned.GraphUnresolvedRefs = slices.Clone(data.GraphUnresolvedRefs)
	if data.SelectedIssue != nil {
		selected := *data.SelectedIssue
		selected.Labels = slices.Clone(data.SelectedIssue.Labels)
		cloned.SelectedIssue = &selected
	}
	return cloned
}

func uiSnapshotHasEnrichment(intent normalizedUISnapshotIntent) bool {
	return intent.SelectedIssueUID != "" || intent.IncludeGraph
}

func cloneUISnapshotAuthority(data db.UISnapshotData) db.UISnapshotData {
	return db.UISnapshotData{
		Cursor:          data.Cursor,
		Projects:        slices.Clone(data.Projects),
		Issues:          slices.Clone(data.Issues),
		CollectionLinks: slices.Clone(data.CollectionLinks),
	}
}

func mergeUISnapshotAuthority(data *db.UISnapshotData, authority db.UISnapshotData) {
	data.Projects = authority.Projects
	data.Issues = authority.Issues
	data.CollectionLinks = authority.CollectionLinks
	data.AuthorityReused = false
}
