package testenv

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv/sqlcount"
)

// Federation poll benchmark shape: a hub with many projects, one polled
// project with a large backlog, and one unrelated project that keeps writing.
const (
	federationPollProjects    = 50
	federationPollIssues      = 1000
	federationPollBusyPause   = 2 * time.Millisecond
	federationPollTokenSecret = "federation-poll-benchmark-token" //nolint:gosec // G101: test-only bearer token.
)

// BenchmarkFederationPoll measures the hub's cost for one spoke poll through
// the daemon HTTP handler, including authentication and authorization. store
// must count statements run under a sqlcount.Counter context; each
// sub-benchmark reports them as stmts/op.
func BenchmarkFederationPoll(b *testing.B, store db.Storage) {
	ctx := b.Context()
	hub := seedFederationPollHub(b, store)

	server := daemon.NewServer(daemon.ServerConfig{
		DB:        store,
		StartedAt: time.Now().UTC(),
		Auth:      config.AuthConfig{Token: "hub-admin-token", TrustPrivateNetwork: true},
	})
	b.Cleanup(func() { _ = server.Close() })
	poll := func(ctx context.Context, afterID int64) api.PollEventsBody {
		path := fmt.Sprintf("/api/v1/projects/%d/federation/events?after_id=%d&limit=1000", hub.project.ID, afterID)
		request := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		request.RemoteAddr = "100.64.0.9:41000"
		request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("100.64.0.5"), Port: 7777}))
		request.Header.Set("Authorization", "Bearer "+federationPollTokenSecret)
		request.Header.Set("X-Kata-Event-Features", db.SupportedEventFeatures)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			b.Fatalf("poll after %d: status %d: %s", afterID, recorder.Code, recorder.Body.String())
		}
		var body api.PollEventsBody
		require.NoError(b, json.Unmarshal(recorder.Body.Bytes(), &body))
		return body
	}

	// Catch up the way a spoke does before the project goes quiet.
	for cursor := int64(0); ; {
		page := poll(ctx, cursor)
		if len(page.Events) == 0 {
			break
		}
		cursor = page.NextAfterID
	}
	_, latest, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: hub.issue.ID, Body: "one new event", Author: "tester"})
	require.NoError(b, err)

	busyCtx, stopBusy := context.WithCancel(ctx)
	var busy sync.WaitGroup
	busy.Go(func() {
		for busyCtx.Err() == nil {
			_, _, err := store.CreateComment(busyCtx, db.CreateCommentParams{IssueID: hub.busyIssue.ID, Body: "busy", Author: "tester"})
			if err != nil && busyCtx.Err() == nil {
				b.Errorf("busy project write: %v", err)
				return
			}
			select {
			case <-busyCtx.Done():
			case <-time.After(federationPollBusyPause):
			}
		}
	})
	b.Cleanup(func() {
		stopBusy()
		busy.Wait()
	})

	run := func(name string, afterID int64, wantEvents int) {
		b.Run(name, func(b *testing.B) {
			var counter sqlcount.Counter
			pollCtx := counter.Context(b.Context())
			if got := len(poll(pollCtx, afterID).Events); got != wantEvents {
				b.Fatalf("poll after %d returned %d events, want %d", afterID, got, wantEvents)
			}
			counter.Reset()
			for b.Loop() {
				poll(pollCtx, afterID)
			}
			b.ReportMetric(float64(counter.Count())/float64(b.N), "stmts/op")
		})
	}
	run("empty", latest.ID, 0)
	run("one_event", latest.ID-1, 1)
	run("first_page", 0, 1000)
}

type federationPollHub struct {
	project   db.Project
	issue     db.Issue
	busyIssue db.Issue
}

func seedFederationPollHub(tb testing.TB, store db.Storage) federationPollHub {
	tb.Helper()
	ctx := tb.Context()
	var hub federationPollHub
	for i := range federationPollProjects {
		project, err := store.CreateProject(ctx, fmt.Sprintf("hub-project-%02d", i))
		require.NoError(tb, err)
		_, err = store.EnableProjectFederation(ctx, project.ID, "tester")
		require.NoError(tb, err)
		issues := 5
		if i == 0 {
			hub.project, issues = project, federationPollIssues
		}
		for range issues {
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "work", Author: "tester"})
			require.NoError(tb, err)
			switch {
			case i == 0 && hub.issue.ID == 0:
				hub.issue = issue
			case i == 1 && hub.busyIssue.ID == 0:
				hub.busyIssue = issue
			}
		}
	}
	_, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            federationPollTokenSecret,
		SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
		ProjectID:        &hub.project.ID,
		Capabilities:     "pull",
		Actor:            "spoke-actor",
	})
	require.NoError(tb, err)
	return hub
}
