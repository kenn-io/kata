package jsonl

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func mcpEventsCutoverSource(t *testing.T) (*sqlitestore.Store, db.Project, db.MCPEventSubscription) {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := t.Context()
	source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	project, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	ended := time.Date(2026, 10, 20, 11, 0, 0, 0, time.UTC)
	row := db.MCPEventSubscription{
		ID: "sub_cutover", PrincipalAuthority: "local", PrincipalSubject: "local", PrincipalKey: "local:local",
		EventName: "kata.inbox.changed", Arguments: []byte(`{}`), CallbackURL: "https://receiver.example/hook",
		SecretEnc: []byte("sealed"), Incarnation: "inc", State: db.MCPEventStateEnded, EndReason: "unsubscribed",
		EndedAt: &ended, PendingKind: db.MCPEventPendingTerminated, PendingMessageID: "msg", PendingBody: []byte(`{"type":"terminated"}`),
		CreatedAt: ended, UpdatedAt: ended,
	}
	require.NoError(t, source.ImportMCPEventSubscription(ctx, row))
	_, err = source.PutMCPEventsKeyCheckIfAbsent(ctx, "cutover-check")
	require.NoError(t, err)
	return source, project, row
}

func TestExportForCutoverKeepsMCPEventsFromSchema35(t *testing.T) {
	source, _, row := mcpEventsCutoverSource(t)
	var out bytes.Buffer
	require.NoError(t, exportForCutover(t.Context(), source, &out, ExportOptions{IncludeDeleted: true}))

	target, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	require.NoError(t, ImportWithOptions(t.Context(), &out, target, ImportOptions{PreserveIssueSyncBindingEnabled: true, PreserveExternalRootBindingsEnabled: true}))
	got, err := target.GetMCPEventSubscription(t.Context(), row.ID)
	require.NoError(t, err)
	assert.Equal(t, row, got)
	check, ok, err := target.MCPEventsKeyCheck(t.Context())
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "cutover-check", check)
}

func TestExportForCutoverProjectScopeOmitsMCPEvents(t *testing.T) {
	source, project, _ := mcpEventsCutoverSource(t)
	var out bytes.Buffer
	require.NoError(t, exportForCutover(t.Context(), source, &out, ExportOptions{IncludeDeleted: true, ProjectID: project.ID}))
	assert.NotContains(t, out.String(), `"kind":"mcp_event_subscription"`)
	assert.False(t, strings.Contains(out.String(), db.MetaKeyMCPEventsKeyCheck), "project scope must not carry the key check")
}
