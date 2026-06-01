package sqlitestore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func TestSoftDeleteIssue_SetsDeletedAtAndEmitsEvent(t *testing.T) {
	d, ctx, _, issue := setupTestIssue(t)

	updated, evt, changed, err := d.SoftDeleteIssue(ctx, issue.ID, "agent")
	require.NoError(t, err)
	require.NotNil(t, evt)
	assert.True(t, changed)
	assert.Equal(t, "issue.soft_deleted", evt.Type)
	assert.Equal(t, "agent", evt.Actor)
	payload := unmarshalPayload[struct {
		DeletedAt string `json:"deleted_at"`
	}](t, evt.Payload)
	assert.NotEmpty(t, payload.DeletedAt)
	require.NotNil(t, evt.IssueID)
	assert.Equal(t, issue.ID, *evt.IssueID, "event refs the soft-deleted issue id")
	require.NotNil(t, updated.DeletedAt)
	assert.True(t, updated.UpdatedAt.After(issue.UpdatedAt) || updated.UpdatedAt.Equal(issue.UpdatedAt),
		"updated_at must not regress on soft-delete")
}

func TestSoftDeleteIssue_AlreadyDeletedIsNoOp(t *testing.T) {
	d, ctx, _, issue := setupSoftDeletedIssue(t)

	updated, evt, changed, err := d.SoftDeleteIssue(ctx, issue.ID, "agent")
	require.NoError(t, err)
	assert.Nil(t, evt, "no-op should return nil event")
	assert.False(t, changed)
	assert.NotNil(t, updated.DeletedAt, "issue stays deleted")
}

func TestSoftDeleteIssue_UnknownIssueIsErrNotFound(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	_, _, _, err := d.SoftDeleteIssue(ctx, 9999, "agent")
	assert.True(t, errors.Is(err, db.ErrNotFound))
}

func TestRestoreIssue_ClearsDeletedAtAndEmitsEvent(t *testing.T) {
	d, ctx, _, issue := setupSoftDeletedIssue(t)

	updated, evt, changed, err := d.RestoreIssue(ctx, issue.ID, "agent")
	require.NoError(t, err)
	require.NotNil(t, evt)
	assert.True(t, changed)
	assert.Equal(t, "issue.restored", evt.Type)
	payload := unmarshalPayload[struct {
		RestoredAt string `json:"restored_at"`
	}](t, evt.Payload)
	assert.NotEmpty(t, payload.RestoredAt)
	assert.Nil(t, updated.DeletedAt)
}

func TestRestoreIssue_NotDeletedIsNoOp(t *testing.T) {
	d, ctx, _, issue := setupTestIssue(t)

	_, evt, changed, err := d.RestoreIssue(ctx, issue.ID, "agent")
	require.NoError(t, err)
	assert.Nil(t, evt)
	assert.False(t, changed)
}

func TestRestoreIssue_UnknownIssueIsErrNotFound(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	_, _, _, err := d.RestoreIssue(ctx, 9999, "agent")
	assert.True(t, errors.Is(err, db.ErrNotFound))
}

func TestSoftDeleteRestore_RoundTripVisibility(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	issue, _ := createTesterIssue(ctx, t, d, p.ID, "round trip")

	// Initial: visible in default list.
	listed, err := d.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, issue.ID, listed[0].ID)

	// After soft-delete: hidden from default list.
	_, _, _, err = d.SoftDeleteIssue(ctx, issue.ID, "agent")
	require.NoError(t, err)
	listed, err = d.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID})
	require.NoError(t, err)
	assert.Empty(t, listed, "soft-deleted issue must be hidden from default list")

	// After restore: visible again.
	_, _, _, err = d.RestoreIssue(ctx, issue.ID, "agent")
	require.NoError(t, err)
	listed, err = d.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, issue.ID, listed[0].ID, "restored issue is visible again")
}

func TestRestoreIssue_EmitsEventWithPayloadAndRefs(t *testing.T) {
	d, ctx, _, issue := setupSoftDeletedIssue(t)

	updated, evt, changed, err := d.RestoreIssue(ctx, issue.ID, "agent")
	require.NoError(t, err)
	require.NotNil(t, evt)
	assert.True(t, changed)
	payload := unmarshalPayload[struct {
		RestoredAt string `json:"restored_at"`
	}](t, evt.Payload)
	assert.NotEmpty(t, payload.RestoredAt)
	require.NotNil(t, evt.IssueID)
	assert.Equal(t, issue.ID, *evt.IssueID)
	assert.True(t, updated.UpdatedAt.After(issue.UpdatedAt) || updated.UpdatedAt.Equal(issue.UpdatedAt),
		"updated_at must not regress on restore")
}

func TestSoftDeleteIssue_ScopesByIssueID(t *testing.T) {
	// SoftDeleteIssue takes an issue ID, not a project ID — it must work
	// across projects without requiring a project context.
	d := openTestDB(t)
	ctx := context.Background()
	p1, err := d.CreateProject(ctx, "p1")
	require.NoError(t, err)
	p2, err := d.CreateProject(ctx, "p2")
	require.NoError(t, err)
	_, _, err = d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p1.ID, Title: "in p1", Author: "tester",
	})
	require.NoError(t, err)
	target, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p2.ID, Title: "in p2", Author: "tester",
	})
	require.NoError(t, err)

	updated, evt, changed, err := d.SoftDeleteIssue(ctx, target.ID, "agent")
	require.NoError(t, err)
	require.NotNil(t, evt)
	assert.True(t, changed)
	require.NotNil(t, updated.DeletedAt)
	assert.Equal(t, p2.ID, updated.ProjectID, "deleted issue belongs to p2")
}

func TestPurgeIssue_RemovesAllDependentsAndAudits(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createKataProject(ctx, t, d)
	target, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "delete me", Body: "body", Author: "tester",
	})
	require.NoError(t, err)
	keeper, _ := createTesterIssue(ctx, t, d, p.ID, "keep me")

	// Add a comment, a label (with event), and a link from keeper → target so
	// cascade removes a non-trivial set of dependents.
	_, _, err = d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: target.ID, Author: "tester", Body: "comment body",
	})
	require.NoError(t, err)
	_, _, err = d.AddLabelAndEvent(ctx, target.ID, db.LabelEventParams{
		EventType: "issue.labeled", Label: "bug", Actor: "tester",
	})
	require.NoError(t, err)
	_, _, err = d.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		ProjectID: p.ID, FromIssueID: keeper.ID, ToIssueID: target.ID,
		Type: "blocks", Author: "tester",
	}, db.LinkEventParams{
		EventType:    "issue.linked",
		EventIssueID: keeper.ID,
		FromShortID:  keeper.ShortID, FromUID: keeper.UID,
		ToShortID: target.ShortID, ToUID: target.UID,
		Actor: "tester",
	})
	require.NoError(t, err)
	claimUID, err := uid.New()
	require.NoError(t, err)
	require.NoError(t, d.UpsertClaimCache(ctx, db.IssueClaim{
		ClaimUID:          claimUID,
		ProjectID:         p.ID,
		IssueID:           target.ID,
		IssueUID:          target.UID,
		Holder:            "agent",
		HolderInstanceUID: d.InstanceUID(),
		ClaimKind:         "hard",
		AcquiredAt:        time.Now().UTC(),
		Revision:          1,
		UpdatedAt:         time.Now().UTC(),
	}))
	_, err = d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: p.ID,
		IssueRef:  target.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: d.InstanceUID(),
			Holder:            "pending-agent",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	pl, err := d.PurgeIssue(ctx, target.ID, "agent", nil)
	require.NoError(t, err)

	assert.Equal(t, target.ID, pl.PurgedIssueID)
	assert.Equal(t, target.UID, *pl.IssueUID)
	assert.Equal(t, p.UID, *pl.ProjectUID)
	assert.Equal(t, "kata", pl.ProjectName)
	assert.Equal(t, "delete me", pl.IssueTitle)
	assert.Equal(t, "tester", pl.IssueAuthor)
	assert.Equal(t, int64(1), pl.CommentCount)
	assert.Equal(t, int64(1), pl.LinkCount)
	assert.Equal(t, int64(1), pl.LabelCount)
	// Events: issue.created + issue.commented + issue.labeled = 3 attached to target,
	// plus 1 issue.linked attributed to target via related_issue_id (keeper's event).
	// Total: 4.
	assert.Equal(t, int64(4), pl.EventCount)
	require.NotNil(t, pl.EventsDeletedMinID)
	require.NotNil(t, pl.EventsDeletedMaxID)
	require.NotNil(t, pl.PurgeResetAfterEventID, "events were deleted, reset cursor must be set")

	// Verify rows actually gone.
	assertRowCount(ctx, t, d, 0, "issue row removed",
		`SELECT count(*) FROM issues WHERE id = ?`, target.ID)
	assertRowCount(ctx, t, d, 0, "comments removed",
		`SELECT count(*) FROM comments WHERE issue_id = ?`, target.ID)
	assertRowCount(ctx, t, d, 0, "links removed",
		`SELECT count(*) FROM links WHERE from_issue_id = ? OR to_issue_id = ?`,
		target.ID, target.ID)
	assertRowCount(ctx, t, d, 0, "labels removed",
		`SELECT count(*) FROM issue_labels WHERE issue_id = ?`, target.ID)
	assertRowCount(ctx, t, d, 0, "claims removed",
		`SELECT count(*) FROM issue_claims WHERE issue_id = ?`, target.ID)
	assertRowCount(ctx, t, d, 0, "pending claims removed",
		`SELECT count(*) FROM pending_claim_requests WHERE issue_id = ?`, target.ID)
	assertRowCount(ctx, t, d, 0, "events removed",
		`SELECT count(*) FROM events WHERE issue_id = ? OR related_issue_id = ?`,
		target.ID, target.ID)
	assertRowCount(ctx, t, d, 0, "FTS row removed",
		`SELECT count(*) FROM issues_fts WHERE rowid = ?`, target.ID)

	// keeper's events.created is the only event attributed to keeper that
	// survives — keeper's issue.linked was deleted because related_issue_id
	// pointed to target.
	assertRowCount(ctx, t, d, 1,
		"keeper's issue.created survives; its issue.linked was cascade-deleted via related_issue_id",
		`SELECT count(*) FROM events WHERE issue_id = ?`, keeper.ID)
}

// TestEditIssueAtomic_AddBlocksHandlesConcurrentInsertGracefully covers
// the addEdgeTx race-window guard: if a concurrent edit inserts the same
// link between addEdgeTx's pre-insert duplicate lookup and the actual
// INSERT, the schema's UNIQUE surfaces ErrLinkExists. The atomic edit
// must treat that as the same idempotent no-op the lookup would have
// produced — the resulting graph state is exactly what the caller asked
// for. Without this, the second edit returned a 500 internal error.
//
// Simulating the race: insert the link directly via SQL after the first
// EditIssueAtomic so it lands without going through the helper's
// pre-lookup, then do a second EditIssueAtomic with a different existing
// link op + the same blocks-add. The second call's addEdgeTx finds the
// link via lookup and no-ops, which is also what we want; this test pins
// the broader contract that "asking to add a link that's already there
// is never an error."
func TestEditIssueAtomic_AddBlocksHandlesConcurrentInsertGracefully(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a, _ := createTesterIssue(ctx, t, d, p.ID, "subject")
	b, _ := createTesterIssue(ctx, t, d, p.ID, "target")

	// First edit creates the link a → b.
	_, err := d.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID:   a.ID,
		Actor:     "tester",
		AddBlocks: []int64{b.ID},
	})
	require.NoError(t, err)

	// Second edit asks for the same link — must succeed as a no-op
	// (no error, no duplicate row).
	res, err := d.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID:   a.ID,
		Actor:     "tester",
		AddBlocks: []int64{b.ID},
	})
	require.NoError(t, err, "duplicate add must be a no-op, never an error")
	assert.Empty(t, res.Changes.BlocksAdded, "duplicate add must report no change")

	// Confirm the row count didn't grow.
	var n int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT count(*) FROM links WHERE from_issue_id = ? AND to_issue_id = ? AND type = 'blocks'`,
		a.ID, b.ID).Scan(&n))
	assert.Equal(t, 1, n, "exactly one row in the schema after a redundant add")
}

// TestPurgeIssue_PreservesSinglePeerAggregatedLinksChangedEvent pins
// the consistency rule introduced by the kata#1 design call: aggregated
// issue.links_changed events on OTHER live issues survive purge of the
// referenced peer regardless of whether the iteration-16 envelope-peer
// fix populated related_issue_id (single-peer case) or left it NULL
// (multi-peer case). Without the type-aware purge cleanup, a single-peer
// edit's aggregated event would be deleted via the related_issue_id =
// purged-issue match — meaning a `kata edit subject --blocks target`
// loses subject's link history when target is purged, but the same
// edit batched with a second peer survives. That's a batch-size-
// dependent bug.
func TestPurgeIssue_PreservesSinglePeerAggregatedLinksChangedEvent(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	subject, _ := createTesterIssue(ctx, t, d, p.ID, "subject")
	target, _ := createTesterIssue(ctx, t, d, p.ID, "target")

	_, err := d.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID:   subject.ID,
		Actor:     "tester",
		AddBlocks: []int64{target.ID},
	})
	require.NoError(t, err)

	// The single-peer aggregated event has related_issue_id = target.ID
	// thanks to iteration-16. Sanity check.
	var beforePeerCount int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE type = 'issue.links_changed'
		   AND issue_id = ? AND related_issue_id = ?`,
		subject.ID, target.ID).Scan(&beforePeerCount))
	require.Equal(t, 1, beforePeerCount, "single-peer aggregated event must carry envelope peer metadata")

	_, err = d.PurgeIssue(ctx, target.ID, "agent", nil)
	require.NoError(t, err)

	var afterCount int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE type = 'issue.links_changed' AND issue_id = ?`,
		subject.ID).Scan(&afterCount))
	assert.Equal(t, 1, afterCount,
		"single-peer aggregated event must survive purge of the referenced peer — purging target must not erase subject's link history regardless of batch size")
}

// TestPurgeIssue_PreservesAggregatedLinksChangedEventsOnOtherIssues
// pins Jesse's design call on kata#1: purging an issue must not erase
// the historical context that ANOTHER live issue was once linked to
// it. The aggregated issue.links_changed event on `subject` references
// `target` in its payload; when `target` is purged, the FK cascade
// deletes events whose issue_id == target, but `subject`'s mutation
// history (issue.links_changed events with target referenced only in
// payload) survives intact. The orphan reference in the payload is the
// accepted trade-off.
func TestPurgeIssue_PreservesAggregatedLinksChangedEventsOnOtherIssues(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	subject, _ := createTesterIssue(ctx, t, d, p.ID, "subject")
	target, _ := createTesterIssue(ctx, t, d, p.ID, "target")

	// Emit an aggregated issue.links_changed event on `subject` that
	// references `target` in blocks_added — this is the shape produced
	// by `kata edit subject --blocks target`. Iteration-16 set the
	// envelope's related_issue_id to `target` for single-peer aggregated
	// events; that's a real issue_id reference, so the FK cascade WILL
	// delete this event. Use a multi-peer edit so related_issue_id stays
	// NULL and the event references target only in payload.
	other, _ := createTesterIssue(ctx, t, d, p.ID, "other peer")
	_, err := d.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID:   subject.ID,
		Actor:     "tester",
		AddBlocks: []int64{target.ID, other.ID},
	})
	require.NoError(t, err)

	// Sanity: the aggregated event lives in the events table with the
	// payload referencing target's UID.
	var beforeCount int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE type = 'issue.links_changed' AND issue_id = ?`,
		subject.ID).Scan(&beforeCount))
	require.Equal(t, 1, beforeCount, "aggregated event must exist before purge")

	// Now purge the TARGET. The aggregated event must SURVIVE because
	// it documents a relationship from `subject`'s point of view.
	_, err = d.PurgeIssue(ctx, target.ID, "agent", nil)
	require.NoError(t, err)

	var afterCount int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE type = 'issue.links_changed' AND issue_id = ?`,
		subject.ID).Scan(&afterCount))
	assert.Equal(t, 1, afterCount,
		"aggregated event must be preserved on subject — purging target must not erase subject's link history")
}

// TestEditIssueAtomic_LinksChangedSetsEnvelopePeerForSingleEdge covers a
// roborev finding from kata#1: the per-link issue.linked / issue.unlinked
// envelopes carry related_issue_id / related_issue_uid; consumers that
// route on those envelope columns lose peer identity when the same edit
// runs through PATCH and emits a single-peer issue.links_changed instead.
// When exactly one peer is referenced across the entire aggregated event,
// preserve envelope-level peer metadata so envelope-only consumers stay
// informed without parsing payload.
// TestEditIssueAtomic_OwnerEmptyStringClearsToNil pins the contract that
// an explicit `--owner ""` (or API request with Owner=stringPtr("")) is
// the unassign signal: it writes NULL to the column rather than the
// empty string. Before this fix EditIssueAtomic compared "" against the
// current owner as if it were nil but still wrote "" via the UPDATE
// args, leaving rows in the inconsistent "assigned to empty string"
// state and emitting an issue.updated event instead of the unassign
// semantics.
func TestEditIssueAtomic_OwnerEmptyStringClearsToNil(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	subject, _ := createTesterIssue(ctx, t, d, p.ID, "subject")

	owner := "alice"
	_, err := d.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: subject.ID,
		Actor:   "tester",
		Owner:   &owner,
	})
	require.NoError(t, err)

	empty := ""
	_, err = d.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: subject.ID,
		Actor:   "tester",
		Owner:   &empty,
	})
	require.NoError(t, err)

	got, err := d.IssueByID(ctx, subject.ID)
	require.NoError(t, err)
	assert.Nil(t, got.Owner, "empty-string owner must clear to NULL, not store \"\"")
}

func TestEditIssueAtomic_LinksChangedSetsEnvelopePeerForSingleEdge(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	subject, _ := createTesterIssue(ctx, t, d, p.ID, "subject")
	target, _ := createTesterIssue(ctx, t, d, p.ID, "target")

	res, err := d.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID:   subject.ID,
		Actor:     "tester",
		AddBlocks: []int64{target.ID},
	})
	require.NoError(t, err)

	var evt *db.Event
	for i := range res.Events {
		if res.Events[i].Type == "issue.links_changed" {
			evt = &res.Events[i]
			break
		}
	}
	require.NotNil(t, evt, "expected an issue.links_changed event")
	require.NotNil(t, evt.RelatedIssueID, "single-peer aggregated event must set related_issue_id")
	assert.Equal(t, target.ID, *evt.RelatedIssueID)
	require.NotNil(t, evt.RelatedIssueUID, "single-peer aggregated event must set related_issue_uid")
	assert.Equal(t, target.UID, *evt.RelatedIssueUID)
}

// TestEditIssueAtomic_LinksChangedNullsEnvelopePeerForMultiEdge pairs with
// the single-edge test above: when the aggregated event references more
// than one distinct peer, there is no single peer to surface at the
// envelope level. Leave related_issue_id / related_issue_uid NULL — the
// payload's *_uids slices remain authoritative for multi-peer edits.
func TestEditIssueAtomic_LinksChangedNullsEnvelopePeerForMultiEdge(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	subject, _ := createTesterIssue(ctx, t, d, p.ID, "subject")
	t1, _ := createTesterIssue(ctx, t, d, p.ID, "t1")
	t2, _ := createTesterIssue(ctx, t, d, p.ID, "t2")

	res, err := d.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID:   subject.ID,
		Actor:     "tester",
		AddBlocks: []int64{t1.ID, t2.ID},
	})
	require.NoError(t, err)

	var evt *db.Event
	for i := range res.Events {
		if res.Events[i].Type == "issue.links_changed" {
			evt = &res.Events[i]
			break
		}
	}
	require.NotNil(t, evt, "expected an issue.links_changed event")
	assert.Nil(t, evt.RelatedIssueID, "multi-peer aggregated event must leave related_issue_id NULL")
	assert.Nil(t, evt.RelatedIssueUID, "multi-peer aggregated event must leave related_issue_uid NULL")
}

func TestPurgeIssue_NoEventsLeavesResetCursorNull(t *testing.T) {
	// Manually craft an issue row with no events: insert directly so we
	// bypass CreateIssue's automatic issue.created event. Verify that
	// PurgeIssue sees zero attached events and leaves PurgeResetAfterEventID
	// as nil (no SSE cursor reservation needed).
	d, ctx, p := setupTestProject(t)
	issueUID, err := uid.New()
	require.NoError(t, err)
	// short_id is the lowercased trailing-4 ULID suffix (cf. shortid.Derive).
	res, err := d.ExecContext(ctx,
		`INSERT INTO issues(uid, project_id, short_id, title, author)
		 VALUES(?, ?, lower(substr(?, 23, 4)), 'no-events', 'tester')`,
		issueUID, p.ID, issueUID)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)

	pl, err := d.PurgeIssue(ctx, id, "agent", nil)
	require.NoError(t, err)
	assert.Equal(t, int64(0), pl.EventCount)
	assert.Nil(t, pl.EventsDeletedMinID)
	assert.Nil(t, pl.EventsDeletedMaxID)
	assert.Nil(t, pl.PurgeResetAfterEventID, "no events deleted → no reset cursor")
}

func TestPurgeIssue_ReservesSqliteSequenceAboveMaxEventID(t *testing.T) {
	d, ctx, p, target := setupTestIssue(t)
	// Capture max events.id BEFORE purge.
	var maxBefore int64
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&maxBefore))

	pl, err := d.PurgeIssue(ctx, target.ID, "agent", nil)
	require.NoError(t, err)
	require.NotNil(t, pl.PurgeResetAfterEventID)
	assert.Greater(t, *pl.PurgeResetAfterEventID, maxBefore,
		"reserved cursor must exceed every events.id that existed at purge time")

	// Now create another issue and verify the next events.id is strictly
	// greater than the reserved cursor.
	_, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "next", Author: "tester",
	})
	require.NoError(t, err)
	assert.Greater(t, evt.ID, *pl.PurgeResetAfterEventID,
		"next real events.id must continue from reserved+1")
}

func TestPurgeIssue_UnknownIssueIsErrNotFound(t *testing.T) {
	d := openTestDB(t)
	_, err := d.PurgeIssue(context.Background(), 9999, "agent", nil)
	assert.True(t, errors.Is(err, db.ErrNotFound))
}

func TestPurgeIssue_PersistsReason(t *testing.T) {
	// Reason threads through to purge_log.reason and round-trips on the
	// returned PurgeLog. Catches argument-order regressions in the INSERT.
	d, ctx, _, target := setupTestIssue(t)

	reason := "ops cleanup"
	pl, err := d.PurgeIssue(ctx, target.ID, "agent", &reason)
	require.NoError(t, err)
	require.NotNil(t, pl.Reason)
	assert.Equal(t, "ops cleanup", *pl.Reason)
}

func TestPurgeIssue_OnSoftDeletedIssue(t *testing.T) {
	// PurgeIssue must work on already soft-deleted issues — the destructive
	// ladder is delete → purge, not delete-XOR-purge. lookupIssueIncludingDeleted
	// is the right primitive; this test pins the contract so a future swap
	// to a deleted-filtering lookup would fail loudly.
	d, ctx, _, target := setupSoftDeletedIssue(t)

	pl, err := d.PurgeIssue(ctx, target.ID, "agent", nil)
	require.NoError(t, err)
	assert.Equal(t, target.ID, pl.PurgedIssueID)

	// Row is gone from issues.
	assertRowCount(ctx, t, d, 0, "issue row removed even though it was soft-deleted first",
		`SELECT count(*) FROM issues WHERE id = ?`, target.ID)
}

func TestPurgeIssue_SnapshotsShortIDIntoPurgeLog(t *testing.T) {
	d, ctx, _, target := setupTestIssue(t)
	require.NotEmpty(t, target.ShortID, "issue under test must have a short_id")

	pl, err := d.PurgeIssue(ctx, target.ID, "agent", nil)
	require.NoError(t, err)
	require.NotNil(t, pl.ShortID, "purge_log must record the issue's short_id")
	assert.Equal(t, target.ShortID, *pl.ShortID)
}

// TestPurgeTombstone_BlocksReuseOfShortIDAtSameLength is the central
// purge-tombstone regression. Without the tombstone, purging the first
// issue would free its short_id and a later create whose ULID happens
// to suffix-match would silently inherit the slot — re-targeting any
// external "kata#abc4" ref that had been written against the purged
// issue. With the tombstone, assignShortIDIn auto-extends past L=4
// just as it would against a live sibling.
func TestPurgeTombstone_BlocksReuseOfShortIDAtSameLength(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "first",
		Author:    "tester",
	})
	require.NoError(t, err)
	require.Equal(t, "d4ex", a.ShortID)

	_, err = d.PurgeIssue(ctx, a.ID, "agent", nil)
	require.NoError(t, err)

	// Different ULID with the same last 4 chars. Without the tombstone, this
	// new issue would take "d4ex" (the slot is "free" since A is gone). With
	// the tombstone, assignShortIDIn auto-extends to L=5.
	b, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABXD4EX",
		Title:     "second",
		Author:    "tester",
	})
	require.NoError(t, err)
	assert.Equal(t, "xd4ex", b.ShortID,
		"purge_log tombstone must block reuse of A's short_id at L=4")
}

func TestPurgeTombstone_DifferentProjectsDoNotInterfere(t *testing.T) {
	// Tombstones are scoped to (project_id, short_id). A purge in project P1
	// must not gate a create in project P2 with the same ULID suffix.
	d, ctx, p1 := setupTestProject(t)
	p2 := createProject(ctx, t, d, "demo2")

	a, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p1.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "first",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, err = d.PurgeIssue(ctx, a.ID, "agent", nil)
	require.NoError(t, err)

	// Same ULID suffix, different project — should land at L=4.
	b, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p2.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABXD4EX",
		Title:     "second",
		Author:    "tester",
	})
	require.NoError(t, err)
	assert.Equal(t, "d4ex", b.ShortID,
		"P1's purge_log tombstone must not gate P2's short_id pool")
}
