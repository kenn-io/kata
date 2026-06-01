package sqlitestore_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestAddLabel_RoundTrips(t *testing.T) {
	d, ctx, _, i := setupTestIssue(t)

	row, err := d.AddLabel(ctx, i.ID, "needs-review", "tester")
	require.NoError(t, err)
	assert.Equal(t, "needs-review", row.Label)
	assert.Equal(t, i.ID, row.IssueID)

	got, err := d.LabelsByIssue(ctx, i.ID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "needs-review", got[0].Label)
}

func TestAddLabel_DuplicateIsErrLabelExists(t *testing.T) {
	d, ctx, _, i := setupTestIssue(t)

	_, err := d.AddLabel(ctx, i.ID, "bug", "tester")
	require.NoError(t, err)
	_, err = d.AddLabel(ctx, i.ID, "bug", "tester")
	assert.True(t, errors.Is(err, db.ErrLabelExists), "got %v", err)
}

func TestAddLabel_RejectsBadCharset(t *testing.T) {
	d, ctx, _, i := setupTestIssue(t)

	for _, label := range []string{"UPPER", "with space", "emoji😀", "" /* empty */, "exclam!"} {
		_, err := d.AddLabel(ctx, i.ID, label, "tester")
		assert.Truef(t, errors.Is(err, db.ErrLabelInvalid),
			"label=%q: expected ErrLabelInvalid, got %v", label, err)
	}
}

func TestAddLabel_AcceptsAllAllowedChars(t *testing.T) {
	d, ctx, _, i := setupTestIssue(t)

	for _, label := range []string{"bug", "priority:high", "v1.0", "needs-review", "a-z_0-9"} {
		_, err := d.AddLabel(ctx, i.ID, label, "tester")
		assert.NoErrorf(t, err, "label=%q must be accepted", label)
	}
}

func TestRemoveLabel_RoundTrips(t *testing.T) {
	d, ctx, _, i := setupTestIssue(t)
	_, err := d.AddLabel(ctx, i.ID, "bug", "tester")
	require.NoError(t, err)

	require.NoError(t, d.RemoveLabel(ctx, i.ID, "bug"))

	had, err := d.HasLabel(ctx, i.ID, "bug")
	require.NoError(t, err)
	assert.False(t, had)

	// Idempotent — removing an absent label returns ErrNotFound.
	err = d.RemoveLabel(ctx, i.ID, "bug")
	assert.True(t, errors.Is(err, db.ErrNotFound))
}

func TestAddLabel_BlankAuthorIsNotMisreportedAsInvalidLabel(t *testing.T) {
	d, ctx, _, i := setupTestIssue(t)

	_, err := d.AddLabel(ctx, i.ID, "bug", "" /* blank */)
	require.Error(t, err)
	assert.False(t, errors.Is(err, db.ErrLabelInvalid),
		"blank author must not surface as ErrLabelInvalid, got %v", err)
}

func TestLabelByEndpoints_FindsExisting(t *testing.T) {
	d, ctx, _, i := setupTestIssue(t)

	created, err := d.AddLabel(ctx, i.ID, "bug", "tester")
	require.NoError(t, err)

	got, err := d.LabelByEndpoints(ctx, i.ID, "bug")
	require.NoError(t, err)
	assert.Equal(t, created.IssueID, got.IssueID)
	assert.Equal(t, created.Label, got.Label)

	_, err = d.LabelByEndpoints(ctx, i.ID, "never-attached")
	assert.True(t, errors.Is(err, db.ErrNotFound))
}

func TestLabelCounts_AggregatesPerProject(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a := makeIssue(t, ctx, d, p.ID, "a", "tester")
	b := makeIssue(t, ctx, d, p.ID, "b", "tester")
	addLabels(ctx, t, d, a.ID, "tester", "bug", "priority:high")
	addLabels(ctx, t, d, b.ID, "tester", "bug")

	counts, err := d.LabelCounts(ctx, p.ID)
	require.NoError(t, err)
	got := map[string]int64{}
	for _, c := range counts {
		got[c.Label] = c.Count
	}
	assert.Equal(t, int64(2), got["bug"])
	assert.Equal(t, int64(1), got["priority:high"])
}

func TestLabelEventsCarryIssueUID(t *testing.T) {
	d, ctx, _, i := setupTestIssue(t)

	_, labeled, err := d.AddLabelAndEvent(ctx, i.ID, db.LabelEventParams{
		EventType: "issue.labeled",
		Label:     "bug",
		Actor:     "tester",
	})
	require.NoError(t, err)
	labeledPayload := unmarshalPayload[struct {
		IssueUID string `json:"issue_uid"`
		Label    string `json:"label"`
	}](t, labeled.Payload)
	assert.Equal(t, i.UID, labeledPayload.IssueUID)
	assert.Equal(t, "bug", labeledPayload.Label)

	unlabeled, err := d.RemoveLabelAndEvent(ctx, i.ID, db.LabelEventParams{
		EventType: "issue.unlabeled",
		Label:     "bug",
		Actor:     "tester",
	})
	require.NoError(t, err)
	unlabeledPayload := unmarshalPayload[struct {
		IssueUID string `json:"issue_uid"`
		Label    string `json:"label"`
	}](t, unlabeled.Payload)
	assert.Equal(t, i.UID, unlabeledPayload.IssueUID)
	assert.Equal(t, "bug", unlabeledPayload.Label)
}
