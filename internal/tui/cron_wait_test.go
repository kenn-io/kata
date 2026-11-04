package tui

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/pkg/client/generated"
	"hegel.dev/go/hegel"
)

func TestCronWaitOnlyExactNormalizedActorCounts(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		actor := hegel.Draw(ht, hegel.Text().MaxSize(180))
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
	}, hegel.WithTestCases(300))
}

func TestCronWaitUnavailableOrMalformedNeverBlocks(t *testing.T) {
	r := generated.CronRun{Status: "running", Actor: "operator", IssueUID: new("issue-example")}
	p := cronData{waits: map[string]cronParent{"issue-example": {open: true, metadata: map[string]any{"work.attention": "stuck"}}}, waitErr: map[string]error{"issue-example": errors.New("offline")}}
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
		status := statuses[hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0)))%uint64(len(statuses))]
		attention := attentions[hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0)))%uint64(len(attentions))]
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
			require.Contains(t, metadata, notification.MetadataKey("operator"), "reading attention must not consume requests")
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
