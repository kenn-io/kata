package main

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronRunCountsCLI(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	id := cronCLIUID(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	entry := db.CronTallyEntry{Kind: "job", UID: id, CoverageStartedAt: at, LastPrunedAt: at, OldestCreatedAt: at, NewestCreatedAt: at, Counts: db.CronTallyCounts{Succeeded: 9007199254740993}}
	raw, err := json.Marshal(map[string]any{db.CronTallyPrefix + id: db.CronTallyValue{Version: 1, Groups: map[string]db.CronTallyEntry{"job": entry}}})
	require.NoError(t, err)
	_, err = env.DB.ExecContext(t.Context(), `UPDATE projects SET metadata=$1 WHERE id=$2`, string(raw), project.ID)
	require.NoError(t, err)
	for _, args := range [][]string{{"run", "counts"}, {"run", "counts", "--job", strings.ToLower(id)}, {"run", "counts", "--json=false", "--agent"}} {
		out, _, err := executeCronCLI(t, env.URL, nil, args...)
		require.NoError(t, err)
		require.Contains(t, out, "9007199254740993")
		var result db.CronRunCountsResult
		require.NoError(t, json.Unmarshal([]byte(out), &result))
		require.Len(t, result.Groups, 1)
		require.Equal(t, id, result.Groups[0].UID)
		require.Equal(t, entry.Counts, result.Groups[0].PrunedSinceTracking)
	}
	out, _, err := executeCronCLI(t, env.URL, nil, "run", "counts", "--workflow", id)
	require.NoError(t, err)
	var empty db.CronRunCountsResult
	require.NoError(t, json.Unmarshal([]byte(out), &empty))
	require.NotNil(t, empty.Groups)
	require.Empty(t, empty.Groups)
	for _, args := range [][]string{{"run", "counts", "--job", "bad"}, {"run", "counts", "--job", id, "--workflow", id}} {
		_, _, err = executeCronCLI(t, env.URL, nil, args...)
		require.Error(t, err)
	}
}

func TestCronRunCountsHuman(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	err := renderCronRunCounts(&out, db.CronRunCountsResult{Groups: []db.CronRunCountGroup{{Kind: "job", UID: "example", Name: "Review\nspoof", Retained: db.CronRunRetainedCounts{Running: 2}, PrunedSinceTracking: db.CronTallyCounts{Succeeded: 9007199254740993}, CoverageStartedAt: &at, LastPrunedAt: &at, OldestCreatedAt: &at, NewestCreatedAt: &at}, {Kind: "workflow", UID: "steps"}}})
	require.NoError(t, err)
	text := out.String()
	require.Contains(t, text, "Retained")
	require.Contains(t, text, "Pruned since tracking")
	require.Contains(t, text, "9007199254740993")
	require.Contains(t, text, "2026-01-01T00:00:00Z")
	require.NotContains(t, text, "lifetime")
	require.NotContains(t, text, "Review\nspoof")
	require.NotContains(t, text, "0001-")
}
