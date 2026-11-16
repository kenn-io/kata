package jsonl_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/fileevidence"
	"go.kenn.io/kata/internal/jsonl"
)

func TestFileEvidenceExportImport(t *testing.T) {
	ctx, store, project := newExportEnv(t)
	issue := createTesterIssue(ctx, t, store, project.ID, "Portable file evidence", "")
	size := int64(0)
	_, events, _, err := store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: issue.ID, Actor: "operator", Reason: "done", Evidence: []db.Evidence{{Type: "test", Command: "go test ./example"}, {Type: "file", File: &fileevidence.Reference{Source: "notes", Path: "report.md", Origin: store.InstanceUID(), SHA256: strings.Repeat("0", 64), Size: &size}}}})
	require.NoError(t, err)
	require.NotEmpty(t, events)
	var output bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, store, &output, jsonl.ExportOptions{IncludeDeleted: true}))
	dest, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, dest.Close()) })
	require.NoError(t, jsonl.Import(ctx, &output, dest))
	restored, err := dest.ProjectByUID(ctx, project.UID)
	require.NoError(t, err)
	got, err := dest.EventsByUIDs(ctx, restored.ID, []string{events[0].UID})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, events[0].Payload, got[0].Payload)
	require.Equal(t, events[0].ContentHash, got[0].ContentHash)
}
