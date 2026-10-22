package daemon_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// An undecodable document must not prevent delivery or starve later pages.
func TestSweepLifecycle(t *testing.T) {
	ctx := t.Context()
	env := testenv.New(t)
	p, e := env.DB.CreateProject(ctx, "recurring-project")
	require.NoError(t, e)
	parent, _, e := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Continuing commitment", Author: "operator"})
	require.NoError(t, e)
	var jobs []db.CronJob
	for i := range 102 {
		j := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200}, Action: cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Review"}, Overlap: "allow", Catchup: "latest"}
		job, _, e := env.DB.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: fmt.Sprintf("Review %03d", i), Actor: "operator", Definition: j})
		require.NoError(t, e)
		jobs = append(jobs, job)
	}
	// The JSON document remains syntactically valid but strict stored decoding fails.
	bad := jobs[0]
	raw, e := json.Marshal(bad.Definition)
	require.NoError(t, e)
	var body map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(raw, &body))
	body["unknown_future_field"] = []byte(`true`)
	raw, e = json.Marshal(body)
	require.NoError(t, e)
	q := env.DB
	_, e = q.ExecContext(ctx, `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, string(raw), bad.UID)
	require.NoError(t, e)
	publisher := daemon.NewEventBroadcaster()
	sub := publisher.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()
	sink := &captureHookSink{}
	s := daemon.NewDueNotificationSweeper(env.DB, daemon.NewEventPublisher(publisher, sink), "UTC")
	now := time.Now().UTC().Add(3 * time.Hour)
	require.ErrorContains(t, s.RunOnce(ctx, now), bad.UID, "operators can find the failing job")
	children, e := env.DB.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID})
	require.NoError(t, e)
	require.Len(t, children, 100, "one malformed and 99 healthy candidates on first page")
	require.Len(t, sink.events, 198)
	for _, event := range sink.events {
		if event.Type == "issue.created" {
			require.NotNil(t, event.IssueUID)
			_, e = env.DB.IssueByUID(ctx, *event.IssueUID, db.IncludeDeletedNo)
			require.NoError(t, e, "publication observes committed child")
		}
	}
	require.NoError(t, s.RunOnce(ctx, now))
	children, e = env.DB.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID})
	require.NoError(t, e)
	require.Len(t, children, 102, "remaining jobs are delivered on the next tick")
	s.IdleAdmission = func() (*activity.Lease, bool, <-chan struct{}) { return nil, false, nil }
	require.NoError(t, s.RunOnce(ctx, now.Add(3*time.Hour)))
	require.Len(t, sink.events, 202, "withdrawn admission writes and publishes nothing")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	s.IdleAdmission = nil
	require.ErrorIs(t, s.RunOnce(canceled, now), context.Canceled)
}

type recurringListFailure struct{ db.Storage }

func (s recurringListFailure) ListRecurringCronJobs(context.Context, string, int) ([]db.RecurringCronCandidate, error) {
	return nil, errors.New("injected native listing failure")
}

func TestSweepNativeListingFailureKeepsDateNotifications(t *testing.T) {
	env := testenv.New(t)
	p, e := env.DB.CreateProject(t.Context(), "planning-project")
	require.NoError(t, e)
	_, _, e = env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Due work", Author: "operator", Metadata: map[string]jsontext.Value{"scheduled_on": []byte(`"2026-01-01"`)}})
	require.NoError(t, e)
	sink := &captureHookSink{}
	s := daemon.NewDueNotificationSweeper(recurringListFailure{env.DB}, daemon.NewEventPublisher(daemon.NewEventBroadcaster(), sink), "UTC")
	require.ErrorContains(t, s.RunOnce(t.Context(), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)), "injected native listing failure")
	require.Len(t, sink.events, 2)
	require.Equal(t, "issue.scheduled_reached", sink.events[0].Type)
	require.Equal(t, "issue.metadata_updated", sink.events[1].Type)
}

// One sweep drains an all-catch-up backlog larger than a single dispatch batch.
func TestSweepDrainsRecurringBacklog(t *testing.T) {
	ctx := t.Context()
	env := testenv.New(t)
	p, e := env.DB.CreateProject(ctx, "recurring-project")
	require.NoError(t, e)
	parent, _, e := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Continuing commitment", Author: "operator"})
	require.NoError(t, e)
	j := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200}, Action: cron.Action{Kind: "notify", Recipient: "current-owner-or-author"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Review"}, Overlap: "allow", Catchup: "all"}
	_, _, e = env.DB.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Review", Actor: "operator", Definition: j})
	require.NoError(t, e)
	s := daemon.NewDueNotificationSweeper(env.DB, daemon.NewEventPublisher(daemon.NewEventBroadcaster(), &captureHookSink{}), "UTC")
	require.NoError(t, s.RunOnce(ctx, time.Now().UTC().Add(10*24*time.Hour+time.Minute)))
	children, e := env.DB.ChildrenOfIssue(ctx, parent.ID)
	require.NoError(t, e)
	require.Equal(t, 120, len(children), "ten days of two-hour occurrences arrive in one sweep")
}
