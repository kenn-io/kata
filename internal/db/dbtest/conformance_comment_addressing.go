package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkCommentAddressingValidation(t *testing.T, store db.Storage, backend Backend) error {
	fixture, err := createIssueFixture(t.Context(), store, "example-project", "Task", "alice", nil)
	require.NoError(t, err)
	baseline, err := CollectImportRecords(t.Context(), store, db.ExportFilter{})
	require.NoError(t, err)
	for _, kind := range []string{"issue.commented", "issue.snapshot"} {
		for _, invalid := range []string{`null`, `true`, `42`, `[]`, `{}`, `"bad/handle"`} {
			t.Run(kind+"/"+invalid, func(t *testing.T) {
				commentUID, err := uid.New()
				require.NoError(t, err)
				eventUID, err := uid.New()
				require.NoError(t, err)
				entry := fmt.Sprintf(`{"comment_uid":%q,"author":"alice","teammate":"codex","addressing_teammate":%s,"body":"Finding","created_at":"2026-10-26T00:00:00Z"}`, commentUID, invalid)
				payload := entry
				if kind == "issue.snapshot" {
					payload = fmt.Sprintf(`{"uid":%q,"author":"alice","title":"Task","status":"open","created_at":"2026-10-26T00:00:00Z","comments":[%s]}`, fixture.Issue.UID, entry)
				}
				remote := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: store.InstanceUID(), ProjectUID: fixture.Project.UID, ProjectName: fixture.Project.Name, IssueUID: &fixture.Issue.UID, Type: kind, Actor: "alice", HLCPhysicalMS: 1, CreatedAt: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), Payload: jsontext.Value(payload)}
				remote.ContentHash = remoteEventHash(t, remote)
				_, err = store.InsertRemoteEvent(t.Context(), fixture.Project.ID, remote)
				require.ErrorIs(t, err, db.ErrFederationIngestValidation)
				target := backend.Open(t)
				t.Cleanup(func() { require.NoError(t, target.Close()) })
				record := &db.EventExport{ID: 999, UID: eventUID, OriginInstanceUID: remote.OriginInstanceUID, ProjectID: fixture.Project.ID, ProjectUID: fixture.Project.UID, ProjectName: fixture.Project.Name, IssueID: &fixture.Issue.ID, IssueUID: &fixture.Issue.UID, Type: kind, Actor: "alice", HLCPhysicalMS: 1, CreatedAt: remote.CreatedAt.Format(db.EventTimestampFormat), Payload: remote.Payload, ContentHash: remote.ContentHash}
				records := append(append([]db.ImportRecord{}, baseline...), record)
				require.ErrorIs(t, target.ImportReplay(t.Context(), records, db.ImportOptions{}), db.ErrFederationIngestValidation)
			})
		}
	}
	return nil
}

func checkCommentAddressingRoundTrip(t *testing.T, store db.Storage, backend Backend) error {
	fixture, err := createIssueFixture(t.Context(), store, "example-project", "Task", "alice", nil)
	require.NoError(t, err)
	empty, named := "", "named-worker"
	expected := map[string]*string{}
	teammates := map[string]string{}
	for _, value := range []struct {
		label   string
		address *string
	}{{"legacy", nil}, {"codex", &empty}, {"named-worker", &named}} {
		comment, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: fixture.Issue.ID, Author: "alice", Teammate: value.label, AddressingTeammate: value.address, Body: "Finding"})
		require.NoError(t, err)
		expected[comment.UID] = value.address
		teammates[comment.UID] = value.label
	}
	binding, err := store.EnableProjectFederation(t.Context(), fixture.Project.ID, "alice")
	require.NoError(t, err)
	check := func(t *testing.T, target db.Storage) {
		project, err := target.ProjectByUID(t.Context(), fixture.Project.UID)
		require.NoError(t, err)
		issue, err := target.IssueByUID(t.Context(), fixture.Issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		begin := target.(interface {
			BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
		})
		tx, err := begin.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		addresses, err := db.CommentAddressingTeammates(t.Context(), tx, project.ID, issue.ID)
		require.NoError(t, err)
		require.Equal(t, expected, addresses)
		hidden, err := db.CommentAddressingTeammates(t.Context(), tx, project.ID+1000, issue.ID)
		require.NoError(t, err)
		require.Empty(t, hidden)
		require.NoError(t, tx.Rollback())
		comments, err := target.CommentsByIssue(t.Context(), issue.ID)
		require.NoError(t, err)
		require.Len(t, comments, 3)
		for _, comment := range comments {
			require.Equal(t, teammates[comment.UID], comment.Teammate)
		}
		fold, err := foldProjectIssues(t.Context(), target, project.ID)
		require.NoError(t, err)
		for id, want := range expected {
			require.Equal(t, want, fold.Comments[id].AddressingTeammate)
		}
	}
	check(t, store)
	records, err := CollectImportRecords(t.Context(), store, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	for _, mode := range []string{"restore", "cutover"} {
		t.Run(mode, func(t *testing.T) {
			target := backend.Open(t)
			t.Cleanup(func() { require.NoError(t, target.Close()) })
			opts := db.ImportOptions{}
			if mode == "cutover" {
				opts.PreserveIssueSyncBindingEnabled = true
				opts.PreserveExternalRootBindingsEnabled = true
			}
			require.NoError(t, target.ImportReplay(t.Context(), records, opts))
			check(t, target)
		})
	}
	t.Run("snapshot-only federation", func(t *testing.T) {
		peer := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, peer.Close()) })
		local, err := peer.CreateProject(t.Context(), "spoke-project")
		require.NoError(t, err)
		adopted, err := peer.AdoptProjectIntoFederation(t.Context(), db.AdoptProjectIntoFederationParams{ProjectID: local.ID, HubURL: "https://daemon.example", HubProjectID: fixture.Project.ID, HubProjectUID: fixture.Project.UID, ReplayHorizonEventID: binding.ReplayHorizonEventID, Actor: "alice", EmptyOnly: true})
		require.NoError(t, err)
		events, err := store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: fixture.Project.ID, AfterID: binding.ReplayHorizonEventID - 1, Limit: 100})
		require.NoError(t, err)
		snapshots := 0
		for _, event := range events {
			if event.Type != "issue.snapshot" {
				continue
			}
			snapshots++
			inserted, err := peer.InsertRemoteEvent(db.WithEventTeammate(t.Context(), "unrelated"), adopted.Project.ID, remoteEventFromStored(event))
			require.NoError(t, err)
			require.True(t, inserted)
		}
		require.Positive(t, snapshots)
		require.NoError(t, peer.MaterializeFederatedProject(t.Context(), adopted.Project.ID))
		check(t, peer)
		_, _, err = peer.RefreshProjectFederationBaseline(t.Context(), adopted.Project.ID, "alice")
		require.NoError(t, err)
	})
	return nil
}
