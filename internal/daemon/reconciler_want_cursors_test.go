package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/vector"
)

func TestAdvanceWantCursorsPreservesOmittedAndSkippedProjects(t *testing.T) {
	r := Reconciler{wantCursors: map[string]string{
		"unvisited-project": "last-issue",
		"throttled-project": "page-one-last-issue",
	}}

	r.advanceWantCursors([]vector.ArtifactWant{
		{ProjectUID: "visited-project", IssueUID: "issue-1"},
		{ProjectUID: "throttled-project", IssueUID: "page-two-issue"},
	}, map[string]error{"visited-project": nil})

	require.Equal(t, "last-issue", r.wantCursors["unvisited-project"])
	require.Equal(t, "page-one-last-issue", r.wantCursors["throttled-project"])
}

func TestFetchFederatedArtifactsLogsProjectLookupFailure(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	project, err := store.CreateProject(ctx, "direct-spoke")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: 42, HubProjectUID: project.UID, Actor: "member", Enabled: true,
	})
	require.NoError(t, err)

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	const missingProject = "missing-project"
	r := NewReconciler(store, nil, nil, ReconcilerConfig{
		FetchFederatedArtifacts: func(context.Context, embedding.ArtifactIdentity, []vector.ArtifactWant) map[string]error {
			return map[string]error{missingProject: errors.New("hub fetch failed")}
		},
	})
	r.pendingWants = func(context.Context, string, int, int, map[string]string, string) ([]vector.ArtifactWant, error) {
		return []vector.ArtifactWant{{ProjectUID: missingProject, IssueUID: "issue-1"}}, nil
	}

	got := r.fetchFederatedArtifacts(ctx, "generation", embedding.ArtifactIdentity{})
	require.Equal(t, []string{"issue-1"}, got)
	require.Contains(t, logs.String(), "artifact fetch project lookup failed")
	require.Contains(t, logs.String(), "project_uid=missing-project")
	require.Contains(t, logs.String(), "err=\"not found\"")
}
