package sqlitestore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func TestCreateProject_RoundTrips(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	p, err := d.CreateProject(ctx, "kata")
	require.NoError(t, err)
	assert.Equal(t, "kata", p.Name)
	assertValidUID(t, p.UID)
	assert.False(t, p.CreatedAt.IsZero())

	got, err := d.ProjectByName(ctx, "kata")
	require.NoError(t, err)
	assert.Equal(t, p.ID, got.ID)
	assert.Equal(t, p.UID, got.UID)
}

func assertValidUID(t *testing.T, got string) {
	t.Helper()
	require.Len(t, got, 26)
	assert.True(t, uid.Valid(got), "invalid UID %q", got)
}

func TestProjectByName_NotFound(t *testing.T) {
	d := openTestDB(t)
	_, err := d.ProjectByName(context.Background(), "missing")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestProjectByUID_RoundTrips(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "byuid")

	got, err := d.ProjectByUID(ctx, p.UID)
	require.NoError(t, err)
	assert.Equal(t, p.ID, got.ID)
	assert.Equal(t, p.Name, got.Name)
	assert.Equal(t, p.UID, got.UID)
}

func TestProjectByUID_NotFound(t *testing.T) {
	d := openTestDB(t)
	_, err := d.ProjectByUID(context.Background(), "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestCreateProject_DuplicateName(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	createProject(ctx, t, d, "x")
	_, err := d.CreateProject(ctx, "x")
	require.Error(t, err)
}

func TestRenameProject_UpdatesNameOnly(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createKataProject(ctx, t, d)
	alias := attachAlias(ctx, t, d, p.ID, "github.com/wesm/kata", "/tmp/kata")

	renamed, err := d.RenameProject(ctx, p.ID, "Kata Tracker")
	require.NoError(t, err)
	assert.Equal(t, p.ID, renamed.ID)
	assert.Equal(t, "Kata Tracker", renamed.Name)

	got, err := d.ProjectByID(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, "Kata Tracker", got.Name)
	aliases, err := d.ProjectAliases(ctx, p.ID)
	require.NoError(t, err)
	require.Len(t, aliases, 1)
	assert.Equal(t, alias.ID, aliases[0].ID)
}

func TestRenameProject_MissingReturnsErrNotFound(t *testing.T) {
	d := openTestDB(t)
	_, err := d.RenameProject(context.Background(), 9999, "missing")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestAttachAlias_AndLookup(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createKataProject(ctx, t, d)

	a, err := d.AttachAlias(ctx, p.ID, "github.com/wesm/kata", "git", "/tmp/x")
	require.NoError(t, err)
	assert.Equal(t, p.ID, a.ProjectID)
	assert.Equal(t, "git", a.AliasKind)

	got, err := d.AliasByIdentity(ctx, "github.com/wesm/kata")
	require.NoError(t, err)
	assert.Equal(t, a.ID, got.ID)
}

func TestAliasByIdentity_NotFound(t *testing.T) {
	d := openTestDB(t)
	_, err := d.AliasByIdentity(context.Background(), "missing")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestTouchAlias_UpdatesLastSeen(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "x")
	a := attachAlias(ctx, t, d, p.ID, "x", "/tmp/x")

	require.NoError(t, d.TouchAlias(ctx, a.ID, "/tmp/y"))
	got, err := d.AliasByIdentity(ctx, "x")
	require.NoError(t, err)
	assert.Equal(t, "/tmp/y", got.RootPath)
	assert.True(t, !got.LastSeenAt.Before(a.LastSeenAt))
}

func TestTouchAlias_MissingAliasReturnsErrNotFound(t *testing.T) {
	d := openTestDB(t)
	err := d.TouchAlias(context.Background(), 9999, "/tmp/x")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestListProjects_Empty(t *testing.T) {
	d := openTestDB(t)
	got, err := d.ListProjects(context.Background())
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListProjects_OrdersByIDAsc(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	createProject(ctx, t, d, "a")
	createProject(ctx, t, d, "b")

	got, err := d.ListProjects(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].Name)
	assert.Equal(t, "b", got[1].Name)
}

func TestProjectAliases_ReturnsAllForProject(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	attachAlias(ctx, t, d, p.ID, "alias-a", "/tmp/a")
	attachAlias(ctx, t, d, p.ID, "alias-b", "/tmp/b")

	got, err := d.ProjectAliases(ctx, p.ID)
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

func TestMergeProjects_MovesSourceIntoSurvivingTarget(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	alpha := createProject(ctx, t, d, "alpha")
	beta := createProject(ctx, t, d, "beta")
	attachAlias(ctx, t, d, alpha.ID, "github.com/wesm/alpha", "/tmp/alpha")
	attachAlias(ctx, t, d, beta.ID, "github.com/wesm/beta", "/tmp/beta")
	parent := makeIssue(t, ctx, d, alpha.ID, "parent", "tester")
	child := makeIssue(t, ctx, d, alpha.ID, "child", "tester")
	_, _, err := d.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		ProjectID: alpha.ID, FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "tester",
	}, db.LinkEventParams{
		EventType:    "issue.linked",
		EventIssueID: child.ID,
		FromShortID:  child.ShortID, FromUID: child.UID,
		ToShortID: parent.ShortID, ToUID: parent.UID,
		Actor: "tester",
	})
	require.NoError(t, err)

	merged, err := d.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: alpha.ID,
		TargetProjectID: beta.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, beta.ID, merged.Target.ID)
	assert.Equal(t, "beta", merged.Target.Name)
	assert.Equal(t, int64(2), merged.IssuesMoved)
	assert.Equal(t, int64(1), merged.AliasesMoved)
	assert.Equal(t, int64(3), merged.EventsMoved)

	// TODO(Task 7): merge collision behavior is rewritten there; for now switch
	// the lookups to short_id so the test compiles. The merge-collision
	// failure of this case is a Task 7 concern, not a Task 5 lookup concern.
	gotParent, err := d.IssueByShortID(ctx, beta.ID, parent.ShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, "parent", gotParent.Title)
	assert.Equal(t, beta.ID, gotParent.ProjectID)
	gotChild, err := d.IssueByShortID(ctx, beta.ID, child.ShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, "child", gotChild.Title)
	assert.Equal(t, beta.ID, gotChild.ProjectID)
	_, err = d.IssueByShortID(ctx, alpha.ID, parent.ShortID, db.IncludeDeletedNo)
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = d.ProjectByID(ctx, alpha.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)

	aliases, err := d.ProjectAliases(ctx, beta.ID)
	require.NoError(t, err)
	var aliasIdentities []string
	for _, alias := range aliases {
		aliasIdentities = append(aliasIdentities, alias.AliasIdentity)
		assert.Equal(t, beta.ID, alias.ProjectID)
	}
	assert.ElementsMatch(t, []string{"github.com/wesm/alpha", "github.com/wesm/beta"}, aliasIdentities)

	events, err := d.EventsAfter(ctx, db.EventsAfterParams{ProjectID: beta.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 3)
	for _, event := range events {
		assert.Equal(t, beta.ID, event.ProjectID)
		assert.Equal(t, "beta", event.ProjectName)
	}
}

func TestMergeProjects_PreservesAliasesAlreadyTargetingTarget(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	source := createProject(ctx, t, d, "old")
	target := createProject(ctx, t, d, "new")
	attachAlias(ctx, t, d, target.ID, "github.com/wesm/old", "/tmp/old")

	_, err := d.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: source.ID,
		TargetProjectID: target.ID,
	})
	require.NoError(t, err)

	got, err := d.AliasByIdentity(ctx, "github.com/wesm/old")
	require.NoError(t, err)
	assert.Equal(t, target.ID, got.ProjectID)
}

// TestMergeProjects_RejectsArchivedSource pins the #24 invariant: an
// archived source can't be merged into the target. Restore-then-merge would be
// required if/when restore ships.
func TestMergeProjects_RejectsArchivedSource(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	source := createProject(ctx, t, d, "src")
	target := createProject(ctx, t, d, "tgt")
	_, _, err := d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: source.ID, Actor: "tester"})
	require.NoError(t, err)

	_, err = d.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: source.ID, TargetProjectID: target.ID,
	})
	assert.ErrorIs(t, err, db.ErrProjectMergeArchivedSource)
}

// TestMergeProjects_RejectsArchivedTarget pins the symmetric guard: folding
// live work into an archived project undoes the archive's intent.
func TestMergeProjects_RejectsArchivedTarget(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	source := createProject(ctx, t, d, "src")
	target := createProject(ctx, t, d, "tgt")
	_, _, err := d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: target.ID, Actor: "tester"})
	require.NoError(t, err)

	_, err = d.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: source.ID, TargetProjectID: target.ID,
	})
	assert.ErrorIs(t, err, db.ErrProjectMergeArchivedTarget)
}

func TestMergeProjects_RejectsSystemProject(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	sys, err := d.SystemProject(ctx)
	require.NoError(t, err)
	target := createProject(ctx, t, d, "tgt")

	_, err = d.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: sys.ID, TargetProjectID: target.ID,
	})
	assert.ErrorIs(t, err, db.ErrNotFound)

	_, err = d.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: target.ID, TargetProjectID: sys.ID,
	})
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestRemoveAndRestoreProject_RejectSystemProject(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	sys, err := d.SystemProject(ctx)
	require.NoError(t, err)

	_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: sys.ID,
		Actor:     "tester",
		Force:     true,
	})
	assert.ErrorIs(t, err, db.ErrNotFound)

	_, _, _, err = d.RestoreProject(ctx, sys.ID, "tester")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

// TestMergeProjects_ExtendsCollidingSourceShortIDs pins the §5.2 merge rule:
// source-side issues whose short_ids collide with target-side issues are
// auto-extended to the next non-colliding length. Existing target short_ids
// stay put. The merge response lists each shifted issue's pre/post short_id.
func TestMergeProjects_ExtendsCollidingSourceShortIDs(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	src := createProject(ctx, t, d, "src")
	dst := createProject(ctx, t, d, "dst")

	// Two issues whose ULIDs share the last 4 chars; one in each project.
	// Because short_ids are assigned per project, both land at "d4ex" on
	// their own side. The merge has to break the tie.
	dstIssue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: dst.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "dst",
		Author:    "tester",
	})
	require.NoError(t, err)
	require.Equal(t, "d4ex", dstIssue.ShortID)
	srcIssue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: src.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABXD4EX",
		Title:     "src",
		Author:    "tester",
	})
	require.NoError(t, err)
	require.Equal(t, "d4ex", srcIssue.ShortID) // independent assignment per project

	res, err := d.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: src.ID,
		TargetProjectID: dst.ID,
	})
	require.NoError(t, err)
	require.Len(t, res.ShortIDExtensions, 1)
	ext := res.ShortIDExtensions[0]
	assert.Equal(t, srcIssue.UID, ext.UID)
	assert.Equal(t, "d4ex", ext.PreMergeShortID)
	assert.Equal(t, "xd4ex", ext.PostMergeShortID)

	// Both issues now visible on dst with distinct short_ids.
	got, err := d.IssueByShortID(ctx, dst.ID, "d4ex", db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, dstIssue.UID, got.UID)
	got, err = d.IssueByShortID(ctx, dst.ID, "xd4ex", db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, srcIssue.UID, got.UID)
}

// TestMergeProjects_ExtendsAgainstTargetPurgeLogTombstone pins the
// tombstone-aware merge rule: a source-side issue whose short_id collides
// with a target-side purge_log tombstone (not a live target issue) is
// auto-extended too. Without this, a purged-then-merged-into target would
// silently re-issue the slot the tombstone owned.
func TestMergeProjects_ExtendsAgainstTargetPurgeLogTombstone(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	src := createProject(ctx, t, d, "src")
	dst := createProject(ctx, t, d, "dst")

	// Target side: create issue with short_id "d4ex", then purge it. The
	// purge_log row tombstones "d4ex" against future creates in dst.
	dstGone, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: dst.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "dst (will be purged)",
		Author:    "tester",
	})
	require.NoError(t, err)
	require.Equal(t, "d4ex", dstGone.ShortID)
	_, err = d.PurgeIssue(ctx, dstGone.ID, "agent", nil)
	require.NoError(t, err)

	// Source side: a different ULID with the same last 4 chars. On the source
	// project alone, it lands at "d4ex" — no live siblings.
	srcIssue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: src.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABXD4EX",
		Title:     "src",
		Author:    "tester",
	})
	require.NoError(t, err)
	require.Equal(t, "d4ex", srcIssue.ShortID)

	// Merge: src.d4ex must auto-extend because dst's purge_log already
	// claims "d4ex".
	res, err := d.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: src.ID,
		TargetProjectID: dst.ID,
	})
	require.NoError(t, err)
	require.Len(t, res.ShortIDExtensions, 1)
	assert.Equal(t, srcIssue.UID, res.ShortIDExtensions[0].UID)
	assert.Equal(t, "d4ex", res.ShortIDExtensions[0].PreMergeShortID)
	assert.Equal(t, "xd4ex", res.ShortIDExtensions[0].PostMergeShortID)
}

// TestMergeProjects_DoesNotShortenExistingShortIDs pins the §5.2 invariant
// that a merge rekey only ever extends a colliding source short_id; it must
// never produce a shorter one. The bug this guards against: when a source
// issue is stored at length L > MinLength because earlier neighbors at the
// same length-MinLength suffix have since been purged, the namespace at
// MinLength on the source side is free again. A naive rekey that searches
// from MinLength would shorten the issue's display ID — silently breaking
// every external reference that already cited the longer form.
//
// The setup uses ShortIDOverride on both sides to seed the exact pathological
// state without recreating the neighbor-then-purge dance: source B and target
// T both carry short_id "xd4ex" (length 5) under UIDs whose length-4 suffix
// "d4ex" is unoccupied on either side. Merge must rekey B to a length >= 6.
func TestMergeProjects_DoesNotShortenExistingShortIDs(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	src := createProject(ctx, t, d, "src")
	dst := createProject(ctx, t, d, "dst")

	// Source B at length 5: UID ends in "XD4EX" so the length-4 suffix is
	// "d4ex" and the length-5 suffix is "xd4ex". ShortIDOverride lets us
	// skip the auto-extend search and persist length 5 directly, simulating
	// a state where earlier source-side neighbors at "d4ex" have been
	// purged but B itself was already extended to length 5.
	srcIssue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID:       src.ID,
		UID:             "01HZNQ7VFPK1XGD8R5MABXD4EX",
		ShortIDOverride: "xd4ex",
		Title:           "src",
		Author:          "tester",
	})
	require.NoError(t, err)
	require.Equal(t, "xd4ex", srcIssue.ShortID)

	// Target T at the same length-5 short_id, under a distinct UID that
	// also ends in "XD4EX". The collision basis is (project_id, short_id),
	// so the two rows coexist across projects but will clash on merge.
	dstIssue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID:       dst.ID,
		UID:             "01HZNQ7VFPK1XGD8R5MACXD4EX",
		ShortIDOverride: "xd4ex",
		Title:           "dst",
		Author:          "tester",
	})
	require.NoError(t, err)
	require.Equal(t, "xd4ex", dstIssue.ShortID)

	res, err := d.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: src.ID,
		TargetProjectID: dst.ID,
	})
	require.NoError(t, err)
	require.Len(t, res.ShortIDExtensions, 1)
	ext := res.ShortIDExtensions[0]
	assert.Equal(t, srcIssue.UID, ext.UID)
	assert.Equal(t, "xd4ex", ext.PreMergeShortID)
	// The core assertion: the post-merge short_id is strictly longer than
	// the pre-merge value. Without the floor, length 4 ("d4ex") is free on
	// both projects, so the buggy path would assign that — a shortening.
	assert.Greaterf(t, len(ext.PostMergeShortID), len(ext.PreMergeShortID),
		"rekeyed short_id %q must be longer than pre-merge %q",
		ext.PostMergeShortID, ext.PreMergeShortID)
	assert.GreaterOrEqual(t, len(ext.PostMergeShortID), 6,
		"rekey must search at lengths strictly greater than the colliding length")

	// Target's original short_id stays put (existing target ids are not bumped).
	got, err := d.IssueByShortID(ctx, dst.ID, "xd4ex", db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, dstIssue.UID, got.UID)
	// Source row resolves at its new (longer) short_id on the merged target.
	got, err = d.IssueByShortID(ctx, dst.ID, ext.PostMergeShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, srcIssue.UID, got.UID)
}

func TestMergeProjects_MovesImportMappingsToTargetProject(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	source := createProject(ctx, t, d, "src")
	target := createProject(ctx, t, d, "target")
	issue := makeIssue(t, ctx, d, source.ID, "mapped issue", "tester")
	comment, _, err := d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "mapped comment"})
	require.NoError(t, err)

	_, err = d.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "beads", ExternalID: "issue-1", ObjectType: "issue", ProjectID: source.ID, IssueID: &issue.ID,
	})
	require.NoError(t, err)
	_, err = d.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "beads", ExternalID: "comment-1", ObjectType: "comment", ProjectID: source.ID, IssueID: &issue.ID, CommentID: &comment.ID,
	})
	require.NoError(t, err)

	_, err = d.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: source.ID, TargetProjectID: target.ID})
	require.NoError(t, err)

	mappings, err := d.ImportMappingsByProjectSource(ctx, target.ID, "beads")
	require.NoError(t, err)
	require.Len(t, mappings, 2)
	for _, mapping := range mappings {
		assert.Equal(t, target.ID, mapping.ProjectID)
	}
	_, err = d.ImportMappingBySource(ctx, source.ID, "beads", "issue", "issue-1")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestMergeProjects_ImportMappingCollisionReturnsError(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	source := createProject(ctx, t, d, "src")
	target := createProject(ctx, t, d, "target")
	sourceIssue := makeIssue(t, ctx, d, source.ID, "source mapped", "tester")
	targetIssue := makeIssue(t, ctx, d, target.ID, "target mapped", "tester")

	_, err := d.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "beads", ExternalID: "issue-1", ObjectType: "issue", ProjectID: source.ID, IssueID: &sourceIssue.ID,
	})
	require.NoError(t, err)
	_, err = d.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "beads", ExternalID: "issue-1", ObjectType: "issue", ProjectID: target.ID, IssueID: &targetIssue.ID,
	})
	require.NoError(t, err)

	_, err = d.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: source.ID, TargetProjectID: target.ID})
	require.ErrorIs(t, err, db.ErrProjectMergeImportMappingCollision)

	_, lookupErr := d.ProjectByID(ctx, source.ID)
	require.NoError(t, lookupErr)
	assertRowCount(ctx, t, d, 1, "source mapping preserved after failed merge",
		`SELECT count(*) FROM import_mappings WHERE project_id = ?`, source.ID)
	assertRowCount(ctx, t, d, 1, "target mapping preserved after failed merge",
		`SELECT count(*) FROM import_mappings WHERE project_id = ?`, target.ID)
}

func TestMergeProjects_RejectsFederationBinding(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	source := createProject(ctx, t, d, "src")
	target := createProject(ctx, t, d, "target")
	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            source.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        source.UID,
		ReplayHorizonEventID: 7,
		PullCursorEventID:    6,
		Enabled:              true,
	})
	require.NoError(t, err)

	_, err = d.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: source.ID, TargetProjectID: target.ID})
	require.ErrorIs(t, err, db.ErrProjectMergeFederationBinding)

	_, lookupErr := d.ProjectByID(ctx, source.ID)
	require.NoError(t, lookupErr)
}

func TestBatchProjectStats_EmptyProjectReturnsZeroes(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "empty")

	stats, err := d.BatchProjectStats(ctx)
	require.NoError(t, err)

	require.Contains(t, stats, p.ID)
	s := stats[p.ID]
	assert.Equal(t, 0, s.Open)
	assert.Equal(t, 0, s.Closed)
	assert.Nil(t, s.LastEventAt, "no events → LastEventAt is nil")
}

// TestBatchProjectStats_NoCountInflation pins the spec §6.1 contract:
// the issues-and-events join MUST be pre-aggregated, otherwise N issues
// times M events would inflate counts. Three issues + four events on the
// same project must still report Open=3.
func TestBatchProjectStats_NoCountInflation(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "proj")
	var first db.Issue
	for i := range 3 {
		iss := makeIssue(t, ctx, d, p.ID, "i", "tester")
		if i == 0 {
			first = iss
		}
	}
	iss, err := d.IssueByShortID(ctx, p.ID, first.ShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, _, err = d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: iss.ID,
		Author:  "tester",
		Body:    "note",
	})
	require.NoError(t, err)

	stats, err := d.BatchProjectStats(ctx)
	require.NoError(t, err)
	require.Contains(t, stats, p.ID)
	assert.Equal(t, 3, stats[p.ID].Open, "must not inflate by event count")
	assert.Equal(t, 0, stats[p.ID].Closed)
	assert.NotNil(t, stats[p.ID].LastEventAt)
}

// TestBatchProjectStats_ExcludesSoftDeletedIssues pins that issues with
// deleted_at != NULL do not count toward Open/Closed. SoftDeleteIssue is
// the right primitive — PurgeIssue would hard-delete the row and the
// `WHERE deleted_at IS NULL` filter would never get a chance to exercise
// itself. Spec §6.1.
func TestBatchProjectStats_ExcludesSoftDeletedIssues(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "proj")
	makeIssue(t, ctx, d, p.ID, "live", "tester")
	soft := makeIssue(t, ctx, d, p.ID, "soft", "tester")
	_, _, _, err := d.SoftDeleteIssue(ctx, soft.ID, "tester")
	require.NoError(t, err)
	// Sanity: the soft-deleted row still exists with deleted_at set, so
	// the filter is what's actually doing the work.
	got, err := d.IssueByID(ctx, soft.ID)
	require.NoError(t, err)
	require.NotNil(t, got.DeletedAt, "soft-delete must leave row with deleted_at set")

	stats, err := d.BatchProjectStats(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, stats[p.ID].Open, "soft-deleted issue must not count")
}

// TestBatchProjectStats_ExcludesArchivedProjects pins that archived
// projects don't appear in the result map at all. Spec §6.1.
func TestBatchProjectStats_ExcludesArchivedProjects(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	live := createProject(ctx, t, d, "live")
	arch := createProject(ctx, t, d, "arch")
	_, _, err := d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: arch.ID, Actor: "tester"})
	require.NoError(t, err)

	stats, err := d.BatchProjectStats(ctx)
	require.NoError(t, err)
	assert.Contains(t, stats, live.ID)
	assert.NotContains(t, stats, arch.ID)
}

// TestBatchProjectStats_PartitionsByProject pins that two projects with
// distinct issue counts produce distinct rows; counts are not summed
// across projects. Spec §6.1.
func TestBatchProjectStats_PartitionsByProject(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	a := createProject(ctx, t, d, "a")
	b := createProject(ctx, t, d, "b")
	for range 2 {
		makeIssue(t, ctx, d, a.ID, "x", "tester")
	}
	makeIssue(t, ctx, d, b.ID, "y", "tester")

	stats, err := d.BatchProjectStats(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, stats[a.ID].Open)
	assert.Equal(t, 1, stats[b.ID].Open)
}

// TestBatchProjectStats_ParsesZonedLegacyTimestamp pins that
// parseSQLiteTimestamp accepts the zoned legacy layout
// ("YYYY-MM-DD HH:MM:SS.NNN-07:00") that jsonl.parseExportTime emits on
// the import path. Without this layout, an imported database with a
// zoned offset on events.created_at would 500 the stats endpoint.
//
// The zoned event uses a far-future year (2099) so MAX(created_at)'s
// string comparison deterministically picks this row over the
// CreateIssue-generated issue.created event, whose RFC3339Nano
// timestamp differs at position 10 ('T' vs ' '). Without the
// year-bump, the issue.created row wins the MAX regardless of date
// and parseSQLiteTimestamp's zoned layout is never exercised.
func TestBatchProjectStats_ParsesZonedLegacyTimestamp(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "zoned")
	issue := makeIssue(t, ctx, d, p.ID, "live", "tester")
	// Stamp an event whose created_at uses the zoned legacy layout. The
	// MAX(created_at) over events for this project will surface this row,
	// driving parseSQLiteTimestamp through the new layout slot.
	const zonedTS = "2099-05-04 12:34:56.789-07:00"
	insertLegacyEvent(ctx, t, d, p, issue, "issue.edited", zonedTS)

	stats, err := d.BatchProjectStats(ctx)
	require.NoError(t, err)
	require.Contains(t, stats, p.ID)
	require.NotNil(t, stats[p.ID].LastEventAt, "zoned timestamp must parse, not nil")
	// "2099-05-04 12:34:56.789-07:00" → UTC 2099-05-04 19:34:56.789Z.
	// Without parseSQLiteTimestamp's zoned layout, BatchProjectStats would
	// either drop this row or 500. The exact-time assertion prevents a
	// loose nil-check from masking the parser regressing.
	expected := time.Date(2099, 5, 4, 19, 34, 56, 789000000, time.UTC)
	require.True(t, stats[p.ID].LastEventAt.Equal(expected),
		"zoned event time wrong: got %v, want %v", stats[p.ID].LastEventAt, expected)
}

// TestBatchProjectStats_PicksAbsoluteLatestAcrossMixedFormats pins that
// MAX over events.created_at compares by parsed time, not by lex string,
// so the absolute-latest event wins even when the events table contains
// a mix of T-separated RFC3339 and space/offset legacy layouts (which
// parseSQLiteTimestamp accepts on the read path).
//
// Concretely: we stamp two events on the same project, both at the same
// real-world UTC instant. One uses the legacy zoned layout
// ("YYYY-MM-DD HH:MM:SS.SSS-07:00"); the other is a millisecond earlier
// in the current RFC3339Nano layout. Because '<space>' (0x20) sorts
// before 'T' (0x54), a naive lex MAX(created_at) would pick the
// later-T-formatted earlier event. After the julianday() normalization,
// the absolute-latest space-zoned event wins.
func TestBatchProjectStats_PicksAbsoluteLatestAcrossMixedFormats(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "mixed-ts")
	issue := makeIssue(t, ctx, d, p.ID, "live", "tester")

	// Earlier-in-absolute-time but lex-LATER (T separator > space).
	const earlierTRFC = "2050-01-01T00:00:00.000Z"
	insertLegacyEvent(ctx, t, d, p, issue, "issue.edited", earlierTRFC)

	// Later-in-absolute-time but lex-EARLIER (space separator < T).
	// 2050-01-01 00:00:01-00:00 == 2050-01-01T00:00:01Z which is one
	// second after earlierTRFC. Lex MAX would pick earlierTRFC.
	const laterZoned = "2050-01-01 00:00:01.000-00:00"
	insertLegacyEvent(ctx, t, d, p, issue, "issue.commented", laterZoned)

	stats, err := d.BatchProjectStats(ctx)
	require.NoError(t, err)
	require.Contains(t, stats, p.ID)
	require.NotNil(t, stats[p.ID].LastEventAt)
	expected := time.Date(2050, 1, 1, 0, 0, 1, 0, time.UTC)
	assert.True(t, stats[p.ID].LastEventAt.Equal(expected),
		"BatchProjectStats picked the lex-largest row, not the absolute-latest: got %v, want %v",
		stats[p.ID].LastEventAt, expected)
}

func TestHardDeleteProject(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	proj, err := d.CreateProject(ctx, "doomed")
	require.NoError(t, err)

	require.NoError(t, d.HardDeleteProject(ctx, proj.ID))

	_, err = d.ProjectByID(ctx, proj.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestReassignAlias(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	src, err := d.CreateProject(ctx, "src")
	require.NoError(t, err)
	dst, err := d.CreateProject(ctx, "dst")
	require.NoError(t, err)
	alias, err := d.AttachAlias(ctx, src.ID, "git:example.com/repo", "git", "/old/root")
	require.NoError(t, err)

	require.NoError(t, d.ReassignAlias(ctx, alias.ID, dst.ID, "/new/root"))

	got, err := d.AliasByIdentity(ctx, "git:example.com/repo")
	require.NoError(t, err)
	require.Equal(t, dst.ID, got.ProjectID)
	require.Equal(t, "/new/root", got.RootPath)
}
