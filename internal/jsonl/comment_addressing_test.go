package jsonl_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func TestCommentAddressingSurvivesJSONLAndCutover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	source := openCutoverTargetDB(t, t.Context(), path)
	project, err := source.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := source.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Task", Author: "alice"})
	require.NoError(t, err)
	empty := ""
	comment, event, err := source.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "alice", Teammate: "codex", AddressingTeammate: &empty, Body: "Finding"})
	require.NoError(t, err)
	check := func(target *sqlitestore.Store) {
		t.Helper()
		p, err := target.ProjectByUID(t.Context(), project.UID)
		require.NoError(t, err)
		i, err := target.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		tx, err := target.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		values, err := db.CommentAddressingTeammates(t.Context(), tx, p.ID, i.ID)
		require.NoError(t, err)
		require.NotNil(t, values[comment.UID])
		require.Equal(t, "", *values[comment.UID])
		require.NoError(t, tx.Rollback())
		events, err := target.EventsByUIDs(t.Context(), p.ID, []string{event.UID})
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.Equal(t, event.Payload, events[0].Payload)
		require.Equal(t, event.ContentHash, events[0].ContentHash)
	}
	var wire bytes.Buffer
	require.NoError(t, jsonl.Export(t.Context(), source, &wire, jsonl.ExportOptions{IncludeDeleted: true}))
	restored, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, jsonl.Import(t.Context(), bytes.NewReader(wire.Bytes()), restored))
	check(restored)
	// Only the isolated source fixture is marked old to exercise the real cutover.
	_, err = source.ExecContext(t.Context(), `UPDATE meta SET value=? WHERE key='schema_version'`, db.CurrentSchemaVersion()-1)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	require.NoError(t, jsonl.AutoCutover(t.Context(), path))
	cutover, err := sqlitestore.Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cutover.Close()) })
	check(cutover)
}
