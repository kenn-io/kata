package dbtest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Reason filtering follows RunReason, including structured precedence and exact
// LF-delimited legacy lines, and applies before page limits on both backends.
func checkCronRunReasons(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "reason-project")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, store, project)
	long := strings.Repeat("a", 64)
	for _, summary := range []cron.Summary{
		{Version: 1, Reason: "failed", Message: "reason: timeout"},
		{Version: 1, Message: "reason: failed"},
		{Version: 1, Message: "reason: failed\nDetails"},
		{Version: 1, Reason: "timeout", Message: "reason: failed"},
		{Version: 1, Message: "reason: failed\r\nDetails"},
		{Version: 1, Message: "reason: failed extra"},
		{Version: 1, Message: "Details\nreason: failed"},
		{Version: 1, Message: " reason: failed"},
		{Version: 1, Message: "reason: FAILED"},
		{Version: 1, Message: "reason: " + long + "\nDetails"},
		{Version: 1, Reason: long},
		{Version: 1, Message: "reason: failed\n"},
		{Version: 1, Reason: cron.ReasonOverlapSkipped, Message: "reason: failed"},
		{Version: 1, Message: "reason: " + cron.ReasonOverlapSkipped},
		{Version: 1, Reason: cron.ReasonCatchupMissed},
		{Version: 1, Message: "reason: " + cron.ReasonCatchupMissed},
		{Version: 1, Reason: cron.ReasonPausedPassedOver},
		{Version: 1, Message: "reason: " + cron.ReasonPausedPassedOver},
		{Version: 1, Message: "unrelated newest run"},
	} {
		id, err := uid.New()
		require.NoError(t, err)
		_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: id, Actor: "operator", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "failed", Summary: summary})
		require.NoError(t, err)
	}
	all, err := store.ListCronRuns(ctx, db.CronRunList{ProjectID: project.ID})
	require.NoError(t, err)
	for _, reason := range []string{"failed", "timeout", long, "absent", cron.ReasonOverlapSkipped, cron.ReasonCatchupMissed, cron.ReasonPausedPassedOver} {
		want := []string{}
		for _, run := range all {
			if value, ok := cron.RunReason(run.Summary); ok && value == reason {
				want = append(want, run.UID)
			}
		}
		got := []string{}
		before := ""
		for range len(all) + 1 {
			page, err := store.ListCronRuns(ctx, db.CronRunList{ProjectID: project.ID, JobUID: job.UID, Reason: reason, Limit: 1, BeforeUID: before})
			require.NoError(t, err)
			if len(page) == 0 {
				break
			}
			require.Len(t, page, 1)
			got = append(got, page[0].UID)
			before = page[0].UID
		}
		require.Equal(t, want, got, reason)
	}
	for _, invalid := range []string{"FAILED", "bad code", strings.Repeat("a", 65), "failed\n"} {
		_, err := store.ListCronRuns(ctx, db.CronRunList{ProjectID: project.ID, Reason: invalid})
		require.ErrorIs(t, err, cron.ErrInvalid)
	}
	return nil
}

// Step messages must behave identically across stores: reject NUL before SQL,
// while retaining other free-text controls and reason-filtered readback.
func checkCronSummaryStepControls(t *testing.T, store db.Storage) error {
	for _, message := range []string{"before\x00after", "line\nnext\tcolumn\rreturn\x01control"} {
		t.Run(message, func(t *testing.T) {
			ctx := t.Context()
			id, err := uid.New()
			require.NoError(t, err)
			project, err := store.CreateProject(ctx, "step-controls-"+strings.ToLower(id))
			require.NoError(t, err)
			job, _ := nativeFederationDefinitions(t, store, project)
			summary := cron.Summary{Version: 1, Reason: "failed", Steps: []cron.SummaryStep{{Key: "review", Status: "failed", Message: message}}}
			_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: id, Actor: "operator", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "failed", Summary: summary})
			if strings.ContainsRune(message, 0) {
				require.ErrorIs(t, err, cron.ErrInvalid)
				_, err = store.CronRun(ctx, project.ID, id)
				require.ErrorIs(t, err, db.ErrNotFound)
				return
			}
			require.NoError(t, err)
			run, err := store.CronRun(ctx, project.ID, id)
			require.NoError(t, err)
			require.Equal(t, summary, run.Summary)
			runs, err := store.ListCronRuns(ctx, db.CronRunList{ProjectID: project.ID, Reason: "failed"})
			require.NoError(t, err)
			require.Len(t, runs, 1)
			require.Equal(t, id, runs[0].UID)
		})
	}
	return nil
}
