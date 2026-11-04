package tui

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/pkg/client/generated"
	"hegel.dev/go/hegel"
)

func TestCronWaitOnlyExactNormalizedActorCounts(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		arbitrary := hegel.Draw(ht, hegel.Text().MaxSize(180))
		valid := hegel.Draw(ht, hegel.SampledFrom([]string{"operator", " operator ", "operator/reader", "Équipe"}))
		for _, actor := range []string{valid, arbitrary, arbitrary + "\x00"} {
			normalized, err := notification.NormalizeRecipient(actor)
			metadata := map[string]any{notification.MetadataKey("other-recipient"): map[string]any{"from": "worker", "message": "unrelated"}}
			if err == nil {
				metadata[notification.MetadataKey(normalized)] = map[string]any{"from": "worker", "message": "review\nnow"}
			}
			got := cronRunWaitForActor(cronParent{open: true, metadata: metadata}, actor)
			if (got.state == "blocked") != (err == nil) {
				ht.Fatalf("actor %q: %v, normalization error %v", actor, got, err)
			}
			if strings.ContainsAny(got.reason, "\r\n\x1b") {
				ht.Fatalf("wait reason is not a sanitized line: %q", got.reason)
			}
		}
	}, hegel.WithTestCases(300))
}

func TestCronWaitUnavailableOrMalformedNeverBlocks(t *testing.T) {
	r := generated.CronRun{Status: "running", Actor: "operator", IssueUID: new("issue-example")}
	p := cronData{waitErr: map[string]error{"issue-example": errors.New("offline")}}
	require.Equal(t, "running", cronRunHealth(p, r).state)
	require.Contains(t, cronRunHealth(p, r).reason, "attention unavailable")
	require.Contains(t, cronRunHealth(cronData{}, r).reason, "attention unavailable")
	for _, value := range []any{nil, "invalid", map[string]any{"from": "worker"}, map[string]any{"message": "review"}, map[string]any{"from": "worker", "message": " "}} {
		got := cronRunWaitForActor(cronParent{open: true, metadata: map[string]any{notification.MetadataKey("operator"): value}}, r.Actor)
		require.NotEqual(t, "blocked", got.state)
	}

	{
		got := cronRunHealth(cronData{}, generated.CronRun{Status: "running"})
		require.Equal(t, "running", got.state)
		require.Empty(t, got.reason)
	}
}

func TestCronWaitClientReadsOpenAndDeletedMetadata(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprint(deleted), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/api/v1/projects/7/issues/issue-example", r.URL.Path)
				require.Equal(t, "true", r.URL.Query().Get("include_deleted"))
				w.Header().Set("Content-Type", "application/json")
				deletedAt := "null"
				if deleted {
					deletedAt = `"2026-05-01T12:00:00Z"`
				}
				_, _ = w.Write([]byte(`{"issue":{"status":"open","deleted_at":` + deletedAt + `,"metadata":{"work.attention":"needs-human"}}}`))
			}))
			defer srv.Close()
			got, err := NewClient(srv.URL, srv.Client()).readCronParent(t.Context(), 7, "issue-example")
			require.NoError(t, err)
			require.Equal(t, !deleted, got.open)
			require.Equal(t, "needs-human", got.metadata["work.attention"])
		})
	}
}

// Contract: a wait is an inference from a running observation plus an open
// issue's work.attention or the exact run actor's validated notification.
func TestCronWaitBlockedIffRunningOpenAndSignalled(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		statuses := []string{"running", "failed", "unknown", "succeeded", "cancelled"}
		attentions := []string{"needs-human", "stuck", "ok", "", "unrecognized"}
		status := hegel.Draw(ht, hegel.SampledFrom(statuses))
		attention := hegel.Draw(ht, hegel.SampledFrom(attentions))
		open := hegel.Draw(ht, hegel.Booleans())
		request := hegel.Draw(ht, hegel.Booleans())
		r := generated.CronRun{Status: status, Actor: " operator ", IssueUID: new("issue-example")}
		metadata := map[string]any{"work.attention": attention}
		if request {
			metadata[notification.MetadataKey("operator")] = map[string]any{"from": "worker", "message": "please review"}
		}
		p := cronData{waits: map[string]cronParent{"issue-example": {open: open, metadata: metadata}}}
		want := status == "running" && open && (request || attention == "needs-human" || attention == "stuck")
		got := cronRunHealth(p, r)
		if request {
			if _, ok := metadata[notification.MetadataKey("operator")]; !ok {
				ht.Fatal("reading attention must not consume requests")
			}
		}
		if (got.state == "blocked") != want {
			ht.Fatalf("status=%q open=%v attention=%q request=%v: got %v want blocked=%v", status, open, attention, request, got, want)
		}
	}, hegel.WithTestCases(600))

	t.Run("both signals must clear", func(t *testing.T) {
		key := notification.MetadataKey("operator")
		w := cronParent{open: true, metadata: map[string]any{"work.attention": "needs-human", key: map[string]any{"from": "worker", "message": "review"}}}
		require.Equal(t, "blocked", cronRunWaitForActor(w, "operator").state)
		require.Contains(t, w.metadata, key, "reading attention must not consume the request")
		delete(w.metadata, key)
		require.Equal(t, "blocked", cronRunWaitForActor(w, "operator").state)
		w.metadata["work.attention"] = "ok"
		require.Equal(t, "running", cronRunWaitForActor(w, "operator").state)
		w.metadata[key] = map[string]any{"from": "worker", "message": "review"}
		require.Equal(t, "blocked", cronRunWaitForActor(w, "operator").state)
		w.open = false
		require.Equal(t, "running", cronRunWaitForActor(w, "operator").state)
	})
}

func TestCronHealthOverdueWinsAndNativeIgnoresWait(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	j.Definition.Trigger = generated.TriggerResponse{Kind: "manual"}
	j.Definition.TimeoutSeconds = new(int64(60))
	r := generated.CronRun{Status: "running", Actor: "operator", IssueUID: new("issue-example"), JobUID: new(j.UID), CreatedAt: snapshotFixedNow.Add(-time.Hour)}
	p := cronData{jobs: []generated.CronJob{j}, lastRuns: map[string]generated.CronRun{j.UID: r}, lastErr: map[string]error{}, waits: map[string]cronParent{"issue-example": {open: true, metadata: map[string]any{"work.attention": "stuck"}}}}
	got := cronHealth(p, j, snapshotFixedNow)
	require.Equal(t, "overdue", got.state)
	require.Contains(t, got.reason, "Stuck")
	j.Definition.TimeoutSeconds = nil
	require.Equal(t, "blocked", cronHealth(p, j, snapshotFixedNow).state)
	nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))})(&j.Definition)
	p.jobs = []generated.CronJob{j}
	p.parentOpen = map[string]bool{nextParentUID: true}
	p.progress = map[string]cron.RecurringProgress{j.UID: {Version: 1, Anchor: snapshotFixedNow, Through: snapshotFixedNow}}
	require.Equal(t, "ok", cronHealth(p, j, snapshotFixedNow).state)
	require.NotEqual(t, "blocked", cronRunHealth(p, r).state)
}
