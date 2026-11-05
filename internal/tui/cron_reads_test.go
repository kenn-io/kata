package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
)

// Contract: the existing run route must receive the exact job/page constraints,
// and callers need the server cursor to discover runs outside the project head.
func TestCronClientPages(t *testing.T) {
	requests := make(chan *http.Request, 1)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"runs":[],"next_before_uid":"next-example"}`))
	})
	runs, next, err := c.ListCronJobRuns(t.Context(), 7, "job-example", "before-example", 20)
	require.NoError(t, err)
	require.Len(t, requests, 1, "the client must issue exactly one request")
	req := <-requests
	require.Equal(t, "/api/v1/projects/7/cron/runs", req.URL.Path)
	require.Equal(t, "job-example", req.URL.Query().Get("job_uid"))
	require.Equal(t, "before-example", req.URL.Query().Get("before_uid"))
	require.Equal(t, "20", req.URL.Query().Get("limit"))

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
	require.EqualError(t, p.lastErr["foreign"], "received runs for a different job")
	require.NotContains(t, p.lastRuns, "foreign", "a mismatched reply cannot replace job evidence")
	// A failed shared read cannot prevent a successful job-specific read.
	p = cronData{jobs: jobs[:1], runsErr: errors.New("head unavailable")}
	cronEnrich(t.Context(), a, 7, &p)
	require.Equal(t, "old-run", p.lastRuns["head"].UID)
	require.NoError(t, p.lastErr["head"])
}

func TestCronEnrichmentAdmissionAndWorkerCaps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		t.Cleanup(func() {
			select {
			case <-release:
			default:
				close(release)
			}
		})
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
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("enrichment worker did not start")
			}
		}
		close(release)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("enrichment did not finish")
		}
		require.Equal(t, 200, a.calls)
		require.Equal(t, 200, a.parents)
		// Fills block together; parent reads run later. This checks per-stage caps.
		require.LessOrEqual(t, a.peak, 4)
		require.Greater(t, a.peak, 1)
		require.ErrorContains(t, p.lastErr["204"], "200 jobs per refresh")
		require.NotContains(t, p.parentOpen, "parent-204")
	})
	t.Run("adapter and enabled priority", func(t *testing.T) {
		j := cronFixture(t).cron.data.jobs[0]
		adapter := j
		adapter.UID = "quiet-adapter"
		p := cronData{}
		for i := range 201 {
			native := j
			native.UID = fmt.Sprintf("native-%03d", i)
			nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))})(&native.Definition)
			p.jobs = append(p.jobs, native)
		}
		p.jobs = append(p.jobs, adapter)
		a := &cronEnrichAPI{read: func(_ context.Context, job string) ([]generated.CronRun, error) {
			return []generated.CronRun{{UID: job + "-run", JobUID: new(job), Status: "succeeded"}}, nil
		}}
		cronEnrich(t.Context(), a, 7, &p)
		require.NoError(t, p.lastErr[adapter.UID])
		require.Equal(t, adapter.UID+"-run", p.lastRuns[adapter.UID].UID)
		require.Equal(t, "native-000-run", p.lastRuns["native-000"].UID, "native observations still feed exact display and status filters")
		require.Error(t, p.lastErr["native-200"], "native display lookups remain bounded")
		p = cronData{}
		for i := range 210 {
			job := j
			job.UID = fmt.Sprintf("ordered-%03d", i)
			job.Definition.Enabled = i >= 150
			p.jobs = append(p.jobs, job)
		}
		cronEnrich(t.Context(), a, 7, &p)
		for _, job := range p.jobs[150:] {
			require.NoError(t, p.lastErr[job.UID], "enabled jobs must be admitted before paused display reads")
			require.Equal(t, job.UID+"-run", p.lastRuns[job.UID].UID)
		}
	})
}

func TestCronEnrichmentCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := cronFixture(t).cron.data.jobs[0]
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		entered := make(chan struct{})
		a := &cronEnrichAPI{read: func(ctx context.Context, _ string) ([]generated.CronRun, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		p := cronData{jobs: []generated.CronJob{j}}
		done := make(chan struct{})
		go func() { cronEnrich(ctx, a, 7, &p); close(done) }()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("enrichment read did not start")
		}
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("enrichment did not cancel")
		}
		require.ErrorIs(t, p.lastErr[j.UID], context.Canceled)
	})
	t.Run("pre-cancelled context performs no exact read", func(t *testing.T) {
		m, a := cronRefreshFixture(t, 12)
		r := m.cron.runPage.runs[0]
		p := cronData{inspectedRuns: []generated.CronRun{r}, historyRuns: m.cron.runPage.runs}
		a.exact = func(context.Context, string) (generated.CronRun, error) {
			t.Error("cancelled read performed I/O")
			return r, nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		cronEnrich(ctx, a, 7, &p)
		require.Empty(t, a.reads)
		require.ErrorIs(t, p.runErr[r.UID], context.Canceled)
		require.Equal(t, r, p.lookup[r.UID])
	})
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

func TestCronRunClientByUID(t *testing.T) {
	requests := make(chan *http.Request, 1)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		respondJSON(t, w, map[string]any{"run": map[string]any{"uid": "run-example", "status": "failed", "revision": 2}})
	})
	reader, ok := any(newUndoClient(c)).(cronRunAPI)
	require.True(t, ok)
	r, err := reader.readCronRun(t.Context(), 7, "run-example")
	require.NoError(t, err)
	require.Len(t, requests, 1, "the client must issue exactly one request")
	req := <-requests
	require.Equal(t, "/api/v1/projects/7/cron/runs/run-example", req.URL.Path)

	require.Equal(t, "run-example", r.UID)
	require.Equal(t, "failed", r.Status)
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

	t.Run("jobs error keeps fresh head evidence", func(t *testing.T) {
		m := cronSeenFixture(t)
		job := m.cron.data.jobs[0]
		fresh := m.cron.data.runs[0]
		fresh.UID, fresh.Status = "new-head-run", "failed"
		fresh.CreatedAt = fresh.CreatedAt.Add(time.Hour)
		data := *m.cron.data
		data.jobsErr = errors.New("jobs unavailable")
		data.runs = []generated.CronRun{fresh}
		m.api = &cronReadTestAPI{data: data}
		msg := m.fetchCron()().(cronLoadedMsg)
		require.Equal(t, fresh.UID, msg.lastRuns[job.UID].UID, "the successful head read supplies fresh last-run evidence")
		m = m.cronLoaded(msg)
		require.Equal(t, "unavailable", cronHealth(*m.cron.data, job, snapshotFixedNow).state)
		require.Contains(t, cronJobLast(*m.cron.data, job.UID), "failed", "Last run must retain the successful head read despite the unrelated jobs failure")
	})
}

func TestCronEventRefreshCoalescesQuietJobFills(t *testing.T) {
	m := cronSeenFixture(t)
	data := *m.cron.data
	for i := range 2 {
		j := data.jobs[0]
		j.UID = fmt.Sprintf("quiet-job-%d", i)
		data.jobs = append(data.jobs, j)
	}
	a := &cronFanoutAPI{cronReadTestAPI: &cronReadTestAPI{data: data}}
	m.api = a
	defer func() { m.cron.reads.stop() }()
	for range 3 {
		// This is Update's event handler followed by its Cron scheduling hook.
		// Leave the debounce command pending while completing any immediate fetch.
		next, _ := m.handleEventReceived(eventReceivedMsg{eventType: "cron.run.observed", projectID: 7})
		m = next.(Model)
		nextModel, cmd := m.cronSync()
		m = nextModel
		if cmd != nil {
			m = m.cronLoaded(cmd().(cronLoadedMsg))
		}
	}
	require.Equal(t, int64(2), a.fills.Load(), "one pending event debounce must issue exactly one quiet-job sweep")
}

type cronFanoutAPI struct {
	*cronReadTestAPI
	fills atomic.Int64
}

func (a *cronFanoutAPI) ListCronJobRuns(ctx context.Context, id int64, job, before string, limit int64) ([]generated.CronRun, string, error) {
	if job == "" {
		return a.cronReadTestAPI.ListCronJobRuns(ctx, id, job, before, limit)
	}
	a.fills.Add(1)
	return nil, "", nil // These enabled jobs have never run.
}
