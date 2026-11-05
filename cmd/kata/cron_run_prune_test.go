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

func TestCronRunPruneHumanReportsBatchWindowAndReplicaReset(t *testing.T) {
	id := "01K00000000000000000000001"
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	err := renderCronRunPrune(&out, db.CronRunPruneResult{Pruned: 1, EventsDeleted: 2, More: true, ResetAfterEventID: 42,
		Groups: []db.CronRunPruneGroup{{JobUID: &id, Name: "Inspect", Counts: map[string]int{"failed": 1}, OldestCreatedAt: at, NewestCreatedAt: at.Add(time.Hour)}}})
	require.NoError(t, err)
	require.Contains(t, out.String(), "2026-08-01T00:00:00Z")
	require.Contains(t, out.String(), "2026-08-01T01:00:00Z")
	require.Contains(t, out.String(), "Replicas will rebuild.")
	require.Contains(t, out.String(), "more: true")
}

func TestCronPruneOlderThan(t *testing.T) {
	// Check prune's accepted duration range and whole-day input rules.
	// These overflow inputs are rejected, but do not isolate the day guard.
	for value, expected := range map[string]time.Duration{"30d": 720 * time.Hour, "24h": 24 * time.Hour, "25h30m": 25*time.Hour + 30*time.Minute, "1d": 24 * time.Hour, "106751d": 106751 * 24 * time.Hour} {
		d, err := parseCronPruneAge(value)
		require.NoError(t, err)
		require.Equal(t, expected, d)
	}
	for _, value := range []string{"", "0d", "-1d", "1.5d", "1h2d", "2026-05-01T00:00:00Z", "23h", "abc", "106752d", "9223372036854775807d"} {
		_, err := parseCronPruneAge(value)
		require.Error(t, err, value)
	}
}

func TestCronRunPruneCLIPreviewAndConfirmation(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "operator")
	env := testenv.New(t)
	_, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	out, _, err := executeCronCLI(t, env.URL, strings.NewReader(""), "run", "prune", "--older-than", "30d")
	require.NoError(t, err)
	var preview db.CronRunPruneResult
	require.NoError(t, json.Unmarshal([]byte(out), &preview))
	require.True(t, preview.DryRun)
	require.Equal(t, 10, preview.Keep)
	require.Zero(t, preview.ResetAfterEventID)
	_, _, err = executeCronCLI(t, env.URL, strings.NewReader(""), "run", "prune", "--older-than", "30d", "--force")
	require.ErrorContains(t, err, "--confirm")
	out, _, err = executeCronCLI(t, env.URL, strings.NewReader(""), "run", "prune", "--older-than", "30d", "--force", "--confirm", "PRUNE spoke-project cron runs")
	require.NoError(t, err)
	var confirmed db.CronRunPruneResult
	require.NoError(t, json.Unmarshal([]byte(out), &confirmed))
	require.False(t, confirmed.DryRun)
}

func TestCronRunExplicitEmptyFilters(t *testing.T) {
	for _, command := range []string{"prune", "counts"} {
		for _, flag := range []string{"--job", "--workflow"} {
			t.Run(command+flag, func(t *testing.T) {
				args := []string{"run", command, flag, ""}
				if command == "prune" {
					args = append(args, "--older-than", "90d", "--force", "--confirm", "PRUNE spoke-project cron runs")
				}
				_, _, err := executeCronCLI(t, "http://127.0.0.1:0", nil, args...)
				require.ErrorContains(t, err, "cron identity must be a 26-character ULID")
			})
		}
	}
}
