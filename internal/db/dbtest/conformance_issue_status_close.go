package dbtest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkIssueStatusProviderCloseMetadata(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	f, err := createIssueFixture(ctx, store, "example-project", "Mapped task", "worker", nil)
	require.NoError(t, err)
	b, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: f.Project.ID, Provider: "github", SourceKey: "github:example-repository", RemoteID: "example-repository", DisplayName: "example-owner/example-repo", Config: []byte(`{}`), IntervalSeconds: 60})
	require.NoError(t, err)
	m, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: f.Project.ID, Source: b.SourceKey, ObjectType: "issue", ExternalID: "issue-id:123", IssueID: &f.Issue.ID})
	require.NoError(t, err)
	at := f.Issue.CreatedAt.Add(time.Hour)
	closedAt := f.Issue.CreatedAt.Add(time.Minute)
	_, ok, err := store.ClaimIssueSyncBinding(ctx, b.ID, b.Provider, at, at.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	p := db.IssueStatusObservationParams{Guard: db.IssueSyncImportGuard{BindingID: b.ID, Provider: b.Provider, StartedAt: at}, MappingID: m.ID, ExternalID: m.ExternalID, IssueUID: f.Issue.UID, Observation: db.IssueStatusObservation{Raw: new("closed"), Version: at}, Status: "closed", ClosedReason: "wontfix", ClosedAt: &closedAt}
	changed, events, err := store.(db.IssueStatusWriter).ObserveIssueStatus(ctx, p)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, events, 1)
	issue, err := store.IssueByID(ctx, f.Issue.ID)
	require.NoError(t, err)
	require.Equal(t, new("wontfix"), issue.ClosedReason)
	require.Equal(t, &closedAt, issue.ClosedAt)
	// Matching status cannot replace native close metadata with later reads.
	p.ClosedReason = "done"
	p.ClosedAt = &at
	p.Observation.Version = at.Add(time.Minute)
	changed, events, err = store.(db.IssueStatusWriter).ObserveIssueStatus(ctx, p)
	require.NoError(t, err)
	require.False(t, changed)
	require.Empty(t, events)
	issue, err = store.IssueByID(ctx, f.Issue.ID)
	require.NoError(t, err)
	require.Equal(t, new("wontfix"), issue.ClosedReason)
	require.Equal(t, &closedAt, issue.ClosedAt)
	return nil
}
