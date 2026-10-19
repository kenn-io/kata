package dbtest

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Publication must preserve recoverability when an operator disables its binding
// and then tries to replace the project's identity or source authority.
func checkIssuePublishFederationAdoption(t *testing.T, store db.Storage, _ Backend) error {
	ctx := t.Context()
	p, claim, pending := publicationFixture(t, store, `{}`)
	require.NoError(t, p.DispatchIssuePublication(ctx, claim.Guard, pending.AttemptUID))
	_, err := store.DisableIssueSyncBinding(ctx, claim.Guard.ProjectID)
	require.NoError(t, err)
	for _, action := range []string{"adopt", "spoke"} {
		t.Run(action, func(t *testing.T) {
			if action == "adopt" {
				_, err = store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: claim.Guard.ProjectID, HubProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", HubProjectID: 42, HubURL: "https://daemon.example", Actor: "worker"})
			} else {
				_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: claim.Guard.ProjectID, Role: db.FederationRoleSpoke, HubProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", HubProjectID: 42, HubURL: "https://daemon.example", Enabled: true})
			}
			var blocked *db.IssuePublicationPendingError
			require.ErrorAs(t, err, &blocked)
			project, e := store.ProjectByID(ctx, claim.Guard.ProjectID)
			require.NoError(t, e)
			require.Equal(t, claim.Guard.ProjectUID, project.UID)
			saved, e := p.PendingIssuePublication(ctx, project.ID)
			require.NoError(t, e)
			require.NotNil(t, saved)
			require.Equal(t, pending.AttemptUID, saved.AttemptUID)
		})
	}
	return nil
}

// Content has a distinct revision from workflow edits. A real edit between the
// claim snapshot and preparation must prevent saving the obsolete request.
func checkIssuePublishPreflightContent(t *testing.T, store db.Storage, _ Backend) error {
	ctx := t.Context()
	for _, field := range []string{"title", "body"} {
		f, err := createIssueFixture(ctx, store, "preflight-"+field, "Native", "worker", nil)
		require.NoError(t, err)
		_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: f.Project.ID, Provider: "notion", SourceKey: "notion:" + field, RemoteID: field, DisplayName: "Example", Config: []byte(`{}`), IntervalSeconds: 60})
		require.NoError(t, err)
		p := store.(db.IssuePublicationStore)
		now := time.Now().UTC().Truncate(time.Millisecond)
		claim, err := p.ClaimIssuePublication(ctx, f.Project.ID, f.Issue.ID, "notion", now, now.Add(-time.Hour), false)
		require.NoError(t, err)
		edit := db.EditIssueParams{IssueID: f.Issue.ID, Actor: "worker"}
		if field == "title" {
			edit.Title = new("New title")
		} else {
			edit.Body = new("New body")
		}
		_, _, _, err = store.EditIssue(ctx, edit)
		require.NoError(t, err)
		current, err := store.IssueByID(ctx, f.Issue.ID)
		require.NoError(t, err)
		require.Equal(t, claim.Issue.Revision, current.Revision)
		require.NotEqual(t, claim.Issue.Title+claim.Issue.Body, current.Title+current.Body)
		_, err = p.BeginIssuePublication(ctx, db.IssuePublicationBegin{Guard: claim.Guard, Publication: db.IssuePublication{AttemptUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Actor: "worker", Revision: claim.Issue.Revision, Payload: []byte(`{"title":"Native"}`)}})
		require.ErrorIs(t, err, db.ErrImportValidation)
		saved, err := p.PendingIssuePublication(ctx, f.Project.ID)
		require.NoError(t, err)
		require.Nil(t, saved)
	}
	return nil
}

// A completion observed during recovery must still reach the normal TickTick
// importer, unless an intervening two-way local intent owns the next delivery.
func checkIssuePublishTickTickRecoveryStatus(t *testing.T, store db.Storage, _ Backend) error {
	ctx := t.Context()
	for _, localIntent := range []bool{false, true} {
		f, err := createIssueFixture(ctx, store, fmt.Sprintf("recovery-%v", localIntent), "Native", "worker", nil)
		require.NoError(t, err)
		config := `{"status_sync":"one-way"}`
		if localIntent {
			config = `{"status_sync":"two-way"}`
		}
		b, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: f.Project.ID, Provider: "ticktick", SourceKey: "ticktick:" + f.Project.UID, RemoteID: "example", DisplayName: "Example", Config: []byte(config), IntervalSeconds: 60})
		require.NoError(t, err)
		p := store.(db.IssuePublicationStore)
		now := time.Now().UTC().Truncate(time.Millisecond)
		claim, err := p.ClaimIssuePublication(ctx, f.Project.ID, f.Issue.ID, "ticktick", now, now.Add(-time.Hour), false)
		require.NoError(t, err)
		attempt, err := p.BeginIssuePublication(ctx, db.IssuePublicationBegin{Guard: claim.Guard, Publication: db.IssuePublication{AttemptUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Actor: "worker", Revision: f.Issue.Revision, Payload: []byte(`{}`)}})
		require.NoError(t, err)
		require.NoError(t, p.DispatchIssuePublication(ctx, claim.Guard, attempt.AttemptUID))
		if localIntent {
			_, _, _, err = store.CloseIssue(ctx, f.Issue.ID, "done", "worker", "Completed locally", nil)
			require.NoError(t, err)
			_, _, _, err = store.ReopenIssue(ctx, f.Issue.ID, "worker")
			require.NoError(t, err)
		}
		observed := now.Add(time.Minute)
		remote := db.IssuePublicationRemote{ExternalID: "task:remote", Status: "closed", RawStatus: new("2"), Version: observed}
		m, _, err := p.AttachIssuePublication(ctx, db.IssuePublicationAttach{Guard: claim.Guard, AttemptUID: attempt.AttemptUID, Remote: remote})
		require.NoError(t, err)
		guard := db.IssueSyncImportGuard{BindingID: b.ID, Provider: "ticktick", StartedAt: now}
		state, err := store.(db.IssueStatusReader).IssueStatusMappingByID(ctx, guard, m.ID)
		require.NoError(t, err)
		if localIntent {
			require.NotEmpty(t, state.State.PendingEventUID)
			require.NotNil(t, state.State.Observed)
			continue
		}
		batch := db.ImportBatchParams{ProjectID: f.Project.ID, Source: b.SourceKey, Actor: "ticktick-sync", ImportStatusObservations: map[string]db.IssueStatusObservation{remote.ExternalID: {Raw: remote.RawStatus, Version: observed}}, Items: []db.ImportItem{{ExternalID: remote.ExternalID, Title: "Remote title", Body: "Remote body", Author: "worker", Status: "closed", ClosedReason: new("done"), ClosedAt: &observed, CreatedAt: now.Add(-time.Hour), UpdatedAt: observed}}}
		_, _, err = store.ImportBatch(ctx, batch)
		require.NoError(t, err)
		current, err := store.IssueByID(ctx, f.Issue.ID)
		require.NoError(t, err)
		require.Equal(t, "closed", current.Status, "first ordinary import applies completion observed during recovery")
	}
	return nil
}

// Incoming mappings must not consume an identity reserved for recovery.
func checkIssuePublishMoveDestination(t *testing.T, store db.Storage, _ Backend) error {
	ctx := t.Context()
	p, claim, pending := publicationFixture(t, store, `{}`)
	require.NoError(t, p.DispatchIssuePublication(ctx, claim.Guard, pending.AttemptUID))
	remote := publicationRemote()
	require.NoError(t, p.RecordIssuePublicationCreated(ctx, claim.Guard, pending.AttemptUID, remote))
	source, err := createIssueFixture(ctx, store, "source-project", "Imported issue", "worker", nil)
	require.NoError(t, err)
	_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: source.Project.ID, Source: claim.Binding.SourceKey, ObjectType: "issue", ExternalID: remote.ExternalID, IssueID: &source.Issue.ID})
	require.NoError(t, err)
	for _, dry := range []bool{true, false} {
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: source.Issue.ID, FromProjectID: source.Project.ID, ToProjectID: claim.Guard.ProjectID, IfMatchRev: source.Issue.Revision, Actor: "worker", DryRun: dry})
		var blocked *db.IssuePublicationPendingError
		require.ErrorAs(t, err, &blocked)
	}
	current, err := store.IssueByID(ctx, source.Issue.ID)
	require.NoError(t, err)
	require.Equal(t, source.Project.ID, current.ProjectID)
	mapping, _, err := p.AttachIssuePublication(ctx, db.IssuePublicationAttach{Guard: claim.Guard, AttemptUID: pending.AttemptUID, Remote: remote})
	require.NoError(t, err)
	require.Equal(t, claim.Issue.ID, *mapping.IssueID)
	return nil
}
