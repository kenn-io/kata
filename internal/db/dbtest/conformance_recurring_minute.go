package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// Model a definition written before the minute guard without changing its revision.
func storeLegacyWildcardMinute(t *testing.T, s db.Storage, job db.CronJob, enabled bool) db.CronJob {
	t.Helper()
	job.Definition.Trigger = cron.Trigger{Kind: "cron", Cron: "* 9 * * *", Timezone: "UTC"}
	job.Definition.Enabled = enabled
	raw, err := json.Marshal(job.Definition)
	require.NoError(t, err)
	q := s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = q.ExecContext(t.Context(), `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, string(raw), job.UID)
	require.NoError(t, err)
	return job
}

// Passes on the base: a stored definition keeps its old meaning, including
// all sixty children in the 09:00 hour, across bounded delivery batches.
func checkRecurringStoredWildcardMinuteKeepsDelivering(t *testing.T, s db.Storage) error {
	p, parent, job, _ := recurringFixture(t, s, "all")
	job = storeLegacyWildcardMinute(t, s, job, true)
	// A fixed old checkpoint isolates the 09:00 hour regardless of the wall clock.
	anchor := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	progress := cron.RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}
	var metadata map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(parent.Metadata), &metadata))
	if metadata == nil {
		metadata = make(map[string]jsontext.Value)
	}
	raw, err := json.Marshal(progress)
	require.NoError(t, err)
	metadata["cron.progress."+job.UID] = raw
	raw, err = json.Marshal(metadata)
	require.NoError(t, err)
	q := s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = q.ExecContext(t.Context(), `UPDATE issues SET metadata=$1 WHERE id=$2`, string(raw), parent.ID)
	require.NoError(t, err)
	start := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	in := db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: start.Add(time.Hour)}
	for batch := 0; ; batch++ {
		require.Less(t, batch, 3, "delivery must finish in two bounded batches")
		out, err := s.DispatchRecurringCron(t.Context(), in)
		require.NoError(t, err)
		if !out.More {
			break
		}
	}
	children, err := s.ChildrenOfIssue(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Len(t, children, 60)
	seen := map[time.Time]bool{}
	for _, child := range children {
		var metadata map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(child.Metadata), &metadata))
		var occurrence struct {
			ScheduledAt time.Time `json:"scheduled_at"`
		}
		require.NoError(t, json.Unmarshal(metadata["cron.occurrence"], &occurrence))
		require.False(t, seen[occurrence.ScheduledAt], "duplicate occurrence")
		seen[occurrence.ScheduledAt] = true
	}
	for minute := range 60 {
		require.True(t, seen[start.Add(time.Duration(minute)*time.Minute)])
	}
	return nil
}

// Each minute is a child issue: editing a legacy definition must fix the minute,
// even when disabling or re-enabling. Rejected writes leave its revision intact.
func checkRecurringUpdateKeepingWildcardMinuteRejected(t *testing.T, s db.Storage) error {
	p, _, initial, _ := recurringFixture(t, s, "all")
	for _, change := range []string{"title", "disable", "enable"} {
		t.Run(change, func(t *testing.T) {
			current, err := s.CronJob(t.Context(), p.ID, initial.UID)
			require.NoError(t, err)
			stored := storeLegacyWildcardMinute(t, s, current, change != "enable")
			definition := stored.Definition
			issue := *definition.Issue
			definition.Issue = &issue
			switch change {
			case "title":
				definition.Issue.Title = "Changed review"
			case "disable":
				definition.Enabled = false
			case "enable":
				definition.Enabled = true
			}
			_, _, err = s.PutCronJob(t.Context(), db.PutCronJob{UID: stored.UID, ProjectID: p.ID, Name: stored.Name, Definition: definition, ExpectedEventUID: stored.DefinitionEventUID, Actor: "operator"})
			require.ErrorIs(t, err, cron.ErrInvalid)
			require.ErrorContains(t, err, "minute is a wildcard")
			unchanged, err := s.CronJob(t.Context(), p.ID, stored.UID)
			require.NoError(t, err)
			require.Equal(t, stored.DefinitionEventUID, unchanged.DefinitionEventUID)
			require.Equal(t, stored.Revision, unchanged.Revision)
			require.Equal(t, stored.Definition, unchanged.Definition)
		})
	}
	current, err := s.CronJob(t.Context(), p.ID, initial.UID)
	require.NoError(t, err)
	fixed := current.Definition
	fixed.Trigger = cron.Trigger{Kind: "cron", Cron: "0 9 * * *", Timezone: "UTC"}
	_, _, err = s.PutCronJob(t.Context(), db.PutCronJob{UID: initial.UID, ProjectID: p.ID, Name: initial.Name, Definition: fixed, ExpectedEventUID: current.DefinitionEventUID, Actor: "operator"})
	require.NoError(t, err)
	return nil
}
