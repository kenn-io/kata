package jsonl_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
	_ "modernc.org/sqlite"
)

// Released SQLite schema 34: main before MCP Events subscriptions.
//
//go:embed testdata/schema_v34.sql
var mcpEventsSQLiteSchema34 string

const (
	mcpEventsSourceCheck = "source-key-check"
	mcpEventsDestCheck   = "destination-key-check"
)

var mcpEventsAt = time.Date(2026, 10, 20, 12, 0, 0, 123456789, time.UTC)

// mcpEventsRows returns one ended row with a pending terminated notice and one
// active row in a gap state. Every nullable column is set on at least one.
func mcpEventsRows() []db.MCPEventSubscription {
	ended := mcpEventsAt.Add(-time.Hour)
	until := mcpEventsAt.Add(time.Minute)
	head, cursor := int64(9), int64(12)
	terminated := db.MCPEventSubscription{
		ID: "sub_ended", PrincipalAuthority: "db_token", PrincipalSubject: "7", PrincipalKey: "db_token:7",
		Actor: "Example Actor", EventName: "kata.inbox.changed", Arguments: []byte(`{"project_uids":["p1"]}`),
		CallbackURL: "https://receiver.example/hook/ended", SecretEnc: []byte{0, 1, 2, 250},
		PreviousSecretEnc: []byte("older-sealed"), PreviousSecretUntil: &until, VerifiedAt: &ended,
		Incarnation: "inc-ended", Generation: 4, ContinuityGeneration: 2,
		State: db.MCPEventStateEnded, EndReason: "principal_revoked", EndedAt: &ended,
		CursorEventID: 41, PendingKind: db.MCPEventPendingTerminated, PendingMessageID: "msg_terminated",
		PendingBody: []byte(`{"type":"terminated"}`), AttemptCount: 2, NextAttemptAt: &until,
		FailedSince: &ended, FailedAttempts: 3, LastDeliveryAt: &ended, LastError: "http_500", DeadLetterCount: 1,
		CreatedAt: ended.Add(-time.Hour), UpdatedAt: ended,
	}
	gap := db.MCPEventSubscription{
		ID: "sub_gap", PrincipalAuthority: "static_token", PrincipalSubject: "static", PrincipalKey: "static_token:static",
		EventName: "kata.inbox.changed", Arguments: []byte(`{}`),
		CallbackURL: "https://receiver.example/hook/gap", SecretEnc: []byte("sealed"),
		Incarnation: "inc-gap", Generation: 1, ContinuityGeneration: 3, CatalogVersion: "catalog-v1", ReplayCutoff: &until, ReplayHead: &head,
		State: db.MCPEventStateActive, ExpiresAt: &until, CursorEventID: 50, GapUnacked: true,
		PendingKind: db.MCPEventPendingGap, PendingMessageID: "msg_gap", PendingBody: []byte(`{"type":"gap"}`),
		PendingNextCursor: &cursor, ClaimToken: "claim-token", ClaimUntil: &until,
		CreatedAt: ended, UpdatedAt: mcpEventsAt,
	}
	return []db.MCPEventSubscription{terminated, gap}
}

func seedMCPEvents(t *testing.T, store db.Storage, check string) {
	t.Helper()
	for _, row := range mcpEventsRows() {
		require.NoError(t, store.ImportMCPEventSubscription(t.Context(), row))
	}
	if check != "" {
		stored, err := store.PutMCPEventsKeyCheckIfAbsent(t.Context(), check)
		require.NoError(t, err)
		require.Equal(t, check, stored)
	}
}

func mcpEventsSourceDB(t *testing.T) *sqlitestore.Store {
	t.Helper()
	source := openExportTestDB(t)
	project, err := source.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, _, err = source.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Example", Author: "tester"})
	require.NoError(t, err)
	seedMCPEvents(t, source, mcpEventsSourceCheck)
	return source
}

func assertMCPEvents(t *testing.T, want []db.MCPEventSubscription, store db.Storage, check string) {
	t.Helper()
	for _, row := range want {
		got, err := store.GetMCPEventSubscription(t.Context(), row.ID)
		require.NoError(t, err, row.ID)
		assert.Equal(t, row, got, row.ID)
	}
	var count int
	for _, err := range store.ExportMCPEventSubscriptions(t.Context(), db.ExportFilter{}) {
		require.NoError(t, err)
		count++
	}
	assert.Equal(t, len(want), count, "subscription count")
	assertMCPEventsCheck(t, store, check)
}

func assertMCPEventsCheck(t *testing.T, store db.Storage, want string) {
	t.Helper()
	got, ok, err := store.MCPEventsKeyCheck(t.Context())
	require.NoError(t, err)
	assert.Equal(t, want != "", ok, "key check presence")
	assert.Equal(t, want, got, "key check")
}

type mcpEventsWire struct {
	kinds      map[string]int
	checkLines []string
	lines      []string
}

func readMCPEventsWire(t *testing.T, data []byte) mcpEventsWire {
	t.Helper()
	wire := mcpEventsWire{kinds: map[string]int{}}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		wire.lines = append(wire.lines, line)
		var env struct {
			Kind string `json:"kind"`
			Data struct {
				Key string `json:"key"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &env))
		wire.kinds[env.Kind]++
		if env.Kind == "meta" && env.Data.Key == db.MetaKeyMCPEventsKeyCheck {
			wire.checkLines = append(wire.checkLines, line)
		}
	}
	require.NoError(t, scanner.Err())
	return wire
}

func exportMCPEvents(t *testing.T, store db.Storage, projectID int64) []byte {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, jsonl.Export(t.Context(), store, &out, jsonl.ExportOptions{IncludeDeleted: true, ProjectID: projectID}))
	return out.Bytes()
}

func TestMCPEventsFullRoundTripSQLite(t *testing.T) {
	source := mcpEventsSourceDB(t)
	backup := exportMCPEvents(t, source, 0)

	wire := readMCPEventsWire(t, backup)
	assert.Equal(t, 2, wire.kinds["mcp_event_subscription"])
	assert.Len(t, wire.checkLines, 1)

	restored := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(t.Context(), bytes.NewReader(backup), restored))
	assertMCPEvents(t, mcpEventsRows(), restored, mcpEventsSourceCheck)
	assert.Equal(t, string(backup), string(exportMCPEvents(t, restored, 0)), "a second export is identical")
}

func TestMCPEventsRestoreWithoutKeyFile(t *testing.T) {
	source := mcpEventsSourceDB(t)
	backup := exportMCPEvents(t, source, 0)

	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	target, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(backup), target, jsonl.ImportOptions{RequireFreshTarget: true, NewInstance: true}))
	assertMCPEvents(t, mcpEventsRows(), target, mcpEventsSourceCheck)
	require.NoError(t, filepath.WalkDir(home, func(_ string, entry fs.DirEntry, err error) error {
		require.NoError(t, err)
		assert.NotEqual(t, "mcp-events.key", entry.Name(), "restore must not create a key file")
		return nil
	}))
}

func TestMCPEventsProjectExportOmitsKindAndKeyCheck(t *testing.T) {
	source := mcpEventsSourceDB(t)
	project, err := source.ProjectByName(t.Context(), "example-project")
	require.NoError(t, err)

	wire := readMCPEventsWire(t, exportMCPEvents(t, source, project.ID))
	assert.Zero(t, wire.kinds["mcp_event_subscription"])
	assert.Empty(t, wire.checkLines)
	assert.Positive(t, wire.kinds["issue"], "the project export still has its issue")
}

func TestMCPEventsStandaloneImportDropsKindAndKeyCheck(t *testing.T) {
	source := mcpEventsSourceDB(t)
	backup := exportMCPEvents(t, source, 0)

	copied := openImportTargetDB(t)
	require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(backup), copied, jsonl.ImportOptions{AsStandalone: true}))
	assertMCPEvents(t, nil, copied, "")
	for record, err := range copied.ExportMeta(t.Context()) {
		require.NoError(t, err)
		assert.NotEqual(t, db.MetaKeyMCPEventsKeyCheck, record.Key)
	}
}

// withInjectedMCPEvents adds a key check and a subscription to a project
// export, the way a hand-edited or mis-selected file could.
func withInjectedMCPEvents(t *testing.T, projectExport []byte) []byte {
	t.Helper()
	lines := strings.Split(strings.TrimRight(string(projectExport), "\n"), "\n")
	require.Contains(t, lines[0], "export_version")
	check, err := json.Marshal(map[string]any{"kind": "meta", "data": map[string]string{"key": db.MetaKeyMCPEventsKeyCheck, "value": "injected-check"}})
	require.NoError(t, err)
	injected := db.MCPEventSubscriptionExportOf(mcpEventsRows()[1])
	injected.ID = "sub_injected"
	data, err := json.Marshal(injected)
	require.NoError(t, err)
	row, err := json.Marshal(map[string]any{"kind": "mcp_event_subscription", "data": json.RawMessage(data)})
	require.NoError(t, err)
	out := append([]string{lines[0], string(check)}, lines[1:]...)
	out = append(out, string(row))
	return []byte(strings.Join(out, "\n") + "\n")
}

func mergeSource(t *testing.T) (project string, export []byte) {
	t.Helper()
	source := openExportTestDB(t)
	created, err := source.CreateProject(t.Context(), "merged-project")
	require.NoError(t, err)
	_, _, err = source.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: created.ID, Title: "Merged", Author: "tester"})
	require.NoError(t, err)
	return created.Name, withInjectedMCPEvents(t, exportMCPEvents(t, source, created.ID))
}

func assertPartialImportLeavesDestination(t *testing.T, destination db.Storage) {
	t.Helper()
	assertMCPEvents(t, mcpEventsRows(), destination, mcpEventsDestCheck)
	_, err := destination.GetMCPEventSubscription(t.Context(), "sub_injected")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestMCPEventsPartialMergeNeverWritesKeyCheckSQLite(t *testing.T) {
	_, input := mergeSource(t)
	destination := openImportTargetDB(t)
	seedMCPEvents(t, destination, mcpEventsDestCheck)

	require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(input), destination, jsonl.ImportOptions{MergeProject: true}))
	assertPartialImportLeavesDestination(t, destination)
	_, err := destination.ProjectByName(t.Context(), "merged-project")
	require.NoError(t, err, "the project itself merged")
}

func TestMCPEventsPartialMergeAbsentCheckStaysAbsent(t *testing.T) {
	_, input := mergeSource(t)
	destination := openImportTargetDB(t)

	require.NoError(t, jsonl.ImportWithOptions(t.Context(), bytes.NewReader(input), destination, jsonl.ImportOptions{MergeProject: true}))
	assertMCPEvents(t, nil, destination, "")
}

func TestMCPEventsFullRestoreReplacesDestinationState(t *testing.T) {
	// A backup taken before Events were used has neither rows nor a check. A
	// full restore replaces the destination, so neither may survive it.
	plain := openExportTestDB(t)
	_, err := plain.CreateProject(t.Context(), "plain-project")
	require.NoError(t, err)
	backup := exportMCPEvents(t, plain, 0)

	destination := openImportTargetDB(t)
	seedMCPEvents(t, destination, mcpEventsDestCheck)
	require.NoError(t, jsonl.Import(t.Context(), bytes.NewReader(backup), destination))
	assertMCPEvents(t, nil, destination, "")

	withEvents := exportMCPEvents(t, mcpEventsSourceDB(t), 0)
	require.NoError(t, jsonl.Import(t.Context(), bytes.NewReader(withEvents), destination))
	assertMCPEvents(t, mcpEventsRows(), destination, mcpEventsSourceCheck)
}

func TestMCPEventsCutoverFromSchema34HasNoRows(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, mcpEventsSQLiteSchema34+`
 INSERT INTO meta(key,value) VALUES('schema_version','34'),('instance_uid','00000000000000000000000009');
 INSERT INTO projects(uid,name) VALUES('00000000000000000000000001','existing-project');`)
	require.NoError(t, err)
	var tables int
	require.NoError(t, raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name='mcp_event_subscriptions'`).Scan(&tables))
	require.Zero(t, tables, "the schema 34 fixture predates the table")
	require.NoError(t, raw.Close())

	require.NoError(t, jsonl.AutoCutover(ctx, path))
	upgraded, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	assertMCPEvents(t, nil, upgraded, "")
	_, err = upgraded.ProjectByName(ctx, "existing-project")
	require.NoError(t, err)
	// The upgraded database accepts subscriptions.
	seedMCPEvents(t, upgraded, mcpEventsSourceCheck)
	assertMCPEvents(t, mcpEventsRows(), upgraded, mcpEventsSourceCheck)
}

func TestMCPEventsRoundTripAcrossPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	ctx := context.Background()
	source := mcpEventsSourceDB(t)
	backup := exportMCPEvents(t, source, 0)

	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	postgres, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, postgres.Close()) })
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(backup), postgres))
	assertMCPEvents(t, mcpEventsRows(), postgres, mcpEventsSourceCheck)

	back := exportMCPEvents(t, postgres, 0)
	assert.Equal(t, readMCPEventsWire(t, backup).kinds["mcp_event_subscription"], readMCPEventsWire(t, back).kinds["mcp_event_subscription"])
	restored := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(back), restored))
	assertMCPEvents(t, mcpEventsRows(), restored, mcpEventsSourceCheck)
}

func TestMCPEventsPostgresPartialPathsLeaveDestination(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	ctx := context.Background()
	_, input := mergeSource(t)

	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	postgres, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, postgres.Close()) })
	seedMCPEvents(t, postgres, mcpEventsDestCheck)

	require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(input), postgres, jsonl.ImportOptions{MergeProject: true}))
	assertPartialImportLeavesDestination(t, postgres)

	// A standalone copy into PostgreSQL drops both as well.
	dsn2, cleanup2 := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup2)
	copied, err := pgstore.Open(ctx, dsn2)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, copied.Close()) })
	backup := exportMCPEvents(t, mcpEventsSourceDB(t), 0)
	require.NoError(t, jsonl.ImportWithOptions(ctx, bytes.NewReader(backup), copied, jsonl.ImportOptions{AsStandalone: true}))
	assertMCPEvents(t, nil, copied, "")
}

func TestMCPEventsPostgresFullRestoreReplacesDestinationState(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	ctx := context.Background()
	plain := openExportTestDB(t)
	_, err := plain.CreateProject(ctx, "plain-project")
	require.NoError(t, err)
	backup := exportMCPEvents(t, plain, 0)

	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	postgres, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, postgres.Close()) })
	seedMCPEvents(t, postgres, mcpEventsDestCheck)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(backup), postgres))
	assertMCPEvents(t, nil, postgres, "")
}
