package pgstore

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// federationGroupFixture builds two spoke projects bound to the SAME hub
// origin, so federationBindingGroupProjectIDs puts them in one link group.
type federationGroupFixture struct {
	store  *Store
	first  db.Project
	second db.Project
	origin string
}

func newFederationGroupFixture(t *testing.T, schema string) *federationGroupFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{Schema: schema, SchemaMode: SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	fixture := &federationGroupFixture{store: store}
	fixture.first = fixture.bindSpoke(t, "spoke-project")
	fixture.second = fixture.bindSpoke(t, "hub-project")
	fixture.origin, err = uid.New()
	require.NoError(t, err)
	return fixture
}

func (f *federationGroupFixture) bindSpoke(t *testing.T, name string) db.Project {
	t.Helper()
	ctx := context.Background()
	project, err := f.store.CreateProject(ctx, name)
	require.NoError(t, err)
	_, err = f.store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://daemon.example", HubProjectID: project.ID,
		HubProjectUID: project.UID, Enabled: true,
	})
	require.NoError(t, err)
	return project
}

func (f *federationGroupFixture) seedIssue(t *testing.T, project db.Project, counter int64) string {
	t.Helper()
	issueUID, err := uid.New()
	require.NoError(t, err)
	payload := json.RawMessage(`{"uid":"` + issueUID + `","title":"seeded","body":"",` +
		`"author":"remote","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`)
	event := projectionSnapshotEvent(t, project, issueUID, f.origin, counter, payload)
	inserted, err := f.store.InsertRemoteEvent(context.Background(), project.ID, event)
	require.NoError(t, err)
	require.True(t, inserted)
	return issueUID
}

// Materializing one member of a link group must not create, modify, or prune
// anything in a sibling project. This is the direct guard for the highest-risk
// failure mode of the shared event cache: handing the group-wide projection to
// a per-project reconciler.
func TestMaterializeFederatedProjectLeavesSiblingUntouched(t *testing.T) {
	fixture := newFederationGroupFixture(t, "materialize_sibling")
	ctx := context.Background()
	firstUID := fixture.seedIssue(t, fixture.first, 1)
	secondUID := fixture.seedIssue(t, fixture.second, 2)

	require.NoError(t, fixture.store.MaterializeFederatedProject(ctx, fixture.second.ID))
	sibling, err := fixture.store.IssueByUID(ctx, secondUID, db.IncludeDeletedYes)
	require.NoError(t, err)

	require.NoError(t, fixture.store.MaterializeFederatedProject(ctx, fixture.first.ID))

	afterSibling, err := fixture.store.IssueByUID(ctx, secondUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, sibling.Revision, afterSibling.Revision,
		"materializing a group member must not touch its sibling's rows")
	assert.Equal(t, sibling.ShortID, afterSibling.ShortID)
	assert.Equal(t, fixture.second.ID, afterSibling.ProjectID)

	own, err := fixture.store.IssueByUID(ctx, firstUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, fixture.first.ID, own.ProjectID,
		"the sibling's issue must not have been materialized into this project")

	firstIssues, err := fixture.store.ListIssues(ctx, db.ListIssuesParams{ProjectID: fixture.first.ID})
	require.NoError(t, err)
	require.Len(t, firstIssues, 1, "exactly one issue belongs to the materialized project")
}

// Re-materializing with no new events must be a no-op. If the cache changed
// any reconcile decision, a revision would move.
func TestMaterializeFederatedProjectTwiceBumpsNoRevisions(t *testing.T) {
	fixture := newFederationGroupFixture(t, "materialize_idempotent")
	ctx := context.Background()
	issueUID := fixture.seedIssue(t, fixture.first, 1)

	require.NoError(t, fixture.store.MaterializeFederatedProject(ctx, fixture.first.ID))
	before, err := fixture.store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	beforeProject, err := fixture.store.ProjectByID(ctx, fixture.first.ID)
	require.NoError(t, err)

	require.NoError(t, fixture.store.MaterializeFederatedProject(ctx, fixture.first.ID))
	after, err := fixture.store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	afterProject, err := fixture.store.ProjectByID(ctx, fixture.first.ID)
	require.NoError(t, err)

	assert.Equal(t, before.Revision, after.Revision)
	assert.Equal(t, before.UpdatedAt, after.UpdatedAt)
	assert.Equal(t, beforeProject.Revision, afterProject.Revision)
}

// The regression this change fixes is a duplicate read of the current
// project's event log: federationBindingGroupProjectIDs includes the caller,
// so the group fold used to re-read and re-fold it. Count the reads.
func TestMaterializeFederatedProjectReadsEachEventLogOnce(t *testing.T) {
	fixture := newFederationGroupFixture(t, "materialize_fold_count")
	ctx := context.Background()
	fixture.seedIssue(t, fixture.first, 1)
	fixture.seedIssue(t, fixture.second, 2)

	reads := map[int64]int{}
	fixture.store.federationFoldObserver = func(projectID int64) { reads[projectID]++ }
	t.Cleanup(func() { fixture.store.federationFoldObserver = nil })

	require.NoError(t, fixture.store.MaterializeFederatedProject(ctx, fixture.first.ID))

	assert.Equal(t, 1, reads[fixture.first.ID],
		"the materialized project's event log must be read once, not twice")
	assert.Equal(t, 1, reads[fixture.second.ID],
		"each sibling in the link group contributes one read")
	assert.Len(t, reads, 2)
}

// A newly materialized issue must never be a prune candidate. The candidate
// set is drawn from the pre-insert snapshot, so this pins the equivalence
// argument for that substitution.
func TestMaterializeFederatedProjectDoesNotPruneJustInsertedIssues(t *testing.T) {
	fixture := newFederationGroupFixture(t, "materialize_prune_fresh")
	ctx := context.Background()
	firstUID := fixture.seedIssue(t, fixture.first, 1)

	require.NoError(t, fixture.store.MaterializeFederatedProject(ctx, fixture.first.ID))
	_, err := fixture.store.IssueByUID(ctx, firstUID, db.IncludeDeletedYes)
	require.NoError(t, err, "an issue created by this very pass must survive prune")

	// A second issue arriving on a later pass must also survive, and the
	// first must not be pruned now that it is in the snapshot.
	secondUID := fixture.seedIssue(t, fixture.first, 3)
	require.NoError(t, fixture.store.MaterializeFederatedProject(ctx, fixture.first.ID))
	_, err = fixture.store.IssueByUID(ctx, firstUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	_, err = fixture.store.IssueByUID(ctx, secondUID, db.IncludeDeletedYes)
	require.NoError(t, err)
}

// Prune deletes only issues with no surviving event referencing them, and it
// must decide that per issue rather than for the batch. Seed two orphan
// candidates where one is still referenced by an event.
func TestMaterializeFederatedProjectPrunesOnlyUnreferencedOrphans(t *testing.T) {
	fixture := newFederationGroupFixture(t, "materialize_prune_split")
	ctx := context.Background()
	keptUID := fixture.seedIssue(t, fixture.first, 1)
	droppedUID := fixture.seedIssue(t, fixture.first, 2)
	require.NoError(t, fixture.store.MaterializeFederatedProject(ctx, fixture.first.ID))

	kept, err := fixture.store.IssueByUID(ctx, keptUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	dropped, err := fixture.store.IssueByUID(ctx, droppedUID, db.IncludeDeletedYes)
	require.NoError(t, err)

	// Remove both issues from the projection by deleting their snapshot
	// events, then re-attach one event to the kept issue so it stays
	// referenced.
	_, err = fixture.store.ExecContext(ctx,
		`DELETE FROM events WHERE project_id=$1`, fixture.first.ID)
	require.NoError(t, err)
	_, err = fixture.store.ExecContext(ctx,
		`INSERT INTO events(project_id, project_name, issue_id, uid, origin_instance_uid, type, actor,
		 payload, hlc_physical_ms, hlc_counter, content_hash, created_at)
		 VALUES($1,$2,$3,$4,$5,'issue.updated','remote','{}',1779537600000,9,$6,$7)`,
		fixture.first.ID, fixture.first.Name, kept.ID, mustUID(t), mustUID(t),
		"0000000000000000000000000000000000000000000000000000000000000000",
		"2026-05-23T12:00:00.000Z")
	require.NoError(t, err)

	require.NoError(t, fixture.store.MaterializeFederatedProject(ctx, fixture.first.ID))

	_, err = fixture.store.IssueByUID(ctx, keptUID, db.IncludeDeletedYes)
	assert.NoError(t, err, "an issue still referenced by an event must survive prune")
	_, err = fixture.store.IssueByUID(ctx, droppedUID, db.IncludeDeletedYes)
	assert.ErrorIs(t, err, db.ErrNotFound, "an unreferenced orphan must be pruned")
	_ = dropped
}

func mustUID(t *testing.T) string {
	t.Helper()
	value, err := uid.New()
	require.NoError(t, err)
	return value
}
