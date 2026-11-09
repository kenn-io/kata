package jsonl_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
)

func TestRoundtripPreservesPriority(t *testing.T) {
	ctx := t.Context()
	src := openExportTestDB(t)
	p, err := src.CreateProject(ctx, "kata")
	require.NoError(t, err)

	prio := int64(0)
	_, _, err = src.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "highest priority", Author: "tester", Priority: &prio,
	})
	require.NoError(t, err)
	_, _, err = src.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "no priority", Author: "tester",
	})
	require.NoError(t, err)
	mid := int64(2)
	_, _, err = src.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "middle priority", Author: "tester", Priority: &mid,
	})
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, src, &buf, jsonl.ExportOptions{}))

	dst := openExportTestDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(buf.Bytes()), dst))

	rows, err := dst.QueryContext(ctx, `SELECT title, priority FROM issues ORDER BY id ASC`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var got []struct {
		Title    string
		Priority *int64
	}
	for rows.Next() {
		var r struct {
			Title    string
			Priority *int64
		}
		require.NoError(t, rows.Scan(&r.Title, &r.Priority))
		got = append(got, r)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 3)
	require.NotNil(t, got[0].Priority)
	assert.Equal(t, int64(0), *got[0].Priority)
	assert.Nil(t, got[1].Priority)
	require.NotNil(t, got[2].Priority)
	assert.Equal(t, int64(2), *got[2].Priority)
}

// TestRoundtripOmitsPriorityKeyWhenUnset verifies the export wire shape: a
// nil priority drops the "priority" key entirely rather than emitting
// "priority":null. Stable wire shape matters for diffing JSONL backups.
func TestRoundtripOmitsPriorityKeyWhenUnset(t *testing.T) {
	ctx := t.Context()
	src := openExportTestDB(t)
	p, err := src.CreateProject(ctx, "kata")
	require.NoError(t, err)
	_, _, err = src.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "no priority", Author: "tester",
	})
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, src, &buf, jsonl.ExportOptions{}))

	out := buf.String()
	// Guard against either "priority":null OR a present priority key. The
	// issue line is the only one that would carry priority; check both forms.
	assert.False(t, strings.Contains(out, `"priority":null`),
		"export should omit priority key entirely, not emit null")
	assert.False(t, strings.Contains(out, `"priority":`),
		"export should omit priority key when unset; got %q", out)
}
