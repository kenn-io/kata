package sqlitestore_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

const updatedAtLayout = "2006-01-02T15:04:05.000Z"

// assertEventCarriesUpdatedAt checks that an issue-mutation event records the
// updated_at it wrote to the issue row, so replay reproduces the directly
// written timestamp instead of falling back to the event's created_at.
func assertEventCarriesUpdatedAt(t *testing.T, evt db.Event, issue db.Issue) {
	t.Helper()
	var p map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(evt.Payload), &p))
	raw, ok := p["updated_at"]
	require.Truef(t, ok, "%s payload missing updated_at: %s", evt.Type, evt.Payload)
	var got string
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equalf(t, issue.UpdatedAt.UTC().Format(updatedAtLayout), got,
		"%s payload updated_at must equal the issue row's updated_at", evt.Type)
}

func issueByID(ctx context.Context, t *testing.T, d *sqlitestore.Store, id int64) db.Issue {
	t.Helper()
	issue, err := d.IssueByID(ctx, id)
	require.NoError(t, err)
	return issue
}

// TestIssueMutationsCarryUpdatedAt asserts every issue-mutation event records
// the issue's resulting updated_at in its payload. Without it the fold falls
// back to the event's created_at, a separate clock read, so a replayed
// projection can diverge from the directly written row.
func TestIssueMutationsCarryUpdatedAt(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "a", Body: "body", Author: "agent",
	})
	require.NoError(t, err)
	b, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "b", Author: "agent",
	})
	require.NoError(t, err)

	title := "edited"
	edited, evt, changed, err := d.EditIssue(ctx, db.EditIssueParams{IssueID: a.ID, Title: &title, Actor: "agent"})
	require.NoError(t, err)
	require.True(t, changed)
	assertEventCarriesUpdatedAt(t, *evt, edited)

	owner := "alice"
	assigned, evt, changed, err := d.UpdateOwner(ctx, a.ID, &owner, "agent")
	require.NoError(t, err)
	require.True(t, changed)
	assertEventCarriesUpdatedAt(t, *evt, assigned)

	unassigned, evt, changed, err := d.UpdateOwner(ctx, a.ID, nil, "agent")
	require.NoError(t, err)
	require.True(t, changed)
	assertEventCarriesUpdatedAt(t, *evt, unassigned)

	prio := int64(2)
	prioritized, evt, changed, err := d.UpdatePriority(ctx, a.ID, &prio, "agent")
	require.NoError(t, err)
	require.True(t, changed)
	assertEventCarriesUpdatedAt(t, *evt, prioritized)

	cleared, evt, changed, err := d.UpdatePriority(ctx, a.ID, nil, "agent")
	require.NoError(t, err)
	require.True(t, changed)
	assertEventCarriesUpdatedAt(t, *evt, cleared)

	_, labelEvt, err := d.AddLabelAndEvent(ctx, a.ID, db.LabelEventParams{
		EventType: "issue.labeled", Label: "bug", Actor: "agent",
	})
	require.NoError(t, err)
	assertEventCarriesUpdatedAt(t, labelEvt, issueByID(ctx, t, d, a.ID))

	unlabelEvt, err := d.RemoveLabelAndEvent(ctx, a.ID, db.LabelEventParams{
		EventType: "issue.unlabeled", Label: "bug", Actor: "agent",
	})
	require.NoError(t, err)
	assertEventCarriesUpdatedAt(t, unlabelEvt, issueByID(ctx, t, d, a.ID))

	link, linkEvt, err := d.CreateLinkAndEvent(ctx,
		db.CreateLinkParams{ProjectID: p.ID, FromIssueID: a.ID, ToIssueID: b.ID, Type: "blocks", Author: "agent"},
		db.LinkEventParams{
			EventType: "issue.linked", EventIssueID: a.ID,
			FromShortID: a.ShortID, FromUID: a.UID, ToShortID: b.ShortID, ToUID: b.UID, Actor: "agent",
		})
	require.NoError(t, err)
	assertEventCarriesUpdatedAt(t, linkEvt, issueByID(ctx, t, d, a.ID))

	unlinkEvt, err := d.DeleteLinkAndEvent(ctx, link, db.LinkEventParams{
		EventType: "issue.unlinked", EventIssueID: a.ID,
		FromShortID: a.ShortID, FromUID: a.UID, ToShortID: b.ShortID, ToUID: b.UID, Actor: "agent",
	})
	require.NoError(t, err)
	assertEventCarriesUpdatedAt(t, unlinkEvt, issueByID(ctx, t, d, a.ID))

	metaOut, err := d.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: a.ID, IfMatchRev: 1, Actor: "agent",
		Patch: map[string]json.RawMessage{"area": json.RawMessage(`"api"`)},
	})
	require.NoError(t, err)
	require.True(t, metaOut.Changed)
	assertEventCarriesUpdatedAt(t, metaOut.Event, metaOut.Issue)

	_, _, changed, err = d.CloseIssue(ctx, a.ID, "done", "agent", "", nil)
	require.NoError(t, err)
	require.True(t, changed)
	reopened, evt, changed, err := d.ReopenIssue(ctx, a.ID, "agent")
	require.NoError(t, err)
	require.True(t, changed)
	assertEventCarriesUpdatedAt(t, *evt, reopened)

	_, _, changed, err = d.SoftDeleteIssue(ctx, a.ID, "agent")
	require.NoError(t, err)
	require.True(t, changed)
	restored, evt, changed, err := d.RestoreIssue(ctx, a.ID, "agent")
	require.NoError(t, err)
	require.True(t, changed)
	assertEventCarriesUpdatedAt(t, *evt, restored)

	claim, err := d.ClaimOwner(ctx, b.ID, "carol", false)
	require.NoError(t, err)
	require.True(t, claim.Changed)
	assertEventCarriesUpdatedAt(t, *claim.Event, claim.Issue)

	atomicTitle := "b edited"
	atomic, err := d.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: b.ID, Actor: "agent", Title: &atomicTitle, SetPriority: &prio, AddBlocks: []int64{a.ID},
	})
	require.NoError(t, err)
	require.True(t, atomic.AnyChange)
	for _, e := range atomic.Events {
		assertEventCarriesUpdatedAt(t, e, atomic.Issue)
	}
}

func TestMoveIssueCarriesUpdatedAt(t *testing.T) {
	d, ctx, src := setupTestProject(t)
	tgt, err := d.CreateProject(ctx, "target")
	require.NoError(t, err)
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: src.ID, Title: "movable", Author: "agent",
	})
	require.NoError(t, err)

	out, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: issue.ID, FromProjectID: src.ID, ToProjectID: tgt.ID, IfMatchRev: 1, Actor: "agent",
	})
	require.NoError(t, err)

	events, err := d.EventsAfter(ctx, db.EventsAfterParams{AfterID: 0, ProjectID: tgt.ID, Limit: 1000})
	require.NoError(t, err)
	var moved *db.Event
	for i := range events {
		if events[i].Type == "issue.moved" {
			moved = &events[i]
		}
	}
	require.NotNil(t, moved, "issue.moved event not found")
	assertEventCarriesUpdatedAt(t, *moved, out.Issue)
}
