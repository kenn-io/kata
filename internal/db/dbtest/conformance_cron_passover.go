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

func seedPassover(t *testing.T, s db.Storage, pid int64, values map[string]jsontext.Value) {
	t.Helper()
	raw, e := json.Marshal(values)
	require.NoError(t, e)
	q := s.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, e = q.ExecContext(t.Context(), `UPDATE projects SET metadata=$1 WHERE id=$2`, string(raw), pid)
	require.NoError(t, e)
}
func passoverFixtureValue(t *testing.T, at time.Time) jsontext.Value {
	t.Helper()
	raw, e := json.Marshal(map[string]any{"version": 1, "through": at, "by": "operator"})
	require.NoError(t, e)
	return raw
}
func fixtureProgress(t *testing.T, s db.Storage, parentID int64, jobUID string) cron.RecurringProgress {
	t.Helper()
	parent, e := s.IssueByID(t.Context(), parentID)
	require.NoError(t, e)
	var values map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(parent.Metadata), &values))
	got, e := cron.DecodeRecurringProgress(values["cron.progress."+jobUID])
	require.NoError(t, e)
	return got
}
func checkCronPassover(t *testing.T, _ db.Storage, b Backend) error {
	for _, policy := range []string{"all", "latest", "skip"} {
		t.Run(policy, func(t *testing.T) {
			s := b.Open(t)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			p, parent, job, progress := recurringFixture(t, s, policy)
			cutoff := progress.Anchor.Add(6 * time.Hour)
			seedPassover(t, s, p.ID, map[string]jsontext.Value{"cron.passover": passoverFixtureValue(t, cutoff)})
			in := db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: cutoff}
			out, e := s.DispatchRecurringCron(t.Context(), in)
			require.NoError(t, e)
			require.Len(t, out.Events, 1, "clamp-only sweep persists once")
			got := fixtureProgress(t, s, parent.ID, job.UID)
			require.Equal(t, cutoff, got.Through)
			require.Equal(t, progress.Anchor, got.Anchor)
			out, e = s.DispatchRecurringCron(t.Context(), in)
			require.NoError(t, e)
			require.Empty(t, out.Events, "idle second sweep emits nothing")
			in.Now = cutoff.Add(4 * time.Hour)
			out, e = s.DispatchRecurringCron(t.Context(), in)
			require.NoError(t, e)
			children, e := s.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: p.ID})
			require.NoError(t, e)
			count := 0
			for _, child := range children {
				if child.ID == parent.ID {
					continue
				}
				count++
				var values map[string]jsontext.Value
				require.NoError(t, json.Unmarshal([]byte(child.Metadata), &values))
				var occurrence struct {
					ScheduledAt time.Time `json:"scheduled_at"`
				}
				require.NoError(t, json.Unmarshal(values["cron.occurrence"], &occurrence))
				require.True(t, occurrence.ScheduledAt.After(cutoff))
			}
			expected := 1
			if policy == "all" {
				expected = 2
			}
			require.Equal(t, expected, count)
		})
	}
	t.Run("unfinished latest scan", func(t *testing.T) {
		s := b.Open(t)
		t.Cleanup(func() { require.NoError(t, s.Close()) })
		p, parent, job, progress := recurringFixture(t, s, "latest")
		definition := job.Definition
		definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 * * * *"}
		job, _, err := s.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Name: job.Name, Definition: definition, Actor: "operator"})
		require.NoError(t, err)
		in := db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor.Add(6000 * time.Hour)}
		first, e := s.DispatchRecurringCron(t.Context(), in)
		require.NoError(t, e)
		require.True(t, first.More)
		require.NotNil(t, fixtureProgress(t, s, parent.ID, job.UID).Scan)
		cutoff := progress.Anchor.Add(1000 * time.Hour)
		seedPassover(t, s, p.ID, map[string]jsontext.Value{"cron.passover." + job.UID: passoverFixtureValue(t, cutoff)})
		next, e := s.DispatchRecurringCron(t.Context(), in)
		require.NoError(t, e)
		require.True(t, next.More)
		require.Len(t, next.Events, 1)
		got := fixtureProgress(t, s, parent.ID, job.UID)
		require.Equal(t, cutoff, got.Through)
		require.Equal(t, progress.Anchor, got.Anchor)
		require.NotNil(t, got.Scan)
		for next.More {
			next, e = s.DispatchRecurringCron(t.Context(), in)
			require.NoError(t, e)
		}
		children, e := s.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: p.ID})
		require.NoError(t, e)
		require.Len(t, children, 2)
	})
	t.Run("malformed applicable state", func(t *testing.T) {
		s := b.Open(t)
		t.Cleanup(func() { require.NoError(t, s.Close()) })
		p, parent, job, progress := recurringFixture(t, s, "all")
		in := db.DispatchRecurringCronIn{ProjectID: p.ID, JobUID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Now: progress.Anchor.Add(2 * time.Hour)}
		valid := passoverFixtureValue(t, progress.Anchor)
		for _, values := range []map[string]jsontext.Value{{"cron.passover": jsontext.Value(`null`), "cron.passover." + job.UID: valid}, {"cron.passover": valid, "cron.passover." + job.UID: jsontext.Value(`null`)}} {
			seedPassover(t, s, p.ID, values)
			out, e := s.DispatchRecurringCron(t.Context(), in)
			require.ErrorIs(t, e, cron.ErrInvalid)
			require.Empty(t, out.Events)
			require.Equal(t, progress, fixtureProgress(t, s, parent.ID, job.UID))
		}
		seedPassover(t, s, p.ID, map[string]jsontext.Value{"cron.passover.01ARZ3NDEKTSV4RRFFQ69G5FAW": jsontext.Value(`null`)})
		out, e := s.DispatchRecurringCron(t.Context(), in)
		require.NoError(t, e)
		require.Len(t, out.Events, 2, "unrelated invalid job cannot block this job")
	})
	return nil
}
