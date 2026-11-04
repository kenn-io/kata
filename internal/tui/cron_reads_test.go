package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
)

// Contract: the existing run route must receive the exact job/page constraints,
// and callers need the server cursor to discover runs outside the project head.
func TestCronClientPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/projects/7/cron/runs", r.URL.Path)
		require.Equal(t, "job-example", r.URL.Query().Get("job_uid"))
		require.Equal(t, "before-example", r.URL.Query().Get("before_uid"))
		require.Equal(t, "20", r.URL.Query().Get("limit"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"runs":[],"next_before_uid":"next-example"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, srv.Client())
	runs, next, err := c.ListCronJobRuns(t.Context(), 7, "job-example", "before-example", 20)
	require.NoError(t, err)
	require.Empty(t, runs)
	require.Equal(t, "next-example", next)
}

type cronEnrichAPI struct {
	KataAPI
	mu                           sync.Mutex
	calls, active, peak, parents int
	read                         func(context.Context, string) ([]generated.CronRun, error)
}

func (a *cronEnrichAPI) ListCronJobRuns(ctx context.Context, _ int64, job, before string, limit int64) ([]generated.CronRun, string, error) {
	a.mu.Lock()
	a.calls++
	a.active++
	a.peak = max(a.peak, a.active)
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.active--; a.mu.Unlock() }()
	if before != "" || limit != 1 {
		return nil, "", fmt.Errorf("unexpected fill constraints")
	}
	runs, err := a.read(ctx, job)
	return runs, "", err
}

func (a *cronEnrichAPI) readCronParent(ctx context.Context, _ int64, _ string) (cronParent, error) {
	a.mu.Lock()
	a.parents++
	a.active++
	a.peak = max(a.peak, a.active)
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.active--; a.mu.Unlock() }()
	if err := ctx.Err(); err != nil {
		return cronParent{}, err
	}
	return cronParent{open: true, progress: map[string]cron.RecurringProgress{}}, nil
}

func TestCronEnrichmentExactAndIsolated(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	jobs := []generated.CronJob{}
	for _, uid := range []string{"head", "quiet", "empty", "failed", "foreign"} {
		j.UID = uid
		jobs = append(jobs, j)
	}
	a := &cronEnrichAPI{read: func(_ context.Context, job string) ([]generated.CronRun, error) {
		switch job {
		case "empty":
			return nil, nil
		case "failed":
			return nil, errors.New("offline")
		case "foreign":
			return []generated.CronRun{{JobUID: new("different")}}, nil
		default:
			return []generated.CronRun{{UID: "old-run", JobUID: new(job), Status: "succeeded"}}, nil
		}
	}}
	p := cronData{jobs: jobs, runs: []generated.CronRun{{UID: "head-run", JobUID: new("head"), Status: "failed"}}}
	cronEnrich(t.Context(), a, 7, &p)
	require.Equal(t, 4, a.calls)
	require.Equal(t, "head-run", p.lastRuns["head"].UID)
	require.Equal(t, "old-run", p.lastRuns["quiet"].UID)
	require.NotContains(t, p.lastRuns, "empty")
	require.NoError(t, p.lastErr["empty"])
	require.ErrorContains(t, p.lastErr["failed"], "offline")
	require.ErrorContains(t, p.lastErr["foreign"], "job")
	// A failed shared read cannot prevent a successful job-specific read.
	p = cronData{jobs: jobs[:1], runsErr: errors.New("head unavailable")}
	cronEnrich(t.Context(), a, 7, &p)
	require.Equal(t, "old-run", p.lastRuns["head"].UID)
	require.NoError(t, p.lastErr["head"])
}

func TestCronEnrichmentCapsAndSharedWorkers(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	p := cronData{}
	for i := range 205 {
		j.UID = fmt.Sprint(i)
		p.jobs = append(p.jobs, j)
	}
	for i := range 205 {
		j.UID = "native-" + fmt.Sprint(i)
		nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))})(&j.Definition)
		j.Definition.Issue.ParentUID = new("parent-" + fmt.Sprint(i))
		p.jobs = append(p.jobs, j)
	}
	entered, release := make(chan struct{}, 200), make(chan struct{})
	a := &cronEnrichAPI{read: func(ctx context.Context, _ string) ([]generated.CronRun, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	done := make(chan struct{})
	go func() { cronEnrich(t.Context(), a, 7, &p); close(done) }()
	for range 4 {
		<-entered
	}
	close(release)
	<-done
	require.Equal(t, 200, a.calls)
	require.Equal(t, 200, a.parents)
	require.LessOrEqual(t, a.peak, 4)
	require.Greater(t, a.peak, 1)
	require.ErrorContains(t, p.lastErr["204"], "200 jobs per refresh")
	require.NotContains(t, p.parentOpen, "parent-204")
}

func TestCronEnrichmentCancellation(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	ctx, cancel := context.WithCancel(t.Context())
	entered := make(chan struct{})
	a := &cronEnrichAPI{read: func(ctx context.Context, _ string) ([]generated.CronRun, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	p := cronData{jobs: []generated.CronJob{j}}
	done := make(chan struct{})
	go func() { cronEnrich(ctx, a, 7, &p); close(done) }()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("enrichment did not cancel")
	}
	require.ErrorIs(t, p.lastErr[j.UID], context.Canceled)
}

func TestCronRefreshLastEvidence(t *testing.T) {
	m := cronFixture(t)
	j := m.cron.data.jobs[0]
	old := m.cron.data.runs[0]
	m.cron.data.lastRuns = map[string]generated.CronRun{j.UID: old}
	msg := cronLoadedMsg{conn: m.connGen, scope: m.scopeGen, jobs: []generated.CronJob{j}, lastRuns: map[string]generated.CronRun{}, lastErr: map[string]error{j.UID: errors.New("offline")}}
	m = m.cronLoaded(msg)
	require.Equal(t, old.UID, m.cron.data.lastRuns[j.UID].UID)
	require.Equal(t, "unavailable", cronHealth(*m.cron.data, j, snapshotFixedNow).state)
	msg.lastErr = map[string]error{}
	m = m.cronLoaded(msg)
	require.NotContains(t, m.cron.data.lastRuns, j.UID, "fresh empty must clear display-only evidence")
}

func TestCronVisitCancelsAndFencesRefresh(t *testing.T) {
	m := cronFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.lifetime = &cronReadScope{ctx: ctx, cancel: cancel}
	m.cron.events++
	a := &cronReadTestAPI{data: *m.cron.data}
	m.api = a
	m, cmd := m.cronSync()
	oldGen := m.cron.readGen
	readCtx := m.cron.reads.ctx
	m = sendRune(m, '1')
	require.ErrorIs(t, readCtx.Err(), context.Canceled)
	msg := cmd().(cronLoadedMsg)
	require.Equal(t, oldGen, msg.generation)
	m = sendRune(m, '2')
	require.Greater(t, m.cron.readGen, oldGen)
	before := m.cron.data
	m = m.cronLoaded(msg)
	require.Same(t, before, m.cron.data, "a canceled visit cannot overwrite a new visit")
	currentCtx := m.cron.reads.ctx
	cancel()
	require.ErrorIs(t, currentCtx.Err(), context.Canceled, "TUI shutdown cancels every Cron child")
}
