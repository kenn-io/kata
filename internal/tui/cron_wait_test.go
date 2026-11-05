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
			requests := make(chan *http.Request, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r
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
			require.Len(t, requests, 1, "the client must issue exactly one request")
			req := <-requests
			require.Equal(t, "/api/v1/projects/7/issues/issue-example", req.URL.Path)
			require.Equal(t, "true", req.URL.Query().Get("include_deleted"))

			require.Equal(t, !deleted, got.open)
			require.Equal(t, "needs-human", got.metadata["work.attention"])
		})
	}
}

// Contract: a wait is an inference from a running observation plus an open
// issue's work.attention or the exact run actor's validated notification.
func TestCronWaitBlockedIffRunningOpenAndSignalled(t *testing.T) {
	// Enumerate the finite contract so removing the transition examples cannot
	// lose their cases to random sampling. Both normalized actor forms matter.
	for _, status := range []string{"running", "failed", "unknown", "succeeded", "cancelled"} {
		for _, attention := range []string{"needs-human", "stuck", "ok", "", "unrecognized"} {
			for _, open := range []bool{false, true} {
				for _, request := range []bool{false, true} {
					for _, actor := range []string{"operator", " operator "} {
						r := generated.CronRun{Status: status, Actor: actor, IssueUID: new("issue-example")}
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
						require.Equal(t, want, got.state == "blocked", "status=%q open=%v attention=%q request=%v actor=%q: %v", status, open, attention, request, actor, got)
					}
				}
			}
		}
	}
}
