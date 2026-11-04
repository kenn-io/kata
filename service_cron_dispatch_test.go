package kata

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// The embedded worker must retain the same committed observations served by
// its HTTP handler, including skip-only progress that creates no child event.
func TestServiceCronDispatchHistory(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	s, err := New(t.Context(), Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	p, err := s.store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	parent, _, err := s.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Review", Author: "operator"})
	require.NoError(t, err)
	job, _, err := s.store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Review", Actor: "operator", Definition: cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200}, Action: cron.Action{Kind: "notify", Recipient: "operator"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Review"}, Overlap: "allow", Catchup: "skip", GraceSeconds: 1}})
	require.NoError(t, err)
	anchor := time.Now().UTC().Add(-3 * time.Hour)
	metadata, err := json.Marshal(map[string]any{"cron.progress." + job.UID: cron.RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}})
	require.NoError(t, err)
	// Seed old progress in this isolated fixture without waiting two hours.
	q := s.store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = q.ExecContext(t.Context(), "UPDATE issues SET metadata=$1 WHERE id=$2", string(metadata), parent.ID)
	require.NoError(t, err)
	sub := s.broadcaster.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(cancel)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	committed := false
	for !committed {
		select {
		case message := <-sub.Ch:
			committed = message.Event != nil && message.Event.Type == "issue.metadata_updated"
		case <-timer.C:
			t.Fatal("recurring progress was never committed")
		}
	}
	// Join the worker after its commit signal. Its recorder must finish before
	// Run returns, and the service must retain diagnostics until Close.
	cancel()
	require.NoError(t, <-done)
	var explanation api.CronDispatchExplanation
	var health struct {
		Cron *api.CronHealth `json:"cron"`
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/dispatch", p.ID, job.UID), nil)
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &explanation))
	healthResponse := httptest.NewRecorder()
	s.Handler().ServeHTTP(healthResponse, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/health", nil))
	require.Equal(t, http.StatusOK, healthResponse.Code)
	require.NoError(t, json.Unmarshal(healthResponse.Body.Bytes(), &health))
	require.NotNil(t, explanation.Last)
	require.Equal(t, cron.DispatchSkipped, explanation.Last.Reason)
	require.Equal(t, 1, explanation.Last.Skipped.Count)
	require.NotNil(t, explanation.LastNonIdle)
	require.Equal(t, cron.DispatchSkipped, explanation.LastNonIdle.Reason)
	require.NotNil(t, health.Cron)
	require.EqualValues(t, 1, health.Cron.SkippedSinceStart)
	raw, err := os.ReadFile(health.Cron.LogPath)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"reason":"skipped"`)
}

// Explicit storage must remain usable when optional diagnostics have no home.
func TestServiceCronDispatchStartsWithoutHome(t *testing.T) {
	t.Setenv("KATA_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	s, err := New(t.Context(), Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	_, err = s.cronRecorder.Record(t.Context(), db.RecurringCronCandidate{ProjectID: 1, JobUID: "job"}, db.DispatchRecurringCronOut{Reason: cron.DispatchIdle}, nil, time.Now())
	require.Error(t, err, "unavailable diagnostic home is an observable log failure")
	s.cronRecorder.FinishCycle(time.Now(), 0, true, false)
	require.True(t, s.cronRecorder.Health().LogErrorPresent)
}
