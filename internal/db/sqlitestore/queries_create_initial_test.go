package sqlitestore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestCreateIssue_WithInitialLabels(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	issue, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
		Labels: []string{"bug", "priority:high", "bug" /* dupe */},
	})
	require.NoError(t, err)
	assert.Equal(t, "issue.created", evt.Type)

	labels, err := d.LabelsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	got := []string{}
	for _, l := range labels {
		got = append(got, l.Label)
	}
	assert.ElementsMatch(t, []string{"bug", "priority:high"}, got, "duplicates deduplicated")

	// Payload includes initial labels (sorted, deduplicated).
	payload := unmarshalPayload[struct {
		Labels []string `json:"labels"`
	}](t, evt.Payload)
	assert.Equal(t, []string{"bug", "priority:high"}, payload.Labels)
}

func TestCreateIssue_WithInitialOwner(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	owner := "alice"
	issue, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
		Owner: &owner,
	})
	require.NoError(t, err)
	require.NotNil(t, issue.Owner)
	assert.Equal(t, "alice", *issue.Owner)

	payload := unmarshalPayload[struct {
		Owner string `json:"owner"`
	}](t, evt.Payload)
	assert.Equal(t, "alice", payload.Owner)
}

func TestCreateIssue_WithInitialLinks(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	parent := makeIssue(t, ctx, d, p.ID, "parent", "tester")
	blocker := makeIssue(t, ctx, d, p.ID, "blocker", "tester")

	child, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "child", Author: "tester",
		Links: []db.InitialLink{
			{Type: "parent", ToNumber: parent.ID},
			{Type: "blocks", ToNumber: blocker.ID},
		},
	})
	require.NoError(t, err)

	// DB state: 2 link rows from child.
	links, err := d.LinksByIssue(ctx, child.ID)
	require.NoError(t, err)
	assert.Len(t, links, 2)

	// Payload references to_number, not to_issue_id.
	payload := unmarshalPayload[struct {
		Links []struct {
			Type     string `json:"type"`
			ToNumber int64  `json:"to_number"`
		} `json:"links"`
	}](t, evt.Payload)
	require.Len(t, payload.Links, 2)
}

func TestCreateIssue_RejectsInitialLinkToMissingTarget(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
		Links: []db.InitialLink{{Type: "parent", ToNumber: 999}},
	})
	assert.True(t, errors.Is(err, db.ErrInitialLinkTargetNotFound),
		"expected ErrInitialLinkTargetNotFound, got %v", err)
}

func TestCreateIssue_RejectsInvalidInitialLinkType(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	target := makeIssue(t, ctx, d, p.ID, "t", "tester")

	_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
		Links: []db.InitialLink{{Type: "child", ToNumber: target.ID}},
	})
	assert.True(t, errors.Is(err, db.ErrInitialLinkInvalidType))
}

func TestCreateIssue_RejectsInvalidLabel(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
		Labels: []string{"BadCase"},
	})
	assert.True(t, errors.Is(err, db.ErrLabelInvalid))
}

func TestCreateIssue_EmitsReplayCompletePayload(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	owner := "alice"
	priority := int64(1)
	issue, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "title",
		Body:      "body",
		Author:    "agent",
		Owner:     &owner,
		Priority:  &priority,
		Labels:    []string{"bug"},
	})
	require.NoError(t, err)

	payload := unmarshalPayload[struct {
		UID       string         `json:"uid"`
		ShortID   string         `json:"short_id"`
		Title     string         `json:"title"`
		Body      string         `json:"body"`
		Author    string         `json:"author"`
		Status    string         `json:"status"`
		Owner     *string        `json:"owner"`
		Priority  *int64         `json:"priority"`
		Labels    []string       `json:"labels"`
		CreatedAt string         `json:"created_at"`
		Metadata  map[string]any `json:"metadata"`
	}](t, evt.Payload)
	assert.Equal(t, issue.UID, payload.UID)
	assert.Equal(t, issue.ShortID, payload.ShortID)
	assert.Equal(t, "title", payload.Title)
	assert.Equal(t, "body", payload.Body)
	assert.Equal(t, "agent", payload.Author)
	assert.Equal(t, "open", payload.Status)
	require.NotNil(t, payload.Owner)
	assert.Equal(t, "alice", *payload.Owner)
	require.NotNil(t, payload.Priority)
	assert.Equal(t, priority, *payload.Priority)
	assert.Equal(t, []string{"bug"}, payload.Labels)
	assert.NotEmpty(t, payload.CreatedAt)
	assert.Empty(t, payload.Metadata)
}

func TestCreateIssue_DuplicateInitialLinksAreDeduped(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	parent := makeIssue(t, ctx, d, p.ID, "parent", "tester")

	child, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "child", Author: "tester",
		Links: []db.InitialLink{
			{Type: "parent", ToNumber: parent.ID},
			{Type: "parent", ToNumber: parent.ID}, // exact dup
		},
	})
	require.NoError(t, err, "duplicate initial links must not roll back")

	// Only one link row inserted.
	links, err := d.LinksByIssue(ctx, child.ID)
	require.NoError(t, err)
	assert.Len(t, links, 1)

	// Payload reflects the deduped list.
	payload := unmarshalPayload[struct {
		Links []struct {
			Type     string `json:"type"`
			ToNumber int64  `json:"to_number"`
		} `json:"links"`
	}](t, evt.Payload)
	assert.Len(t, payload.Links, 1)
}

// TestCreateIssue_RelatedIncomingNormalizes covers a roborev-flagged
// case from kata#1: the API documents Incoming=true on related links as
// a no-op (related is symmetric), but the runtime previously treated
// (related, N, false) and (related, N, true) as distinct entries —
// both targeting the same canonical (smaller-id, larger-id) row, so the
// second insert blew up on the UNIQUE index. This pins that the second
// entry collapses cleanly to a no-op.
func TestCreateIssue_RelatedIncomingNormalizes(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	peer := makeIssue(t, ctx, d, p.ID, "peer", "tester")

	child, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "child", Author: "tester",
		Links: []db.InitialLink{
			{Type: "related", ToNumber: peer.ID, Incoming: false},
			{Type: "related", ToNumber: peer.ID, Incoming: true},
		},
	})
	require.NoError(t, err, "related Incoming=true must collapse to the same link, not error")

	links, err := d.LinksByIssue(ctx, child.ID)
	require.NoError(t, err)
	assert.Len(t, links, 1, "exactly one related link should be persisted")

	payload := unmarshalPayload[struct {
		Links []struct {
			Type     string `json:"type"`
			ToNumber int64  `json:"to_number"`
			Incoming bool   `json:"incoming,omitempty"`
		} `json:"links"`
	}](t, evt.Payload)
	require.Len(t, payload.Links, 1)
	assert.Equal(t, "related", payload.Links[0].Type)
	assert.False(t, payload.Links[0].Incoming, "related entries must report incoming=false in the payload")
}

func TestCreateIssue_EmptyStringOwnerNormalizesToNil(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	empty := ""
	issue, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "x", Author: "tester",
		Owner: &empty,
	})
	require.NoError(t, err)
	assert.Nil(t, issue.Owner, "empty-string owner must persist as NULL")
	payload := unmarshalPayload[map[string]any](t, evt.Payload)
	assert.NotContains(t, payload, "owner", "payload must agree: no owner")
}

func TestCreateIssue_WithAllInitialState(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	parent := makeIssue(t, ctx, d, p.ID, "parent", "tester")

	owner := "alice"
	issue, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "child", Author: "tester",
		Labels: []string{"bug", "priority:high"},
		Links:  []db.InitialLink{{Type: "parent", ToNumber: parent.ID}},
		Owner:  &owner,
	})
	require.NoError(t, err)

	// Issue carries the owner.
	require.NotNil(t, issue.Owner)
	assert.Equal(t, "alice", *issue.Owner)

	// DB has 2 labels + 1 link.
	labels, err := d.LabelsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	assert.Len(t, labels, 2)
	links, err := d.LinksByIssue(ctx, issue.ID)
	require.NoError(t, err)
	assert.Len(t, links, 1)

	// Payload union: labels, links, and owner all present.
	payload := unmarshalPayload[struct {
		Labels []string `json:"labels"`
		Links  []struct {
			Type      string `json:"type"`
			ToShortID string `json:"to_short_id"`
		} `json:"links"`
		Owner string `json:"owner"`
	}](t, evt.Payload)
	assert.Equal(t, []string{"bug", "priority:high"}, payload.Labels)
	require.Len(t, payload.Links, 1)
	assert.Equal(t, "parent", payload.Links[0].Type)
	assert.Equal(t, parent.ShortID, payload.Links[0].ToShortID)
	assert.Equal(t, "alice", payload.Owner)
}

// Auto-extend tests use ULIDs whose suffixes are constructed to collide at
// specific lengths, exercising the loop in assignShortID. The injected UIDs
// flow through the optional CreateIssueParams.UID field; live callers leave
// UID empty and let CreateIssue call uid.New() instead.
//
// Suffix table for the chosen ULIDs:
//
//	01HZNQ7VFPK1XGD8R5MABCD4EX -> last4=d4ex,  last5=cd4ex,  last6=bcd4ex
//	01HZNQ7VFPK1XGD8R5MABXD4EX -> last4=d4ex,  last5=xd4ex,  last6=bxd4ex
//	01HZNQ7VFPK1XGD8R5AYBXD4EX -> last4=d4ex,  last5=xd4ex,  last6=bxd4ex
func TestCreateIssue_AssignsLength4WhenUnique(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "demo")
	row, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "first",
		Author:    "tester",
	})
	require.NoError(t, err)
	assert.Equal(t, "d4ex", row.ShortID)
}

func TestCreateIssue_ExtendsToLength5OnCollision(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "demo")
	_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "first",
		Author:    "tester",
	})
	require.NoError(t, err)
	// Different ULID with the same last 4 chars (D4EX), forcing extension.
	row2, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABXD4EX",
		Title:     "second",
		Author:    "tester",
	})
	require.NoError(t, err)
	assert.Equal(t, "xd4ex", row2.ShortID)
}

func TestCreateIssue_ExtendsToLength6OnDoubleCollision(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "demo")
	uids := []string{
		"01HZNQ7VFPK1XGD8R5MABCD4EX",
		"01HZNQ7VFPK1XGD8R5MABXD4EX",
		"01HZNQ7VFPK1XGD8R5AYBXD4EX",
	}
	for i, u := range uids {
		_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: p.ID,
			UID:       u,
			Title:     "i" + string(rune('0'+i)),
			Author:    "tester",
		})
		require.NoError(t, err, "create %s", u)
	}
	rows, err := d.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID})
	require.NoError(t, err)
	require.Len(t, rows, 3)
	// ListIssues orders by updated_at DESC, so the most recent insert comes
	// first. Index the assertions by UID instead.
	got := map[string]string{}
	for _, r := range rows {
		got[r.UID] = r.ShortID
	}
	assert.Equal(t, "d4ex", got["01HZNQ7VFPK1XGD8R5MABCD4EX"])
	assert.Equal(t, "xd4ex", got["01HZNQ7VFPK1XGD8R5MABXD4EX"])
	assert.Equal(t, "bxd4ex", got["01HZNQ7VFPK1XGD8R5AYBXD4EX"])
}
