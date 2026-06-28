package sqlitestore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/githubsync"
)

func TestGitHubSyncSchemaVersion(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	assert.Equal(t, 23, db.CurrentSchemaVersion())
	got, err := d.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, 23, got)
	assertSchemaVersion(t, d, 23)
}

func TestGitHubSyncEnableAndReenableSameRepository(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	initial := githubSyncBindingParams(p.ID)

	binding, err := d.UpsertIssueSyncBinding(ctx, initial)
	require.NoError(t, err)
	assert.Equal(t, p.ID, binding.ProjectID)
	assert.Equal(t, "github", binding.Provider)
	assert.Equal(t, "github:R_example_repo_1", binding.SourceKey)
	assert.Equal(t, "R_example_repo_1", binding.RemoteID)
	assert.Equal(t, "example-org/example-repo", binding.DisplayName)
	assert.JSONEq(t, `{"host":"github.com","owner":"example-org","repo":"example-repo","repo_id":1001,"title_prefix":true}`, string(binding.Config))
	assert.True(t, binding.Enabled)
	assert.Equal(t, 300, binding.IntervalSeconds)
	assert.Nil(t, binding.LastCursorAt)

	status, err := d.IssueSyncStatusByProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, binding.ID, status.BindingID)
	assert.Equal(t, p.ID, status.ProjectID)

	started := time.Date(2026, 6, 23, 9, 59, 0, 0, time.UTC)
	_, ok, err := d.ClaimIssueSyncBinding(ctx, binding.ID, "github", started, started.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	cursor := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	status, err = d.RecordIssueSyncSuccess(ctx, db.IssueSyncSuccessParams{
		BindingID:     binding.ID,
		StartedAt:     started,
		At:            cursor.Add(time.Minute),
		CursorAt:      cursor,
		LastCreated:   2,
		LastUpdated:   3,
		LastUnchanged: 4,
		LastComments:  5,
	})
	require.NoError(t, err)
	require.NotNil(t, status.LastSuccessAt)

	updatedParams := initial
	updatedParams.DisplayName = "example-owner/renamed-repo"
	updatedParams.Config = mustGitHubSyncConfig(t, "github.example", "example-owner", "renamed-repo", 2002)
	updatedParams.IntervalSeconds = 900
	updated, err := d.UpsertIssueSyncBinding(ctx, updatedParams)
	require.NoError(t, err)
	assert.Equal(t, binding.ID, updated.ID)
	assert.Equal(t, binding.SourceKey, updated.SourceKey)
	assert.Equal(t, binding.RemoteID, updated.RemoteID)
	assert.Equal(t, "example-owner/renamed-repo", updated.DisplayName)
	assert.JSONEq(t, `{"host":"github.example","owner":"example-owner","repo":"renamed-repo","repo_id":2002,"title_prefix":true}`, string(updated.Config))
	assert.True(t, updated.Enabled)
	assert.Equal(t, 900, updated.IntervalSeconds)
	assert.Nil(t, updated.LastCursorAt)
}

func TestGitHubSyncEnableDifferentRepositoryRejected(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	_, err := d.UpsertIssueSyncBinding(ctx, githubSyncBindingParams(p.ID))
	require.NoError(t, err)

	next := githubSyncBindingParams(p.ID)
	next.SourceKey = "github:R_other_repo_2"
	next.RemoteID = "R_other_repo_2"
	next.DisplayName = "example-org/other-repo"
	next.Config = mustGitHubSyncConfig(t, "github.com", "example-org", "other-repo", 1002)
	_, err = d.UpsertIssueSyncBinding(ctx, next)
	require.ErrorIs(t, err, db.ErrIssueSyncProjectAlreadyBound)
}

func TestGitHubSyncDisablePreservesStatusAndMappings(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	binding := mustUpsertIssueSyncBinding(ctx, t, d, p.ID)
	started := time.Date(2026, 6, 23, 11, 0, 0, 0, time.UTC)
	_, ok, err := d.ClaimIssueSyncBinding(ctx, binding.ID, "github", started, started.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	status, err := d.RecordIssueSyncSuccess(ctx, db.IssueSyncSuccessParams{
		BindingID:     binding.ID,
		StartedAt:     started,
		At:            started.Add(time.Minute),
		CursorAt:      started,
		LastCreated:   5,
		LastUpdated:   6,
		LastUnchanged: 7,
		LastComments:  8,
	})
	require.NoError(t, err)
	_, err = d.UpsertImportMapping(ctx, db.ImportMappingParams{
		ProjectID:  p.ID,
		Source:     binding.SourceKey,
		ObjectType: "issue",
		ExternalID: "issue:I_example_1",
		IssueID:    ptrInt64(makeIssue(t, ctx, d, p.ID, "from github", "github-sync").ID),
	})
	require.NoError(t, err)

	disabled, err := d.DisableIssueSyncBinding(ctx, p.ID)
	require.NoError(t, err)
	assert.False(t, disabled.Enabled)
	assert.Equal(t, binding.ID, disabled.ID)
	require.NotNil(t, disabled.LastCursorAt)
	assert.Equal(t, started, *disabled.LastCursorAt)

	gotStatus, err := d.IssueSyncStatusByProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, status.BindingID, gotStatus.BindingID)
	assert.Equal(t, 5, gotStatus.LastCreated)
	assert.Equal(t, 6, gotStatus.LastUpdated)
	assert.Equal(t, 7, gotStatus.LastUnchanged)
	assert.Equal(t, 8, gotStatus.LastComments)

	mappings, err := d.ImportMappingsByProjectSource(ctx, p.ID, binding.SourceKey)
	require.NoError(t, err)
	assert.Len(t, mappings, 1)
}

func TestGitHubSyncDueListing(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	due := mustUpsertIssueSyncBinding(ctx, t, d, p.ID)

	recentProject := createProject(ctx, t, d, "recent-project")
	recent := mustUpsertIssueSyncBinding(ctx, t, d, recentProject.ID)
	_, ok, err := d.ClaimIssueSyncBinding(ctx, recent.ID, "github", now.Add(-time.Minute), now.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	_, err = d.RecordIssueSyncError(ctx, db.IssueSyncErrorParams{
		BindingID: recent.ID,
		StartedAt: now.Add(-time.Minute),
		At:        now.Add(-time.Minute),
		Error:     "rate limit",
	})
	require.NoError(t, err)

	inFlightProject := createProject(ctx, t, d, "in-flight-project")
	inFlight := mustUpsertIssueSyncBinding(ctx, t, d, inFlightProject.ID)
	_, ok, err = d.ClaimIssueSyncBinding(ctx, inFlight.ID, "github", now.Add(-2*time.Hour), now.Add(-3*time.Hour))
	require.NoError(t, err)
	require.True(t, ok)

	disabledProject := createProject(ctx, t, d, "disabled-project")
	disabled := mustUpsertIssueSyncBinding(ctx, t, d, disabledProject.ID)
	_, err = d.DisableIssueSyncBinding(ctx, disabled.ProjectID)
	require.NoError(t, err)

	archivedProject := createProject(ctx, t, d, "archived-project")
	archived := mustUpsertIssueSyncBinding(ctx, t, d, archivedProject.ID)
	_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: archived.ProjectID,
		Actor:     "tester",
		Force:     true,
	})
	require.NoError(t, err)

	otherProviderProject := createProject(ctx, t, d, "other-provider-project")
	otherProviderParams := issueSyncBindingParams(otherProviderProject.ID, "linear", "linear:team-1", "team-1", "example-workspace/team-1")
	otherProvider, err := d.UpsertIssueSyncBinding(ctx, otherProviderParams)
	require.NoError(t, err)

	got, err := d.ListDueIssueSyncBindings(ctx, "github", now, now.Add(-time.Hour), 10)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []int64{due.ID, inFlight.ID}, gitHubSyncBindingIDs(got))

	linearDue, err := d.ListDueIssueSyncBindings(ctx, "linear", now, now.Add(-time.Hour), 10)
	require.NoError(t, err)
	assert.Equal(t, []int64{otherProvider.ID}, gitHubSyncBindingIDs(linearDue))

	empty, err := d.ListDueIssueSyncBindings(ctx, "github", now, now.Add(-time.Hour), 0)
	require.NoError(t, err)
	assert.NotNil(t, empty)
	assert.Empty(t, empty)
}

func TestGitHubSyncClaimRejectsBindingDisabledOrProjectArchivedAfterListing(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	now := time.Date(2026, 6, 23, 12, 30, 0, 0, time.UTC)
	disabled := mustUpsertIssueSyncBinding(ctx, t, d, p.ID)

	archivedProject := createProject(ctx, t, d, "archived-after-list-project")
	archived := mustUpsertIssueSyncBinding(ctx, t, d, archivedProject.ID)

	due, err := d.ListDueIssueSyncBindings(ctx, "github", now, now.Add(-time.Hour), 10)
	require.NoError(t, err)
	assert.ElementsMatch(t, []int64{disabled.ID, archived.ID}, gitHubSyncBindingIDs(due))

	_, err = d.DisableIssueSyncBinding(ctx, disabled.ProjectID)
	require.NoError(t, err)
	_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: archived.ProjectID,
		Actor:     "tester",
		Force:     true,
	})
	require.NoError(t, err)

	_, ok, err := d.ClaimIssueSyncBinding(ctx, disabled.ID, "github", now, now.Add(-time.Hour))
	require.NoError(t, err)
	assert.False(t, ok)

	_, ok, err = d.ClaimIssueSyncBinding(ctx, archived.ID, "github", now, now.Add(-time.Hour))
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestGitHubSyncClaimRejectsDifferentProvider(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	bindingParams := issueSyncBindingParams(p.ID, "linear", "linear:team-1", "team-1", "example-workspace/team-1")
	binding, err := d.UpsertIssueSyncBinding(ctx, bindingParams)
	require.NoError(t, err)
	now := time.Date(2026, 6, 23, 12, 45, 0, 0, time.UTC)

	got, ok, err := d.ClaimIssueSyncBinding(ctx, binding.ID, "github", now, now.Add(-time.Hour))
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, "linear", got.Provider)

	status, err := d.IssueSyncStatusByProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Nil(t, status.SyncStartedAt)
}

func TestGitHubSyncClaimHonorsInFlightAndStaleCutoff(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	binding := mustUpsertIssueSyncBinding(ctx, t, d, p.ID)
	now := time.Date(2026, 6, 23, 13, 0, 0, 0, time.UTC)

	claimed, ok, err := d.ClaimIssueSyncBinding(ctx, binding.ID, "github", now, now.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, binding.ID, claimed.ID)

	_, ok, err = d.ClaimIssueSyncBinding(ctx, binding.ID, "github", now.Add(time.Minute), now.Add(-time.Hour))
	require.NoError(t, err)
	assert.False(t, ok)

	claimed, ok, err = d.ClaimIssueSyncBinding(ctx, binding.ID, "github", now.Add(2*time.Hour), now.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, binding.ID, claimed.ID)
}

func TestGitHubSyncRecordSuccessAndError(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	binding := mustUpsertIssueSyncBinding(ctx, t, d, p.ID)
	started := time.Date(2026, 6, 23, 14, 0, 0, 0, time.UTC)
	cursor := started.Add(30 * time.Second)
	_, ok, err := d.ClaimIssueSyncBinding(ctx, binding.ID, "github", started, started.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)

	status, err := d.RecordIssueSyncSuccess(ctx, db.IssueSyncSuccessParams{
		BindingID:     binding.ID,
		StartedAt:     started,
		At:            started.Add(time.Minute),
		CursorAt:      cursor,
		LastCreated:   8,
		LastUpdated:   9,
		LastUnchanged: 10,
		LastComments:  11,
	})
	require.NoError(t, err)
	assert.Nil(t, status.SyncStartedAt)
	require.NotNil(t, status.LastSuccessAt)
	assert.Equal(t, started.Add(time.Minute), *status.LastSuccessAt)
	assert.Nil(t, status.LastErrorAt)
	assert.Empty(t, status.LastError)
	assert.Equal(t, 8, status.LastCreated)
	assert.Equal(t, 9, status.LastUpdated)
	assert.Equal(t, 10, status.LastUnchanged)
	assert.Equal(t, 11, status.LastComments)
	afterSuccess, err := d.IssueSyncBindingByID(ctx, binding.ID)
	require.NoError(t, err)
	require.NotNil(t, afterSuccess.LastCursorAt)
	assert.Equal(t, cursor, *afterSuccess.LastCursorAt)

	_, ok, err = d.ClaimIssueSyncBinding(ctx, binding.ID, "github", started.Add(2*time.Hour), started.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	status, err = d.RecordIssueSyncError(ctx, db.IssueSyncErrorParams{
		BindingID: binding.ID,
		StartedAt: started.Add(2 * time.Hour),
		At:        started.Add(2*time.Hour + time.Minute),
		Error:     "api unavailable",
	})
	require.NoError(t, err)
	assert.Nil(t, status.SyncStartedAt)
	require.NotNil(t, status.LastErrorAt)
	assert.Equal(t, "api unavailable", status.LastError)
	afterError, err := d.IssueSyncBindingByID(ctx, binding.ID)
	require.NoError(t, err)
	require.NotNil(t, afterError.LastCursorAt)
	assert.Equal(t, cursor, *afterError.LastCursorAt)
}

func TestGitHubSyncRecordSuccessRejectsStaleWorker(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	binding := mustUpsertIssueSyncBinding(ctx, t, d, p.ID)
	firstStarted := time.Date(2026, 6, 23, 15, 0, 0, 0, time.UTC)
	secondStarted := firstStarted.Add(2 * time.Hour)
	firstCursor := firstStarted.Add(time.Minute)

	_, ok, err := d.ClaimIssueSyncBinding(ctx, binding.ID, "github", firstStarted, firstStarted.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = d.ClaimIssueSyncBinding(ctx, binding.ID, "github", secondStarted, firstStarted.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, ok)

	_, err = d.RecordIssueSyncSuccess(ctx, db.IssueSyncSuccessParams{
		BindingID:   binding.ID,
		StartedAt:   firstStarted,
		At:          firstStarted.Add(2 * time.Minute),
		CursorAt:    firstCursor,
		LastCreated: 1,
	})
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)

	status, err := d.IssueSyncStatusByProject(ctx, p.ID)
	require.NoError(t, err)
	require.NotNil(t, status.SyncStartedAt)
	assert.Equal(t, secondStarted, *status.SyncStartedAt)
	assert.Nil(t, status.LastSuccessAt)
	assert.Equal(t, 0, status.LastCreated)

	after, err := d.IssueSyncBindingByID(ctx, binding.ID)
	require.NoError(t, err)
	assert.Nil(t, after.LastCursorAt)
}

func TestGitHubSyncRecordErrorRejectsStaleWorker(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	binding := mustUpsertIssueSyncBinding(ctx, t, d, p.ID)
	firstStarted := time.Date(2026, 6, 23, 16, 0, 0, 0, time.UTC)
	secondStarted := firstStarted.Add(2 * time.Hour)

	_, ok, err := d.ClaimIssueSyncBinding(ctx, binding.ID, "github", firstStarted, firstStarted.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = d.ClaimIssueSyncBinding(ctx, binding.ID, "github", secondStarted, firstStarted.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, ok)

	_, err = d.RecordIssueSyncError(ctx, db.IssueSyncErrorParams{
		BindingID: binding.ID,
		StartedAt: firstStarted,
		At:        firstStarted.Add(2 * time.Minute),
		Error:     "stale worker failed",
	})
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)

	status, err := d.IssueSyncStatusByProject(ctx, p.ID)
	require.NoError(t, err)
	require.NotNil(t, status.SyncStartedAt)
	assert.Equal(t, secondStarted, *status.SyncStartedAt)
	assert.Nil(t, status.LastErrorAt)
	assert.Empty(t, status.LastError)
}

func TestGitHubSyncRefreshRepositoryPreservesSourceIdentity(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	binding := mustUpsertIssueSyncBinding(ctx, t, d, p.ID)

	updated, err := d.RefreshIssueSyncBinding(ctx, db.IssueSyncBindingUpdateParams{
		BindingID:   binding.ID,
		DisplayName: "example-owner/example-renamed",
		Config:      mustGitHubSyncConfig(t, "github.example", "example-owner", "example-renamed", 2002),
	})
	require.NoError(t, err)
	assert.Equal(t, binding.ID, updated.ID)
	assert.Equal(t, binding.ProjectID, updated.ProjectID)
	assert.Equal(t, binding.SourceKey, updated.SourceKey)
	assert.Equal(t, binding.RemoteID, updated.RemoteID)
	assert.Equal(t, "example-owner/example-renamed", updated.DisplayName)
	assert.JSONEq(t, `{"host":"github.example","owner":"example-owner","repo":"example-renamed","repo_id":2002,"title_prefix":true}`, string(updated.Config))
}

func TestGitHubSyncEnableAllowsFederationHub(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	_, err := d.EnableProjectFederation(ctx, p.ID, "tester")
	require.NoError(t, err)

	binding, err := d.UpsertIssueSyncBinding(ctx, githubSyncBindingParams(p.ID))
	require.NoError(t, err)
	assert.Equal(t, p.ID, binding.ProjectID)
	assert.True(t, binding.Enabled)
}

func TestGitHubSyncEnableRejectsFederationSpoke(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)

	_, err = d.UpsertIssueSyncBinding(ctx, githubSyncBindingParams(p.ID))
	require.ErrorIs(t, err, db.ErrIssueSyncFederationBinding)
}

func TestEnableProjectFederationAllowsIssueSyncBinding(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	_ = mustUpsertIssueSyncBinding(ctx, t, d, p.ID)

	binding, err := d.EnableProjectFederation(ctx, p.ID, "tester")
	require.NoError(t, err)
	assert.Equal(t, db.FederationRoleHub, binding.Role)
}

func TestEnableProjectFederationAllowsDisabledIssueSyncBinding(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	_ = mustUpsertIssueSyncBinding(ctx, t, d, p.ID)
	_, err := d.DisableIssueSyncBinding(ctx, p.ID)
	require.NoError(t, err)

	_, err = d.EnableProjectFederation(ctx, p.ID, "tester")
	require.NoError(t, err)
}

func TestGitHubSyncImportGuardRejectsDisabledBinding(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	binding := mustUpsertIssueSyncBinding(ctx, t, d, p.ID)
	started := time.Date(2026, 6, 23, 17, 0, 0, 0, time.UTC)
	_, ok, err := d.ClaimIssueSyncBinding(ctx, binding.ID, "github", started, started.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	_, err = d.DisableIssueSyncBinding(ctx, p.ID)
	require.NoError(t, err)

	_, _, err = d.ImportBatch(ctx, guardedGitHubImport(p.ID, binding, started))
	require.ErrorIs(t, err, db.ErrIssueSyncNotEnabled)

	mappings, err := d.ImportMappingsByProjectSource(ctx, p.ID, binding.SourceKey)
	require.NoError(t, err)
	assert.Empty(t, mappings)
}

func TestGitHubSyncImportGuardRejectsClaimInvalidatedByDisableReenable(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	binding := mustUpsertIssueSyncBinding(ctx, t, d, p.ID)
	started := time.Date(2026, 6, 23, 17, 15, 0, 0, time.UTC)
	_, ok, err := d.ClaimIssueSyncBinding(ctx, binding.ID, "github", started, started.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	_, err = d.DisableIssueSyncBinding(ctx, p.ID)
	require.NoError(t, err)
	_, err = d.UpsertIssueSyncBinding(ctx, githubSyncBindingParams(p.ID))
	require.NoError(t, err)

	_, _, err = d.ImportBatch(ctx, guardedGitHubImport(p.ID, binding, started))
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)

	status, err := d.IssueSyncStatusByProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Nil(t, status.SyncStartedAt)
	mappings, err := d.ImportMappingsByProjectSource(ctx, p.ID, binding.SourceKey)
	require.NoError(t, err)
	assert.Empty(t, mappings)
}

func TestGitHubSyncImportGuardRejectsStaleClaim(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	binding := mustUpsertIssueSyncBinding(ctx, t, d, p.ID)
	firstStarted := time.Date(2026, 6, 23, 17, 30, 0, 0, time.UTC)
	secondStarted := firstStarted.Add(2 * time.Hour)
	_, ok, err := d.ClaimIssueSyncBinding(ctx, binding.ID, "github", firstStarted, firstStarted.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = d.ClaimIssueSyncBinding(ctx, binding.ID, "github", secondStarted, firstStarted.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, ok)

	_, _, err = d.ImportBatch(ctx, guardedGitHubImport(p.ID, binding, firstStarted))
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)

	mappings, err := d.ImportMappingsByProjectSource(ctx, p.ID, binding.SourceKey)
	require.NoError(t, err)
	assert.Empty(t, mappings)
}

func TestGitHubSyncImportGuardAllowsFederationHub(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	binding := mustUpsertIssueSyncBinding(ctx, t, d, p.ID)
	started := time.Date(2026, 6, 23, 18, 0, 0, 0, time.UTC)
	_, ok, err := d.ClaimIssueSyncBinding(ctx, binding.ID, "github", started, started.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	_, err = d.EnableProjectFederation(ctx, p.ID, "tester")
	require.NoError(t, err)

	res, _, err := d.ImportBatch(ctx, guardedGitHubImport(p.ID, binding, started))
	require.NoError(t, err)
	assert.Equal(t, 1, res.Created)

	mappings, err := d.ImportMappingsByProjectSource(ctx, p.ID, binding.SourceKey)
	require.NoError(t, err)
	assert.NotEmpty(t, mappings)
}

func TestUpsertFederationBindingAllowsHubIssueSyncBinding(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	_ = mustUpsertIssueSyncBinding(ctx, t, d, p.ID)

	binding, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleHub,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)
	assert.Equal(t, db.FederationRoleHub, binding.Role)
}

func TestUpsertFederationBindingRejectsSpokeIssueSyncBinding(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	_ = mustUpsertIssueSyncBinding(ctx, t, d, p.ID)

	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.ErrorIs(t, err, db.ErrIssueSyncFederationBinding)
}

func TestAdoptProjectIntoFederationRejectsIssueSyncBinding(t *testing.T) {
	d, ctx, p := setupGitHubSyncProject(t)
	_ = mustUpsertIssueSyncBinding(ctx, t, d, p.ID)

	_, err := d.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
		ProjectID:            p.ID,
		HubURL:               "http://hub:7373",
		HubProjectID:         42,
		HubProjectUID:        newTestUID(t),
		ReplayHorizonEventID: 10,
		Actor:                "bound-actor",
	})
	require.ErrorIs(t, err, db.ErrIssueSyncFederationBinding)
}

func setupGitHubSyncProject(t *testing.T) (*sqlitestore.Store, context.Context, db.Project) {
	t.Helper()
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "example-project")
	return d, ctx, p
}

func githubSyncBindingParams(projectID int64) db.UpsertIssueSyncBindingParams {
	return issueSyncBindingParams(projectID, "github", "github:R_example_repo_1", "R_example_repo_1", "example-org/example-repo")
}

func issueSyncBindingParams(projectID int64, provider, sourceKey, remoteID, displayName string) db.UpsertIssueSyncBindingParams {
	return db.UpsertIssueSyncBindingParams{
		ProjectID:       projectID,
		Provider:        provider,
		SourceKey:       sourceKey,
		RemoteID:        remoteID,
		DisplayName:     displayName,
		Config:          mustGitHubSyncConfig(nil, "github.com", "example-org", "example-repo", 1001),
		IntervalSeconds: 300,
	}
}

func mustGitHubSyncConfig(t testing.TB, host, owner, repo string, repoID int64) []byte {
	if t != nil {
		t.Helper()
	}
	config, err := githubsync.EncodeConfig(githubsync.Config{
		Host:   host,
		Owner:  owner,
		Repo:   repo,
		RepoID: repoID,
	})
	if t != nil {
		require.NoError(t, err)
	} else if err != nil {
		panic(err)
	}
	return config
}

func mustUpsertIssueSyncBinding(ctx context.Context, t *testing.T, d *sqlitestore.Store, projectID int64) db.IssueSyncBinding {
	t.Helper()
	b, err := d.UpsertIssueSyncBinding(ctx, githubSyncBindingParams(projectID))
	require.NoError(t, err)
	return b
}

func guardedGitHubImport(projectID int64, binding db.IssueSyncBinding, started time.Time) db.ImportBatchParams {
	itemTime := started.Add(-time.Minute)
	return db.ImportBatchParams{
		ProjectID: projectID,
		Source:    binding.SourceKey,
		Actor:     "github-sync",
		IssueSyncGuard: &db.IssueSyncImportGuard{
			BindingID: binding.ID,
			Provider:  binding.Provider,
			StartedAt: started,
		},
		Items: []db.ImportItem{{
			ExternalID: "issue:I_guarded_1",
			Title:      "guarded import",
			Body:       "body",
			Author:     "github-sync",
			Status:     "open",
			CreatedAt:  itemTime,
			UpdatedAt:  itemTime,
		}},
	}
}

func gitHubSyncBindingIDs(bindings []db.IssueSyncBinding) []int64 {
	out := make([]int64, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, binding.ID)
	}
	return out
}

func ptrInt64(v int64) *int64 { return &v }

func TestGitHubSyncMissingBindingReturnsNotFound(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	_, err := d.IssueSyncBindingByID(ctx, 999)
	require.True(t, errors.Is(err, db.ErrNotFound))
}
