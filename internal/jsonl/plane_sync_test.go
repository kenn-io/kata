package jsonl_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
)

const planeFixtureSource = "https://api.plane.so/example-workspace/11111111-1111-4111-8111-111111111111"
const planeFixtureConfig = `{"api_origin":"https://api.plane.so","web_origin":"https://app.plane.so","workspace":"example-workspace","project_id":"11111111-1111-4111-8111-111111111111","since":"2026-09-01T00:00:00Z","title_prefix":true}`

type planeExportFixture struct {
	exported []byte
	binding  db.IssueSyncBinding
	claim    time.Time
	issue    db.Issue
}

func exportPlaneFixture(t *testing.T) planeExportFixture {
	t.Helper()
	ctx := context.Background()
	s := openExportTestDB(t)
	p, err := s.CreateProject(ctx, "example-plane-project")
	require.NoError(t, err)
	b, err := s.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: p.ID, Provider: "plane", SourceKey: "plane:" + planeFixtureSource, RemoteID: "example-workspace/11111111-1111-4111-8111-111111111111", DisplayName: "Example tasks", Config: []byte(planeFixtureConfig), IntervalSeconds: 300})
	require.NoError(t, err)
	at := mustParseTime(t, "2026-09-28T01:00:00Z")
	_, ok, err := s.ClaimIssueSyncBinding(ctx, b.ID, "plane", at, at.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	_, _, err = s.ImportBatch(ctx, db.ImportBatchParams{ProjectID: p.ID, Source: b.SourceKey, Actor: "plane-sync", IssueSyncGuard: &db.IssueSyncImportGuard{BindingID: b.ID, Provider: "plane", StartedAt: at}, Items: []db.ImportItem{{ExternalID: "work-item:22222222-2222-4222-8222-222222222222", Title: "[Plane] Example task", Body: "Example body", Author: "plane-unknown", Status: "open", CreatedAt: at.Add(-time.Hour), UpdatedAt: at}}})
	require.NoError(t, err)
	_, err = s.RecordIssueSyncSuccess(ctx, db.IssueSyncSuccessParams{BindingID: b.ID, StartedAt: at, At: at, CursorAt: at, LastCreated: 1})
	require.NoError(t, err)
	claim := at.Add(time.Minute)
	_, ok, err = s.ClaimIssueSyncBinding(ctx, b.ID, "plane", claim, at.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	b, err = s.IssueSyncBindingByID(ctx, b.ID)
	require.NoError(t, err)
	m, err := s.ImportMappingBySource(ctx, p.ID, b.SourceKey, "issue", "work-item:22222222-2222-4222-8222-222222222222")
	require.NoError(t, err)
	require.NotNil(t, m.IssueID)
	issue, err := s.IssueByID(ctx, *m.IssueID)
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, s, &out, jsonl.ExportOptions{IncludeDeleted: true}))
	return planeExportFixture{exported: out.Bytes(), binding: b, claim: claim, issue: issue}
}

func TestPlaneSyncExportPreservesConfigSourceAndMappings(t *testing.T) {
	fixture := exportPlaneFixture(t)
	records := decodeJSONLLines(t, fixture.exported)
	assertKindOrder(t, records)
	var binding, status, mapping map[string]any
	for _, record := range records {
		data := record["data"].(map[string]any)
		switch record["kind"] {
		case "issue_sync_binding":
			binding = data
		case "issue_sync_status":
			status = data
		case "import_mapping":
			mapping = data
		}
	}
	require.NotNil(t, binding)
	require.Equal(t, "plane", binding["provider"])
	require.Equal(t, "plane:"+planeFixtureSource, binding["source_key"])
	raw, err := json.Marshal(binding["config"])
	require.NoError(t, err)
	require.JSONEq(t, planeFixtureConfig, string(raw))
	require.Equal(t, true, binding["enabled"])
	require.NotNil(t, status)
	require.Equal(t, "2026-09-28T01:01:00.000Z", status["sync_started_at"])
	require.NotNil(t, mapping)
	require.Equal(t, "plane:"+planeFixtureSource, mapping["source"])
	require.Equal(t, "work-item:22222222-2222-4222-8222-222222222222", mapping["external_id"])
}
func TestPlaneSyncRestoreRequiresLocalReenable(t *testing.T) {
	ctx := context.Background()
	fixture := exportPlaneFixture(t)
	target := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(fixture.exported), target))
	b, err := target.IssueSyncBindingByProject(ctx, fixture.binding.ProjectID)
	require.NoError(t, err)
	require.False(t, b.Enabled)
	require.Equal(t, fixture.binding.SourceKey, b.SourceKey)
	require.JSONEq(t, planeFixtureConfig, string(b.Config))
	assertTimePtrEqual(t, *fixture.binding.LastCursorAt, b.LastCursorAt)
	status, err := target.IssueSyncStatusByProject(ctx, b.ProjectID)
	require.NoError(t, err)
	assertTimePtrEqual(t, fixture.claim, status.SyncStartedAt)
	mapping, err := target.ImportMappingBySource(ctx, b.ProjectID, b.SourceKey, "issue", "work-item:22222222-2222-4222-8222-222222222222")
	require.NoError(t, err)
	require.Equal(t, &fixture.issue.ID, mapping.IssueID)
	batch := db.ImportBatchParams{ProjectID: b.ProjectID, Source: b.SourceKey, Actor: "plane-sync", IssueSyncGuard: &db.IssueSyncImportGuard{BindingID: b.ID, Provider: "plane", StartedAt: fixture.claim}, Items: []db.ImportItem{{ExternalID: "work-item:22222222-2222-4222-8222-222222222222", Title: "Changed task", Author: "plane-unknown", Status: "open", CreatedAt: fixture.issue.CreatedAt, UpdatedAt: fixture.issue.UpdatedAt.Add(time.Hour)}}}
	_, _, err = target.ImportBatch(ctx, batch)
	require.ErrorIs(t, err, db.ErrIssueSyncNotEnabled)
	b, err = target.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: b.ProjectID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: b.Config, IntervalSeconds: b.IntervalSeconds})
	require.NoError(t, err)
	require.True(t, b.Enabled)
	status, err = target.IssueSyncStatusByProject(ctx, b.ProjectID)
	require.NoError(t, err)
	require.Nil(t, status.SyncStartedAt)
	_, _, err = target.ImportBatch(ctx, batch)
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)
	claim := fixture.claim.Add(time.Hour)
	_, ok, err := target.ClaimIssueSyncBinding(ctx, b.ID, "plane", claim, fixture.claim)
	require.NoError(t, err)
	require.True(t, ok)
	batch.IssueSyncGuard.StartedAt = claim
	result, _, err := target.ImportBatch(ctx, batch)
	require.NoError(t, err)
	require.Equal(t, 1, result.Updated)
}

func TestPlaneSyncTrustedCutoverPreservesEnabled(t *testing.T) {
	ctx := context.Background()
	fixture := exportPlaneFixture(t)
	target := openImportTargetDB(t)
	require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(fixture.exported), target, jsonl.ImportOptions{PreserveIssueSyncBindingEnabled: true}))
	b, err := target.IssueSyncBindingByProject(ctx, fixture.binding.ProjectID)
	require.NoError(t, err)
	require.True(t, b.Enabled)
	require.JSONEq(t, planeFixtureConfig, string(b.Config))
	status, err := target.IssueSyncStatusByProject(ctx, b.ProjectID)
	require.NoError(t, err)
	assertTimePtrEqual(t, fixture.claim, status.SyncStartedAt)
}
