package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Publication status is a read of committed recovery state, even while another
// connection holds the project's writer lock.
func checkIssuePublishStatusReadDuringWrite(t *testing.T, store db.Storage, backend Backend) error {
	_, claim, pending := publicationFixture(t, store, `{}`)
	reader := backend.OpenSame(t, store)
	writer := store.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	tx, err := writer.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(t.Context(), `UPDATE projects SET name=name WHERE id=$1`, claim.Issue.ProjectID)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = reader.ProjectByID(ctx, claim.Issue.ProjectID)
	require.NoError(t, err, "ordinary committed reads remain available during the write")
	saved, err := reader.(db.IssuePublicationStore).PendingIssuePublication(ctx, claim.Issue.ProjectID)
	require.NoError(t, err, "reading publication status must not acquire a project writer lock")
	require.NotNil(t, saved)
	require.Equal(t, pending.AttemptUID, saved.AttemptUID)
	return nil
}

// A dispatched create must remain durable after claim expiry and may never
// acquire another one-send authorization. This is the spec's crash contract.
func checkIssuePublishTransitions(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	f, err := createIssueFixture(ctx, store, "example-project", "Publish me", "worker", nil)
	require.NoError(t, err)
	b, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: f.Project.ID, Provider: "notion", SourceKey: "notion:example-source", RemoteID: "example-source", DisplayName: "Example", Config: []byte(`{}`), IntervalSeconds: 60})
	require.NoError(t, err)
	p, ok := store.(db.IssuePublicationStore)
	require.True(t, ok)
	now := time.Now().UTC().Truncate(time.Millisecond)
	claim, err := p.ClaimIssuePublication(ctx, f.Project.ID, f.Issue.ID, "notion", now, now.Add(-time.Hour), false)
	require.NoError(t, err)
	require.Equal(t, b.ID, claim.Binding.ID)
	require.Equal(t, f.Issue.UID, claim.Issue.UID)
	pending, err := p.BeginIssuePublication(ctx, db.IssuePublicationBegin{Guard: claim.Guard, Publication: db.IssuePublication{AttemptUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Actor: "worker", Revision: f.Issue.Revision, Payload: []byte(`{"title":"Publish me","markdown":""}`)}})
	require.NoError(t, err)
	require.Equal(t, "prepared", pending.Phase)
	require.Equal(t, f.Project.UID, pending.ProjectUID)
	require.Equal(t, f.Issue.UID, pending.IssueUID)
	require.NoError(t, p.DispatchIssuePublication(ctx, claim.Guard, pending.AttemptUID))
	require.Error(t, p.DispatchIssuePublication(ctx, claim.Guard, pending.AttemptUID), "one-send authorization must be consumed")
	other := backend.OpenSame(t, store).(db.IssuePublicationStore)
	saved, err := other.PendingIssuePublication(ctx, f.Project.ID)
	require.NoError(t, err)
	require.NotNil(t, saved)
	require.Equal(t, "dispatching", saved.Phase)
	require.Equal(t, pending.AttemptUID, saved.AttemptUID)
	later := now.Add(2 * time.Hour)
	resumed, err := other.ClaimIssuePublication(ctx, f.Project.ID, f.Issue.ID, "notion", later, later.Add(-time.Hour), true)
	require.NoError(t, err)
	require.Equal(t, pending.AttemptUID, resumed.Pending.AttemptUID)
	require.Error(t, other.DispatchIssuePublication(ctx, resumed.Guard, pending.AttemptUID), "claim expiry cannot permit another POST")
	return nil
}

func publicationFixture(t *testing.T, store db.Storage, config string) (db.IssuePublicationStore, db.IssuePublicationClaim, db.IssuePublication) {
	t.Helper()
	ctx := t.Context()
	f, err := createIssueFixture(ctx, store, "example-project", "Native title", "native-author", nil)
	require.NoError(t, err)
	_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: f.Project.ID, Provider: "notion", SourceKey: "notion:example-source", RemoteID: "example-source", DisplayName: "Example", Config: []byte(config), IntervalSeconds: 60})
	require.NoError(t, err)
	p, ok := store.(db.IssuePublicationStore)
	require.True(t, ok)
	now := time.Now().UTC().Truncate(time.Millisecond)
	claim, err := p.ClaimIssuePublication(ctx, f.Project.ID, f.Issue.ID, "notion", now, now.Add(-time.Hour), false)
	require.NoError(t, err)
	pending, err := p.BeginIssuePublication(ctx, db.IssuePublicationBegin{Guard: claim.Guard, Publication: db.IssuePublication{AttemptUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Actor: "worker", Revision: f.Issue.Revision, Payload: []byte(`{"title":"Native title","markdown":""}`)}})
	require.NoError(t, err)
	return p, claim, pending
}

func publicationRemote() db.IssuePublicationRemote {
	return db.IssuePublicationRemote{ExternalID: "page:example-page", URL: "https://www.notion.so/example-page", Status: "open", RawStatus: new("todo"), Version: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
}

func checkIssuePublishAttach(t *testing.T, store db.Storage, _ Backend) error {
	p, claim, pending := publicationFixture(t, store, `{"status_sync":"two-way"}`)
	ctx := t.Context()
	require.NoError(t, p.DispatchIssuePublication(ctx, claim.Guard, pending.AttemptUID))
	remote := publicationRemote()
	require.NoError(t, p.RecordIssuePublicationCreated(ctx, claim.Guard, pending.AttemptUID, remote))
	saved, err := p.PendingIssuePublication(ctx, claim.Guard.ProjectID)
	require.NoError(t, err)
	require.Equal(t, "created", saved.Phase)
	require.Equal(t, remote.ExternalID, saved.Remote.ExternalID)
	_, _, _, err = store.CloseIssue(ctx, claim.Issue.ID, "done", "worker", "Closed during publication", nil)
	require.NoError(t, err)
	_, latest, _, err := store.ReopenIssue(ctx, claim.Issue.ID, "worker")
	require.NoError(t, err)
	mapping, _, err := p.AttachIssuePublication(ctx, db.IssuePublicationAttach{Guard: claim.Guard, AttemptUID: pending.AttemptUID, Remote: remote})
	require.NoError(t, err)
	require.Equal(t, claim.Issue.ID, *mapping.IssueID)
	require.Equal(t, claim.Binding.SourceKey, mapping.Source)
	require.Equal(t, remote.ExternalID, mapping.ExternalID)
	saved, err = p.PendingIssuePublication(ctx, claim.Guard.ProjectID)
	require.NoError(t, err)
	require.Nil(t, saved)
	current, err := store.IssueByID(ctx, claim.Issue.ID)
	require.NoError(t, err)
	require.Equal(t, "Native title", current.Title)
	require.Equal(t, "native-author", current.Author)
	state, err := store.(db.IssueStatusReader).IssueStatusMappingByID(ctx, db.IssueSyncImportGuard{BindingID: claim.Binding.ID, Provider: "notion", StartedAt: claim.Guard.StartedAt, BindingUpdatedAt: &claim.Binding.UpdatedAt}, mapping.ID)
	require.NoError(t, err)
	require.Equal(t, latest.UID, state.State.PendingEventUID)
	require.Equal(t, "todo", *state.State.Observed.Raw)
	require.NoError(t, p.ReleaseIssuePublication(ctx, claim.Guard, false))
	replay, err := p.ClaimIssuePublication(ctx, claim.Guard.ProjectID, claim.Issue.ID, "notion", time.Now().UTC(), time.Now().Add(-time.Hour), false)
	require.NoError(t, err)
	require.NotNil(t, replay.Mapping)
	require.Equal(t, mapping.ID, replay.Mapping.ID)
	return nil
}

func checkIssuePublishLegacyAliasCollision(t *testing.T, store db.Storage, _ Backend) error {
	ctx := t.Context()
	fixture, err := createIssueFixture(ctx, store, "example-project", "Existing mapped issue", "worker", nil)
	require.NoError(t, err)
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.Project.ID, Title: "Adoption target", Author: "worker"})
	require.NoError(t, err)
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: fixture.Project.ID, Provider: "github", SourceKey: "github:example-source", RemoteID: "example-repo", DisplayName: "Example repository", Config: []byte(`{}`), IntervalSeconds: 60})
	require.NoError(t, err)
	legacy, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: fixture.Project.ID, Source: binding.SourceKey, ObjectType: "issue", ExternalID: "issue:I_example", IssueID: &fixture.Issue.ID})
	require.NoError(t, err)
	_, err = store.DisableIssueSyncBinding(ctx, fixture.Project.ID)
	require.NoError(t, err)
	publication, ok := store.(db.IssuePublicationStore)
	require.True(t, ok)
	now := time.Now().UTC().Truncate(time.Millisecond)
	claim, err := publication.ClaimIssuePublication(ctx, fixture.Project.ID, target.ID, "github", now, now.Add(-time.Hour), true)
	require.NoError(t, err)
	var remote db.IssuePublicationRemote
	require.NoError(t, json.Unmarshal([]byte(`{"external_id":"issue-id:456","legacy_external_ids":["issue:I_example"],"status":"open"}`), &remote))
	_, _, err = publication.AttachIssuePublication(ctx, db.IssuePublicationAttach{Guard: claim.Guard, Remote: remote, RestoreAdoption: true})
	require.Error(t, err, "adoption must reject a legacy GitHub identity already owned by another issue")
	require.NoError(t, publication.ReleaseIssuePublication(ctx, claim.Guard, false))
	_, err = store.ImportMappingBySource(ctx, fixture.Project.ID, binding.SourceKey, "issue", "issue-id:456")
	require.ErrorIs(t, err, db.ErrNotFound, "rejected adoption must not create a canonical mapping for the other issue")
	gotLegacy, err := store.ImportMappingBySource(ctx, fixture.Project.ID, binding.SourceKey, "issue", "issue:I_example")
	require.NoError(t, err)
	require.Equal(t, legacy.ID, gotLegacy.ID)
	require.Equal(t, &fixture.Issue.ID, gotLegacy.IssueID)
	return nil
}

func checkIssuePublishImportFence(t *testing.T, store db.Storage, backend Backend) error {
	p, claim, pending := publicationFixture(t, store, `{}`)
	ctx := t.Context()
	require.NoError(t, p.DispatchIssuePublication(ctx, claim.Guard, pending.AttemptUID))
	// Generic import has no runner guard, but must still respect source ownership.
	other := backend.OpenSame(t, store)
	now := time.Now().UTC().Add(time.Hour)
	batch := db.ImportBatchParams{ProjectID: claim.Issue.ProjectID, Source: claim.Binding.SourceKey, Actor: "worker", Items: []db.ImportItem{{ExternalID: "page:example-page", Title: "Remote title", Author: "remote-author", Status: "open", CreatedAt: now, UpdatedAt: now}}}
	_, _, err := other.ImportBatch(ctx, batch)
	require.Error(t, err, "pending publication must prevent duplicate native issue through generic import")
	remote := publicationRemote()
	require.NoError(t, p.RecordIssuePublicationCreated(ctx, claim.Guard, pending.AttemptUID, remote))
	_, _, err = p.AttachIssuePublication(ctx, db.IssuePublicationAttach{Guard: claim.Guard, AttemptUID: pending.AttemptUID, Remote: remote})
	require.NoError(t, err)
	result, _, err := other.ImportBatch(ctx, batch)
	require.NoError(t, err)
	require.Zero(t, result.Created)
	require.Equal(t, 1, result.Updated)
	mapping, err := other.ImportMappingBySource(ctx, claim.Issue.ProjectID, claim.Binding.SourceKey, "issue", remote.ExternalID)
	require.NoError(t, err)
	require.Equal(t, claim.Issue.ID, *mapping.IssueID)
	return nil
}

func checkIssuePublishConflict(t *testing.T, store db.Storage, _ Backend) error {
	p, claim, pending := publicationFixture(t, store, `{}`)
	ctx := t.Context()
	other, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: claim.Issue.ProjectID, Title: "Other", Author: "worker"})
	require.NoError(t, err)
	remote := publicationRemote()
	existing, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: claim.Issue.ProjectID, Source: claim.Binding.SourceKey, ObjectType: "issue", ExternalID: remote.ExternalID, IssueID: &other.ID})
	require.NoError(t, err)
	require.NoError(t, p.DispatchIssuePublication(ctx, claim.Guard, pending.AttemptUID))
	require.NoError(t, p.RecordIssuePublicationCreated(ctx, claim.Guard, pending.AttemptUID, remote))
	_, _, err = p.AttachIssuePublication(ctx, db.IssuePublicationAttach{Guard: claim.Guard, AttemptUID: pending.AttemptUID, Remote: remote})
	require.Error(t, err)
	actual, err := store.ImportMappingBySource(ctx, claim.Issue.ProjectID, claim.Binding.SourceKey, "issue", remote.ExternalID)
	require.NoError(t, err)
	require.Equal(t, existing.ID, actual.ID)
	require.Equal(t, other.ID, *actual.IssueID)
	saved, err := p.PendingIssuePublication(ctx, claim.Issue.ProjectID)
	require.NoError(t, err)
	require.Equal(t, pending.AttemptUID, saved.AttemptUID)
	return nil
}

func checkIssuePublishDisableRecovery(t *testing.T, store db.Storage, _ Backend) error {
	p, claim, pending := publicationFixture(t, store, `{}`)
	ctx := t.Context()
	require.NoError(t, p.DispatchIssuePublication(ctx, claim.Guard, pending.AttemptUID))
	_, err := store.DisableIssueSyncBinding(ctx, claim.Issue.ProjectID)
	require.NoError(t, err)
	remote := publicationRemote()
	require.NoError(t, p.RecordIssuePublicationCreated(ctx, claim.Guard, pending.AttemptUID, remote), "disable must not discard known remote result")
	_, _, err = p.AttachIssuePublication(ctx, db.IssuePublicationAttach{Guard: claim.Guard, AttemptUID: pending.AttemptUID, Remote: remote})
	require.Error(t, err, "old configuration fence cannot attach")
	require.NoError(t, p.ReleaseIssuePublication(ctx, claim.Guard, false))
	now := time.Now().UTC().Add(time.Second)
	recovery, err := p.ClaimIssuePublication(ctx, claim.Issue.ProjectID, claim.Issue.ID, "notion", now, now.Add(-time.Hour), true)
	require.NoError(t, err)
	_, _, err = p.AttachIssuePublication(ctx, db.IssuePublicationAttach{Guard: recovery.Guard, AttemptUID: pending.AttemptUID, Remote: remote})
	require.NoError(t, err)
	binding, err := store.IssueSyncBindingByProject(ctx, claim.Issue.ProjectID)
	require.NoError(t, err)
	require.False(t, binding.Enabled)
	return nil
}

func checkIssuePublishResolve(t *testing.T, store db.Storage, _ Backend) error {
	p, claim, pending := publicationFixture(t, store, `{}`)
	ctx := t.Context()
	params := db.IssuePublicationResolve{Guard: claim.Guard, AttemptUID: "01ARZ3NDEKTSV4RRFFQ69G5FAX", Actor: "operator", Message: "Inspected the destination and confirmed no object was created."}
	_, err := p.ResolveIssuePublication(ctx, params)
	require.Error(t, err, "stale attempt cannot clear pending")
	params.AttemptUID = pending.AttemptUID
	params.Message = " \n\t "
	_, err = p.ResolveIssuePublication(ctx, params)
	require.Error(t, err, "inspection evidence is required")
	params.Message = "Inspected the destination and confirmed no object was created."
	events, err := p.ResolveIssuePublication(ctx, params)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "operator", events[0].Actor)
	require.Contains(t, events[0].Payload, params.Message)
	require.Contains(t, events[0].Payload, pending.AttemptUID)
	saved, err := p.PendingIssuePublication(ctx, claim.Issue.ProjectID)
	require.NoError(t, err)
	require.Nil(t, saved)
	_, err = p.ResolveIssuePublication(ctx, params)
	require.Error(t, err, "same attestation cannot clear a later attempt")
	return nil
}

func checkIssuePublishDisabledAdoption(t *testing.T, store db.Storage, _ Backend) error {
	p, claim, pending := publicationFixture(t, store, `{}`)
	ctx := t.Context()
	_, err := p.ResolveIssuePublication(ctx, db.IssuePublicationResolve{Guard: claim.Guard, AttemptUID: pending.AttemptUID, Actor: "worker", Message: "Preparation never dispatched; restoring known upstream identity instead."})
	require.NoError(t, err)
	_, _, err = p.AttachIssuePublication(ctx, db.IssuePublicationAttach{Guard: claim.Guard, Remote: publicationRemote(), RestoreAdoption: true})
	require.Error(t, err, "adoption without pending requires disabled binding")
	_, err = store.DisableIssueSyncBinding(ctx, claim.Issue.ProjectID)
	require.NoError(t, err)
	require.NoError(t, p.ReleaseIssuePublication(ctx, claim.Guard, false))
	now := time.Now().UTC().Add(time.Second)
	recovery, err := p.ClaimIssuePublication(ctx, claim.Issue.ProjectID, claim.Issue.ID, "notion", now, now.Add(-time.Hour), true)
	require.NoError(t, err)
	require.Nil(t, recovery.Pending)
	mapping, _, err := p.AttachIssuePublication(ctx, db.IssuePublicationAttach{Guard: recovery.Guard, Remote: publicationRemote(), RestoreAdoption: true})
	require.NoError(t, err)
	require.Equal(t, claim.Issue.ID, *mapping.IssueID)
	b, err := store.IssueSyncBindingByProject(ctx, claim.Issue.ProjectID)
	require.NoError(t, err)
	require.False(t, b.Enabled)
	return nil
}

func checkIssuePublishPreparedRecovery(t *testing.T, store db.Storage, _ Backend) error {
	p, claim, pending := publicationFixture(t, store, `{}`)
	require.NoError(t, p.ReleaseIssuePublication(t.Context(), claim.Guard, false))
	now := time.Now().UTC().Add(time.Second)
	recovery, err := p.ClaimIssuePublication(t.Context(), claim.Issue.ProjectID, claim.Issue.ID, "notion", now, now.Add(-time.Hour), true)
	require.NoError(t, err)
	require.Error(t, p.DispatchIssuePublication(t.Context(), recovery.Guard, pending.AttemptUID), "recovery of prepared state is not a new create authorization")
	saved, err := p.PendingIssuePublication(t.Context(), claim.Issue.ProjectID)
	require.NoError(t, err)
	require.Equal(t, "prepared", saved.Phase)
	return nil
}

func checkIssuePublishConfigRecovery(t *testing.T, store db.Storage, _ Backend) error {
	p, claim, pending := publicationFixture(t, store, `{}`)
	ctx := t.Context()
	require.NoError(t, p.DispatchIssuePublication(ctx, claim.Guard, pending.AttemptUID))
	_, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: claim.Issue.ProjectID, Provider: "notion", SourceKey: claim.Binding.SourceKey, RemoteID: claim.Binding.RemoteID, DisplayName: "Example renamed", Config: []byte(`{"status_sync":"two-way"}`), IntervalSeconds: 60})
	require.NoError(t, err, "selector/status configuration changes do not delete source identity")
	require.NoError(t, p.RecordIssuePublicationCreated(ctx, claim.Guard, pending.AttemptUID, publicationRemote()))
	_, _, err = p.AttachIssuePublication(ctx, db.IssuePublicationAttach{Guard: claim.Guard, AttemptUID: pending.AttemptUID, Remote: publicationRemote()})
	require.Error(t, err, "old worker is fenced")
	require.NoError(t, p.ReleaseIssuePublication(ctx, claim.Guard, false))
	now := time.Now().UTC().Add(time.Second)
	recovery, err := p.ClaimIssuePublication(ctx, claim.Issue.ProjectID, claim.Issue.ID, "notion", now, now.Add(-time.Hour), true)
	require.NoError(t, err)
	_, _, err = p.AttachIssuePublication(ctx, db.IssuePublicationAttach{Guard: recovery.Guard, AttemptUID: pending.AttemptUID, Remote: publicationRemote()})
	require.NoError(t, err, "explicit source-validated recovery must remain usable after config refresh")
	return nil
}

func checkIssuePublishInputFences(t *testing.T, store db.Storage, _ Backend) error {
	p, claim, pending := publicationFixture(t, store, `{}`)
	ctx := t.Context()
	_, err := p.ResolveIssuePublication(ctx, db.IssuePublicationResolve{Guard: claim.Guard, AttemptUID: pending.AttemptUID, Actor: "worker", Message: "Prepared only; no network dispatch occurred."})
	require.NoError(t, err)
	payload := []byte(`{"title":"` + strings.Repeat("界", 700000) + `"}`)
	params := db.IssuePublicationBegin{Guard: claim.Guard, Publication: db.IssuePublication{AttemptUID: pending.AttemptUID, Actor: "worker", Revision: claim.Issue.Revision, Payload: payload}}
	_, err = p.BeginIssuePublication(ctx, params)
	require.Error(t, err, "serialized UTF-8 size exceeds 2 MiB")
	saved, err := p.PendingIssuePublication(ctx, claim.Issue.ProjectID)
	require.NoError(t, err)
	require.Nil(t, saved)
	params.Publication.Payload = []byte(`{}`)
	params.Publication.Revision++
	_, err = p.BeginIssuePublication(ctx, params)
	require.Error(t, err, "changed issue revision cannot freeze stale body")
	params.Publication.Revision = claim.Issue.Revision
	_, _, _, err = store.CloseIssue(ctx, claim.Issue.ID, "done", "worker", "Closed before publication", nil)
	require.NoError(t, err)
	current, err := store.IssueByID(ctx, claim.Issue.ID)
	require.NoError(t, err)
	params.Publication.Revision = current.Revision
	_, err = p.BeginIssuePublication(ctx, params)
	require.Error(t, err, "closed issues cannot create")
	require.NoError(t, p.ReleaseIssuePublication(ctx, claim.Guard, false))
	_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: claim.Issue.ProjectID, Source: claim.Binding.SourceKey, ObjectType: "issue", ExternalID: "page:existing", IssueID: &claim.Issue.ID})
	require.NoError(t, err)
	now := time.Now().UTC().Add(time.Second)
	existing, err := p.ClaimIssuePublication(ctx, claim.Issue.ProjectID, claim.Issue.ID, "notion", now, now.Add(-time.Hour), false)
	require.NoError(t, err, "mapped closed issue is an idempotent read")
	require.NotNil(t, existing.Mapping)
	return nil
}

func checkIssuePublishConcurrentClaim(t *testing.T, store db.Storage, backend Backend) error {
	p, claim, pending := publicationFixture(t, store, `{}`)
	ctx := t.Context()
	_, err := p.ResolveIssuePublication(ctx, db.IssuePublicationResolve{Guard: claim.Guard, AttemptUID: pending.AttemptUID, Actor: "worker", Message: "Preparation only; release to test independent publishers."})
	require.NoError(t, err)
	require.NoError(t, p.ReleaseIssuePublication(ctx, claim.Guard, false))
	other := backend.OpenSame(t, store).(db.IssuePublicationStore)
	now := time.Now().UTC().Add(time.Second)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, candidate := range []db.IssuePublicationStore{p, other} {
		go func() {
			<-start
			_, err := candidate.ClaimIssuePublication(ctx, claim.Issue.ProjectID, claim.Issue.ID, "notion", now, now.Add(-time.Hour), false)
			results <- err
		}()
	}
	close(start)
	a, b := <-results, <-results
	require.True(t, (a == nil) != (b == nil), "exactly one publisher owns the binding: %v, %v", a, b)
	if a != nil {
		require.ErrorIs(t, a, db.ErrIssueSyncAlreadyRunning)
	}
	if b != nil {
		require.ErrorIs(t, b, db.ErrIssueSyncAlreadyRunning)
	}
	return nil
}

func checkIssuePublishMappedDuringPending(t *testing.T, store db.Storage, _ Backend) error {
	p, claim, _ := publicationFixture(t, store, `{}`)
	ctx := t.Context()
	other, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: claim.Issue.ProjectID, Title: "Already published", Author: "worker"})
	require.NoError(t, err)
	m, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: other.ProjectID, Source: claim.Binding.SourceKey, ObjectType: "issue", ExternalID: "page:existing", IssueID: &other.ID})
	require.NoError(t, err)
	now := time.Now().UTC()
	result, err := p.ClaimIssuePublication(ctx, other.ProjectID, other.ID, "notion", now, now.Add(-time.Hour), false)
	require.NoError(t, err, "already-published read does not compete with another pending create")
	require.NotNil(t, result.Mapping)
	require.Equal(t, m.ID, result.Mapping.ID)
	return nil
}

func checkIssuePublishOneWayAndBody(t *testing.T, store db.Storage, _ Backend) error {
	p, claim, pending := publicationFixture(t, store, `{}`)
	ctx := t.Context()
	require.NoError(t, p.DispatchIssuePublication(ctx, claim.Guard, pending.AttemptUID))
	_, _, _, err := store.EditIssue(ctx, db.EditIssueParams{IssueID: claim.Issue.ID, Body: new("Edited locally during create"), Actor: "worker"})
	require.NoError(t, err)
	_, _, _, err = store.CloseIssue(ctx, claim.Issue.ID, "done", "worker", "Closed locally during create", nil)
	require.NoError(t, err)
	require.NoError(t, p.RecordIssuePublicationCreated(ctx, claim.Guard, pending.AttemptUID, publicationRemote()))
	m, _, err := p.AttachIssuePublication(ctx, db.IssuePublicationAttach{Guard: claim.Guard, AttemptUID: pending.AttemptUID, Remote: publicationRemote()})
	require.NoError(t, err)
	current, err := store.IssueByID(ctx, claim.Issue.ID)
	require.NoError(t, err)
	require.Equal(t, "Edited locally during create", current.Body)
	require.Equal(t, "closed", current.Status, "attachment does not apply a synthetic reopen")
	state, err := store.(db.IssueStatusReader).IssueStatusMappingByID(ctx, db.IssueSyncImportGuard{BindingID: claim.Binding.ID, Provider: "notion", StartedAt: claim.Guard.StartedAt, BindingUpdatedAt: &claim.Binding.UpdatedAt}, m.ID)
	require.NoError(t, err)
	require.Empty(t, state.State.PendingEventUID, "one-way mode does not acquire writeback authority")
	return nil
}

func checkIssuePublishOwnership(t *testing.T, store db.Storage, _ Backend) error {
	p, claim, pending := publicationFixture(t, store, `{}`)
	ctx := t.Context()
	_, err := p.ResolveIssuePublication(ctx, db.IssuePublicationResolve{Guard: claim.Guard, AttemptUID: pending.AttemptUID, Actor: "worker", Message: "Prepared only; release before checking source ownership."})
	require.NoError(t, err)
	require.NoError(t, p.ReleaseIssuePublication(ctx, claim.Guard, false))
	_, _, err = store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{ProjectID: claim.Issue.ProjectID, IssueID: claim.Issue.ID, ConnectorInstance: "notes", ExternalRootKey: "example-root", ExternalAccountKey: "example-account", Actor: "worker", ReceiveCommentsAfter: time.Now().UTC()})
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = p.ClaimIssuePublication(ctx, claim.Issue.ProjectID, claim.Issue.ID, "notion", now, now.Add(-time.Hour), false)
	require.ErrorIs(t, err, db.ErrExternalRootIssueSyncConflict)
	other, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: claim.Issue.ProjectID, Title: "Other source", Author: "worker"})
	require.NoError(t, err)
	_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: other.ProjectID, Source: "other:source", ObjectType: "issue", ExternalID: "item:existing", IssueID: &other.ID})
	require.NoError(t, err)
	_, err = p.ClaimIssuePublication(ctx, other.ProjectID, other.ID, "notion", now, now.Add(-time.Hour), false)
	require.Error(t, err)
	second, err := store.CreateProject(ctx, "second-project")
	require.NoError(t, err)
	_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: second.ID, Provider: "notion", SourceKey: "notion:second", RemoteID: "second", DisplayName: "Second source", Config: []byte(`{}`), IntervalSeconds: 60})
	require.NoError(t, err)
	unowned, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: claim.Issue.ProjectID, Title: "Unmapped native issue", Author: "worker"})
	require.NoError(t, err)
	_, err = p.ClaimIssuePublication(ctx, second.ID, unowned.ID, "notion", now, now.Add(-time.Hour), false)
	require.Error(t, err, "project mismatch fails despite a valid binding and unowned issue")
	status, err := store.IssueSyncStatusByProject(ctx, second.ID)
	require.NoError(t, err)
	require.Nil(t, status.SyncStartedAt, "rejected cross-project publication never acquires the binding")
	return nil
}

// A successful remote identity must survive a failed follow-up observation.
func checkIssuePublishPartialIdentity(t *testing.T, store db.Storage, _ Backend) error {
	p, claim, pending := publicationFixture(t, store, `{}`)
	require.NoError(t, p.DispatchIssuePublication(t.Context(), claim.Guard, pending.AttemptUID))
	remote := db.IssuePublicationRemote{ExternalID: "page:remote"}
	require.NoError(t, p.RecordIssuePublicationCreated(t.Context(), claim.Guard, pending.AttemptUID, remote))
	saved, err := p.PendingIssuePublication(t.Context(), claim.Guard.ProjectID)
	require.NoError(t, err)
	require.Equal(t, "created", saved.Phase)
	require.Equal(t, remote.ExternalID, saved.Remote.ExternalID)
	_, _, err = p.AttachIssuePublication(t.Context(), db.IssuePublicationAttach{Guard: claim.Guard, AttemptUID: pending.AttemptUID, Remote: remote})
	require.Error(t, err, "attachment still requires a validated observation")
	return nil
}

func checkIssuePublishFederatedIntent(t *testing.T, store db.Storage, _ Backend) error {
	p, claim, pending := publicationFixture(t, store, `{"status_sync":"two-way"}`)
	ctx := t.Context()
	_, err := store.EnableProjectFederation(ctx, claim.Guard.ProjectID, "worker")
	require.NoError(t, err)
	project, err := store.ProjectByID(ctx, claim.Guard.ProjectID)
	require.NoError(t, err)
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
	require.NoError(t, err)
	var base int64
	for _, e := range events {
		if e.HLCPhysicalMS > base {
			base = e.HLCPhysicalMS
		}
	}
	spoke := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	closeEvent := newRemoteEvent(t, project, &claim.Issue.UID, "issue.closed", "worker", spoke, base+10, []byte(`{"reason":"done","closed_at":"2026-10-18T12:00:00Z"}`))
	restate := newRemoteEvent(t, project, &claim.Issue.UID, "issue.updated", "worker", spoke, base+30, []byte(`{"status":"closed","closed_reason":"done","closed_at":"2026-10-18T12:00:00Z"}`))
	historical := newRemoteEvent(t, project, &claim.Issue.UID, "issue.reopened", "worker", spoke, base+20, []byte(`{"reopened_at":"2026-10-18T12:00:00Z"}`))
	for index, e := range []db.RemoteEvent{closeEvent, restate, historical} {
		_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: spoke, BoundActor: "worker", Events: []db.FederationIngestEvent{{SourceEventID: int64(index + 1), Event: e}}})
		require.NoError(t, err, "ingest event %d", index)
	}
	require.NoError(t, p.DispatchIssuePublication(ctx, claim.Guard, pending.AttemptUID))
	mapping, _, err := p.AttachIssuePublication(ctx, db.IssuePublicationAttach{Guard: claim.Guard, AttemptUID: pending.AttemptUID, Remote: publicationRemote()})
	require.NoError(t, err)
	observed, err := store.(db.IssueStatusReader).IssueStatusMappingByID(ctx, db.IssueSyncImportGuard{BindingID: claim.Binding.ID, Provider: "notion", StartedAt: claim.Guard.StartedAt}, mapping.ID)
	require.NoError(t, err)
	require.Equal(t, closeEvent.EventUID, observed.State.PendingEventUID, "late historical opposite event is not a fresh outbound intent")
	return nil
}

// Leaving two-way mode revokes pending writeback intent before a later mode
// switch or publication recovery can turn it into an outbound mapping event.
func checkIssuePublishModeChangeDiscardsIntent(t *testing.T, store db.Storage, _ Backend) error {
	ctx := t.Context()
	publications, claim, attempt := publicationFixture(t, store, `{"status_sync":"two-way"}`)
	require.NoError(t, publications.DispatchIssuePublication(ctx, claim.Guard, attempt.AttemptUID))
	_, closed, _, err := store.CloseIssue(ctx, claim.Issue.ID, "done", "worker", "Closed during publication", nil)
	require.NoError(t, err)
	before, err := publications.PendingIssuePublication(ctx, claim.Guard.ProjectID)
	require.NoError(t, err)
	require.Equal(t, closed.UID, before.PendingEventUID)
	setMode := func(mode string) {
		_, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{
			ProjectID: claim.Guard.ProjectID, Provider: claim.Binding.Provider,
			SourceKey: claim.Binding.SourceKey, RemoteID: claim.Binding.RemoteID,
			DisplayName: claim.Binding.DisplayName, IntervalSeconds: claim.Binding.IntervalSeconds,
			Config: []byte(`{"status_sync":"` + mode + `"}`),
		})
		require.NoError(t, err)
	}
	setMode("one-way")
	after, err := publications.PendingIssuePublication(ctx, claim.Guard.ProjectID)
	require.NoError(t, err)
	require.Empty(t, after.PendingEventUID, "one-way mode must revoke publication writeback intent")
	before.PendingEventUID = ""
	require.Equal(t, before, after, "mode change preserves the frozen request and recovery identity")
	_, _, _, err = store.ReopenIssue(ctx, claim.Issue.ID, "worker")
	require.NoError(t, err)
	setMode("two-way")
	require.NoError(t, publications.ReleaseIssuePublication(ctx, claim.Guard, false))
	now := time.Now().UTC().Add(time.Second)
	recovery, err := publications.ClaimIssuePublication(ctx, claim.Guard.ProjectID, claim.Issue.ID, "notion", now, now.Add(-time.Hour), true)
	require.NoError(t, err)
	require.Empty(t, recovery.Pending.PendingEventUID, "switching back must not resurrect old intent")
	mapping, _, err := publications.AttachIssuePublication(ctx, db.IssuePublicationAttach{
		Guard: recovery.Guard, AttemptUID: attempt.AttemptUID, Remote: publicationRemote(),
	})
	require.NoError(t, err)
	current, err := store.IssueByID(ctx, claim.Issue.ID)
	require.NoError(t, err)
	require.Equal(t, "open", current.Status)
	state, err := store.(db.IssueStatusReader).IssueStatusMappingByID(ctx, db.IssueSyncImportGuard{
		BindingID: claim.Binding.ID, Provider: "notion", StartedAt: recovery.Guard.StartedAt,
	}, mapping.ID)
	require.NoError(t, err)
	require.Empty(t, state.State.PendingEventUID)
	require.Nil(t, state.PendingEvent, "recovery must not queue the obsolete close for delivery")
	require.NoError(t, publications.ReleaseIssuePublication(ctx, recovery.Guard, false))
	return nil
}

// Every accepted request must leave room to retain a confirmed remote identity.
func checkIssuePublishCapacityRecovery(t *testing.T, store db.Storage, backend Backend) error {
	p, claim, pending := publicationFixture(t, store, `{}`)
	ctx := t.Context()
	_, err := p.ResolveIssuePublication(ctx, db.IssuePublicationResolve{Guard: claim.Guard, AttemptUID: pending.AttemptUID, Actor: "worker", Message: "Prepared request was never dispatched."})
	require.NoError(t, err)
	pending.Payload = []byte(`{"body":"` + strings.Repeat("x", 2*1024*1024-2048) + `"}`)
	pending, err = p.BeginIssuePublication(ctx, db.IssuePublicationBegin{Guard: claim.Guard, Publication: pending})
	require.NoError(t, err, "the frozen request fits the journal")
	require.NoError(t, p.DispatchIssuePublication(ctx, claim.Guard, pending.AttemptUID))
	remote := publicationRemote()
	remote.URL = "https://tracker.example/" + strings.Repeat("a", 2048)
	require.NoError(t, p.RecordIssuePublicationCreated(ctx, claim.Guard, pending.AttemptUID, remote), "an accepted request must retain its confirmed identity")
	other := backend.OpenSame(t, store).(db.IssuePublicationStore)
	saved, err := other.PendingIssuePublication(ctx, claim.Issue.ProjectID)
	require.NoError(t, err)
	require.Equal(t, "created", saved.Phase)
	require.Equal(t, remote, *saved.Remote)
	require.Equal(t, pending.PayloadDigest, saved.PayloadDigest)
	raw, err := json.Marshal(saved)
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), 2*1024*1024)
	return nil
}
