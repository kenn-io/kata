package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/uid"
)

func federatedRehomeFixture(t *testing.T, store db.Storage, suffix string) (db.Project, db.Project, db.Issue, db.RemoteEvent) {
	t.Helper()
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "rehome-source-"+suffix)
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "rehome-target-"+suffix)
	require.NoError(t, err)
	issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Original vectors", Body: "move without regeneration", Author: "worker"})
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		_, err = store.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
	}
	origin, err := uid.New()
	require.NoError(t, err)
	at := time.UnixMilli(max(time.Now().UTC().UnixMilli(), created.HLCPhysicalMS) + 1000).UTC()
	raw, err := json.Marshal(map[string]any{"issue_uid": issue.UID, "from_project_uid": source.UID, "from_short_id": issue.ShortID, "to_project_uid": target.UID, "to_short_id": issue.ShortID, "updated_at": at.Format(time.RFC3339Nano), "snapshot": map[string]string{"uid": issue.UID, "short_id": issue.ShortID, "title": issue.Title, "body": issue.Body, "author": issue.Author, "status": "open"}})
	require.NoError(t, err)
	event := newRemoteEvent(t, target, &issue.UID, "issue.moved", "worker", origin, at.UnixMilli(), jsontext.Value(raw))
	return source, target, issue, event
}

func prepareRehomePull(t *testing.T, store db.Storage, source, target db.Project, mode string) {
	t.Helper()
	if mode != "pull" {
		return
	}
	for _, project := range []db.Project{source, target} {
		_, err := store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true})
		require.NoError(t, err)
	}
}

func replayFederatedRehome(t *testing.T, store db.Storage, target db.Project, event db.RemoteEvent, mode string) {
	t.Helper()
	if mode == "push" {
		_, err := store.IngestFederationEvents(t.Context(), db.FederationIngestParams{ProjectID: target.ID, SpokeInstanceUID: event.OriginInstanceUID, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}})
		require.NoError(t, err)
	} else {
		_, err := store.InsertRemoteEvents(t.Context(), target.ID, []db.RemoteEvent{event})
		require.NoError(t, err)
		require.NoError(t, store.MaterializeFederatedProject(t.Context(), target.ID))
	}
}

func checkFederatedMoveReplayAccessRevision(t *testing.T, store db.Storage) error {
	for _, mode := range []string{"push", "pull"} {
		t.Run(mode, func(t *testing.T) {
			source, target, issue, event := federatedRehomeFixture(t, store, "revision-"+mode)
			prepareRehomePull(t, store, source, target, mode)
			access := store.(db.ProjectAccessStorage)
			before, err := access.ProjectAccessRevision(t.Context())
			require.NoError(t, err)
			replayFederatedRehome(t, store, target, event, mode)
			moved, err := store.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Equal(t, target.ID, moved.ProjectID)
			after, err := access.ProjectAccessRevision(t.Context())
			require.NoError(t, err)
			require.Greater(t, after, before, "replay ownership changes must invalidate admitted restricted reads")
			replayFederatedRehome(t, store, target, event, mode)
			unchanged, err := access.ProjectAccessRevision(t.Context())
			require.NoError(t, err)
			require.Equal(t, after, unchanged, "a replay which does not rehome again retains the access revision")
		})
	}
	return nil
}

func checkFederatedMoveReplayEmbeddingArtifacts(t *testing.T, store db.Storage) error {
	for _, mode := range []string{"push", "pull"} {
		t.Run(mode, func(t *testing.T) {
			source, target, issue, event := federatedRehomeFixture(t, store, "artifact-"+mode)
			prepareRehomePull(t, store, source, target, mode)
			artifacts := store.(db.EmbeddingArtifactStorage)
			original, err := embedding.NewArtifact(embedding.ArtifactIdentity{ProjectUID: source.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("d", 64)}, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{.25, .75}})
			require.NoError(t, err)
			durable, err := artifacts.RetainEmbeddingArtifact(t.Context(), original)
			require.NoError(t, err)
			require.True(t, durable)
			replayFederatedRehome(t, store, target, event, mode)
			rebound, err := embedding.RebindArtifactProject(original, target.UID)
			require.NoError(t, err)
			got, err := artifacts.StoredEmbeddingArtifact(t.Context(), target.UID, rebound.Digest)
			require.NoError(t, err, "replay must retain portable artifacts in the current project")
			require.Equal(t, rebound, got)
			require.Equal(t, original.Chunks[0].VectorBytes, got.Chunks[0].VectorBytes)
			_, err = artifacts.StoredEmbeddingArtifact(t.Context(), source.UID, original.Digest)
			require.ErrorIs(t, err, db.ErrNotFound)
			manifests, err := artifacts.EmbeddingArtifactManifests(t.Context(), target.UID, 10)
			require.NoError(t, err)
			require.Len(t, manifests, 1)
			exports := 0
			for record, exportErr := range store.(db.EmbeddingArtifactExporter).ExportEmbeddingArtifacts(t.Context(), db.ExportFilter{ProjectID: &target.ID}) {
				require.NoError(t, exportErr)
				exported, ok := record.(*db.EmbeddingArtifactExport)
				require.True(t, ok)
				require.Equal(t, rebound, exported.EmbeddingArtifact)
				exports++
			}
			require.Equal(t, 1, exports)
		})
	}
	return nil
}
