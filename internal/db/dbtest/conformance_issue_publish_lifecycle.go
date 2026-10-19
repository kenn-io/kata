package dbtest

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Pending publication must keep its exact identities addressable until recovery.
// Each operation runs through a second connection, not a handler preflight.
func checkIssuePublishLifecycleFence(t *testing.T, _ db.Storage, backend Backend) error {
	actions := map[string]func(context.Context, db.Storage, db.IssuePublicationClaim, int64) error{
		"delete issue": func(ctx context.Context, s db.Storage, c db.IssuePublicationClaim, _ int64) error {
			_, _, _, err := s.SoftDeleteIssue(ctx, c.Issue.ID, "worker")
			return err
		},
		"purge issue": func(ctx context.Context, s db.Storage, c db.IssuePublicationClaim, _ int64) error {
			_, err := s.PurgeIssue(ctx, c.Issue.ID, "worker", nil)
			return err
		},
		"move issue": func(ctx context.Context, s db.Storage, c db.IssuePublicationClaim, target int64) error {
			_, err := s.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: c.Issue.ID, FromProjectID: c.Issue.ProjectID, ToProjectID: target, IfMatchRev: c.Issue.Revision, Actor: "worker"})
			return err
		},
		"archive project": func(ctx context.Context, s db.Storage, c db.IssuePublicationClaim, _ int64) error {
			_, _, err := s.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: c.Issue.ProjectID, Actor: "worker", Force: true})
			return err
		},
		"purge project": func(ctx context.Context, s db.Storage, c db.IssuePublicationClaim, _ int64) error {
			_, err := s.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: c.Issue.ProjectID, Actor: "worker"})
			return err
		},
		"hard delete project": func(ctx context.Context, s db.Storage, c db.IssuePublicationClaim, _ int64) error {
			_, err := s.HardDeleteProject(ctx, c.Issue.ProjectID)
			return err
		},
		"merge project": func(ctx context.Context, s db.Storage, c db.IssuePublicationClaim, target int64) error {
			_, err := s.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: c.Issue.ProjectID, TargetProjectID: target, Actor: "worker"})
			return err
		},
		"retarget binding": func(ctx context.Context, s db.Storage, c db.IssuePublicationClaim, _ int64) error {
			_, err := s.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: c.Issue.ProjectID, Provider: "notion", SourceKey: "notion:replacement", RemoteID: "replacement", DisplayName: "Replacement", Config: []byte(`{}`), IntervalSeconds: 60})
			return err
		},
		"attach another source": func(ctx context.Context, s db.Storage, c db.IssuePublicationClaim, _ int64) error {
			_, err := s.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: c.Issue.ProjectID, Source: "other:source", ObjectType: "issue", ExternalID: "item:other", IssueID: &c.Issue.ID})
			return err
		},
		"bind external root": func(ctx context.Context, s db.Storage, c db.IssuePublicationClaim, _ int64) error {
			_, _, err := s.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{ProjectID: c.Issue.ProjectID, IssueID: c.Issue.ID, ConnectorInstance: "notes", ExternalRootKey: "example-root", ExternalAccountKey: "example-account", Actor: "worker", ReceiveCommentsAfter: time.Now().UTC()})
			return err
		},
	}
	for _, phase := range []string{"prepared", "dispatching", "created"} {
		for name, action := range actions {
			t.Run(phase+"/"+name, func(t *testing.T) {
				store := backend.Open(t)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				p, c, pending := publicationFixture(t, store, `{}`)
				ctx := t.Context()
				if phase != "prepared" {
					require.NoError(t, p.DispatchIssuePublication(ctx, c.Guard, pending.AttemptUID))
				}
				if phase == "created" {
					require.NoError(t, p.RecordIssuePublicationCreated(ctx, c.Guard, pending.AttemptUID, publicationRemote()))
				}
				target, err := store.CreateProject(ctx, "target-project")
				require.NoError(t, err)
				err = action(ctx, backend.OpenSame(t, store), c, target.ID)
				require.Error(t, err)
				require.Contains(t, err.Error(), pending.AttemptUID, "refusal must identify recoverable pending attempt")
				require.Contains(t, err.Error(), "example-project#"+c.Issue.ShortID, "recovery guidance must retain the pending issue's project scope")
				saved, err := p.PendingIssuePublication(ctx, c.Issue.ProjectID)
				require.NoError(t, err)
				require.Equal(t, pending.AttemptUID, saved.AttemptUID)
				_, err = store.IssueByID(ctx, c.Issue.ID)
				require.NoError(t, err, "issue remains addressable")
			})
		}
	}
	return nil
}

func checkIssuePublishLifecycleRace(t *testing.T, _ db.Storage, backend Backend) error {
	for _, kind := range []string{"delete", "move", "archive", "hard-delete"} {
		t.Run(kind, func(t *testing.T) {
			store := backend.Open(t)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			p, c, pending := publicationFixture(t, store, `{}`)
			ctx := t.Context()
			_, err := p.ResolveIssuePublication(ctx, db.IssuePublicationResolve{Guard: c.Guard, AttemptUID: pending.AttemptUID, Actor: "worker", Message: "Preparation never dispatched; checking lifecycle serialization."})
			require.NoError(t, err)
			target, err := store.CreateProject(ctx, "target-project")
			require.NoError(t, err)
			other := backend.OpenSame(t, store)
			start := make(chan struct{})
			beginResult := make(chan error, 1)
			mutationResult := make(chan error, 1)
			go func() {
				<-start
				_, err := p.BeginIssuePublication(ctx, db.IssuePublicationBegin{Guard: c.Guard, Publication: pending})
				beginResult <- err
			}()
			go func() {
				<-start
				var err error
				switch kind {
				case "delete":
					_, _, _, err = other.SoftDeleteIssue(ctx, c.Issue.ID, "worker")
				case "move":
					_, err = other.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: c.Issue.ID, FromProjectID: c.Issue.ProjectID, ToProjectID: target.ID, IfMatchRev: c.Issue.Revision, Actor: "worker"})
				case "archive":
					_, _, err = other.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: c.Issue.ProjectID, Actor: "worker", Force: true})
				case "hard-delete":
					_, err = other.HardDeleteProject(ctx, c.Issue.ProjectID)
				}
				mutationResult <- err
			}()
			close(start)
			beginErr, mutationErr := <-beginResult, <-mutationResult
			require.True(t, (beginErr == nil) != (mutationErr == nil), "one operation wins: begin=%v mutation=%v", beginErr, mutationErr)
			if beginErr == nil {
				saved, err := p.PendingIssuePublication(ctx, c.Issue.ProjectID)
				require.NoError(t, err)
				require.NotNil(t, saved)
				_, err = store.IssueByID(ctx, c.Issue.ID)
				require.NoError(t, err)
			}
		})
	}
	return nil
}

func checkIssuePublishLifecycleAllowsRecovery(t *testing.T, store db.Storage, _ Backend) error {
	p, c, pending := publicationFixture(t, store, `{}`)
	ctx := t.Context()
	_, err := store.RenameProject(ctx, c.Issue.ProjectID, "renamed-project")
	require.NoError(t, err)
	other, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: c.Issue.ProjectID, Title: "Unrelated issue", Author: "worker"})
	require.NoError(t, err)
	_, _, _, err = store.SoftDeleteIssue(ctx, other.ID, "worker")
	require.NoError(t, err)
	_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: c.Issue.ID, Body: new("Allowed edit"), Actor: "worker"})
	require.NoError(t, err)
	_, err = store.DisableIssueSyncBinding(ctx, c.Issue.ProjectID)
	require.NoError(t, err)
	require.NoError(t, p.ReleaseIssuePublication(ctx, c.Guard, false))
	now := time.Now().UTC().Add(time.Second)
	recovery, err := p.ClaimIssuePublication(ctx, c.Issue.ProjectID, c.Issue.ID, "notion", now, now.Add(-time.Hour), true)
	require.NoError(t, err)
	_, err = p.ResolveIssuePublication(ctx, db.IssuePublicationResolve{Guard: recovery.Guard, AttemptUID: pending.AttemptUID, Actor: "worker", Message: "Prepared request was never dispatched; lifecycle can proceed."})
	require.NoError(t, err)
	_, _, _, err = store.SoftDeleteIssue(ctx, c.Issue.ID, "worker")
	require.NoError(t, err, "resolved pending no longer blocks lifecycle")
	return nil
}

func checkIssuePublishExportIsolation(t *testing.T, store db.Storage, _ Backend) error {
	_, c, _ := publicationFixture(t, store, `{}`)
	for meta, err := range store.ExportMeta(t.Context()) {
		require.NoError(t, err)
		require.False(t, strings.HasPrefix(meta.Key, db.IssuePublicationMetaPrefix), "executable publication state must not leave through JSONL")
	}
	_, err := store.IssueByID(t.Context(), c.Issue.ID)
	require.NoError(t, err)
	return nil
}

func checkIssuePublishReplayAuthority(t *testing.T, store db.Storage, backend Backend) error {
	p, c, pending := publicationFixture(t, store, `{}`)
	ctx := t.Context()
	records, err := CollectImportRecords(ctx, store, db.ExportFilter{})
	require.NoError(t, err)
	var filtered []db.ImportRecord
	for _, record := range records {
		if meta, ok := record.(*db.MetaKV); ok && strings.HasPrefix(meta.Key, db.IssuePublicationMetaPrefix) {
			continue
		}
		filtered = append(filtered, record)
	}
	raw, err := json.Marshal(pending)
	require.NoError(t, err)
	filtered = append(filtered, &db.MetaKV{Key: db.IssuePublicationMetaPrefix + c.Issue.ProjectUID, Value: string(raw)})
	restored := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportReplay(ctx, filtered, db.ImportOptions{NewInstance: true}))
	b, err := restored.IssueSyncBindingByProject(ctx, c.Issue.ProjectID)
	require.NoError(t, err)
	require.False(t, b.Enabled)
	imported, err := restored.(db.IssuePublicationStore).PendingIssuePublication(ctx, c.Issue.ProjectID)
	require.NoError(t, err)
	require.Nil(t, imported, "imported JSON cannot grant a POST")
	// A destructive replay against the serving database must not strand the live
	// pending identity. Raw file/database replacement is a separate operator path.
	err = store.ImportReplay(ctx, []db.ImportRecord{&db.MetaKV{Key: db.IssuePublicationMetaPrefix + c.Issue.ProjectUID, Value: `{"phase":"prepared"}`}}, db.ImportOptions{})
	require.Error(t, err)
	live, err := p.PendingIssuePublication(ctx, c.Issue.ProjectID)
	require.NoError(t, err)
	require.Equal(t, pending.AttemptUID, live.AttemptUID)
	_, err = store.IssueByID(ctx, c.Issue.ID)
	require.NoError(t, err)
	return nil
}

func checkIssuePublishFederatedLifecycle(t *testing.T, store db.Storage, _ Backend) error {
	p, c, pending := publicationFixture(t, store, `{}`)
	ctx := t.Context()
	_, err := store.EnableProjectFederation(ctx, c.Issue.ProjectID, "worker")
	require.NoError(t, err)
	project, err := store.ProjectByID(ctx, c.Issue.ProjectID)
	require.NoError(t, err)
	// Receiving an ordinary remote edit remains permitted while creation is pending.
	origin := "01ARZ3NDEKTSV4RRFFQ69G5FAY"
	clock := time.Now().UTC().Add(time.Minute).UnixMilli()
	edit := newRemoteEvent(t, project, &c.Issue.UID, "issue.updated", "worker", origin, clock, []byte(`{"body":"Remote local-project edit"}`))
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: origin, BoundActor: "worker", Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: edit}}})
	require.NoError(t, err)
	deletion := newRemoteEvent(t, project, &c.Issue.UID, "issue.soft_deleted", "worker", origin, clock+1, []byte(`{"deleted_at":"2026-10-18T22:00:00Z"}`))
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: origin, BoundActor: "worker", Events: []db.FederationIngestEvent{{SourceEventID: 2, Event: deletion}}})
	require.Error(t, err, "federation cannot strand a pending publication by bypassing native delete")
	require.Contains(t, err.Error(), pending.AttemptUID)
	current, err := store.IssueByID(ctx, c.Issue.ID)
	require.NoError(t, err)
	require.Nil(t, current.DeletedAt)
	require.Equal(t, "Remote local-project edit", current.Body)
	err = store.ResetFederatedProject(ctx, project.ID, 0, 0)
	require.Error(t, err, "projection replacement must preserve pending identities")
	saved, err := p.PendingIssuePublication(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, pending.AttemptUID, saved.AttemptUID)
	return nil
}

func checkIssuePublishKnownRemoteMappingFence(t *testing.T, store db.Storage, _ Backend) error {
	p, c, pending := publicationFixture(t, store, `{}`)
	ctx := t.Context()
	other, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: c.Issue.ProjectID, Title: "Other", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, p.DispatchIssuePublication(ctx, c.Guard, pending.AttemptUID))
	remote := publicationRemote()
	require.NoError(t, p.RecordIssuePublicationCreated(ctx, c.Guard, pending.AttemptUID, remote))
	_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: c.Issue.ProjectID, Source: c.Binding.SourceKey, ObjectType: "issue", ExternalID: remote.ExternalID, IssueID: &other.ID})
	require.Error(t, err)
	require.Contains(t, err.Error(), pending.AttemptUID)
	_, _, err = p.AttachIssuePublication(ctx, db.IssuePublicationAttach{Guard: c.Guard, AttemptUID: pending.AttemptUID, Remote: remote})
	require.NoError(t, err)
	return nil
}
