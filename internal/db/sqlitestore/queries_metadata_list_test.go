package sqlitestore_test

import (
	"context"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// makeIssueWithMetadata creates an issue with the given metadata blob.
func makeIssueWithMetadata(t *testing.T, d *sqlitestore.Store, projectID int64, title string, meta map[string]jsontext.Value) db.Issue {
	t.Helper()
	issue, _, err := d.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: projectID,
		Title:     title,
		Author:    "tester",
		Metadata:  meta,
	})
	require.NoError(t, err)
	return issue
}

// TestListIssues_MetaPresenceFilter pins that `key` alone matches any issue
// where that flat key is present in top-level metadata.
func TestListIssues_MetaPresenceFilter(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	with := makeIssueWithMetadata(t, d, p.ID, "has key", map[string]jsontext.Value{
		"work.attention": jsontext.Value(`"needs-human"`),
	})
	_ = makeIssueWithMetadata(t, d, p.ID, "no key", map[string]jsontext.Value{
		"other": jsontext.Value(`"v"`),
	})

	got, err := d.ListIssues(ctx, db.ListIssuesParams{
		ProjectID: p.ID,
		Meta:      []db.MetaFilter{{Key: "work.attention"}},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, with.ShortID, got[0].ShortID)
}

// TestListIssues_MetaEqualityFilter pins that `key=value` matches only issues
// whose string-valued key equals value.
func TestListIssues_MetaEqualityFilter(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	stuck := makeIssueWithMetadata(t, d, p.ID, "stuck", map[string]jsontext.Value{
		"work.attention": jsontext.Value(`"stuck"`),
	})
	_ = makeIssueWithMetadata(t, d, p.ID, "ok", map[string]jsontext.Value{
		"work.attention": jsontext.Value(`"ok"`),
	})

	got, err := d.ListIssues(ctx, db.ListIssuesParams{
		ProjectID: p.ID,
		Meta:      []db.MetaFilter{{Key: "work.attention", Value: "stuck", HasValue: true}},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, stuck.ShortID, got[0].ShortID)
}

// TestListIssues_MetaFlatDottedKeyDoesNotTraverseNested is the adversarial
// case: a flat dotted key `work.branch` must NOT match a nested object
// {"work":{"branch":...}}. json_each iterates only top-level keys, so the
// nested issue exposes key "work" (an object), never "work.branch".
func TestListIssues_MetaFlatDottedKeyDoesNotTraverseNested(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	flat := makeIssueWithMetadata(t, d, p.ID, "flat", map[string]jsontext.Value{
		"work.branch": jsontext.Value(`"feature/x"`),
	})
	_ = makeIssueWithMetadata(t, d, p.ID, "nested", map[string]jsontext.Value{
		"work": jsontext.Value(`{"branch":"feature/x"}`),
	})

	// Presence filter: only the flat issue has the top-level "work.branch" key.
	got, err := d.ListIssues(ctx, db.ListIssuesParams{
		ProjectID: p.ID,
		Meta:      []db.MetaFilter{{Key: "work.branch"}},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, flat.ShortID, got[0].ShortID)

	// Equality filter: same — nested must not match.
	got, err = d.ListIssues(ctx, db.ListIssuesParams{
		ProjectID: p.ID,
		Meta:      []db.MetaFilter{{Key: "work.branch", Value: "feature/x", HasValue: true}},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, flat.ShortID, got[0].ShortID)
}

// TestListIssues_MetaFiltersAndTogether pins that multiple meta filters AND,
// and combine with status/label filters.
func TestListIssues_MetaFiltersAndTogether(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	match := makeIssueWithMetadata(t, d, p.ID, "match", map[string]jsontext.Value{
		"work.attention": jsontext.Value(`"stuck"`),
		"work.branch":    jsontext.Value(`"feature/x"`),
	})
	_ = makeIssueWithMetadata(t, d, p.ID, "partial", map[string]jsontext.Value{
		"work.attention": jsontext.Value(`"stuck"`),
	})

	got, err := d.ListIssues(ctx, db.ListIssuesParams{
		ProjectID: p.ID,
		Status:    "open",
		Meta: []db.MetaFilter{
			{Key: "work.attention", Value: "stuck", HasValue: true},
			{Key: "work.branch"},
		},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, match.ShortID, got[0].ShortID)
}
