package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/pkg/client/generated"
	"hegel.dev/go/hegel"
)

type cronWaitReadAPI struct {
	*cronEnrichAPI
	muWait    sync.Mutex
	waitCalls map[string]int
	wait      func(context.Context, string) (cronParent, error)
}

func (a *cronWaitReadAPI) readCronParent(ctx context.Context, _ int64, uid string) (cronParent, error) {
	a.muWait.Lock()
	a.waitCalls[uid]++
	a.muWait.Unlock()
	a.mu.Lock()
	a.active++
	a.peak = max(a.peak, a.active)
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.active--; a.mu.Unlock() }()
	return a.wait(ctx, uid)
}
func waitReadAPI(read func(context.Context, string) ([]generated.CronRun, error)) *cronWaitReadAPI {
	return &cronWaitReadAPI{cronEnrichAPI: &cronEnrichAPI{read: read}, waitCalls: map[string]int{}, wait: func(context.Context, string) (cronParent, error) {
		return cronParent{open: true, metadata: map[string]any{notification.MetadataKey("operator"): map[string]any{"from": "worker", "message": "review"}}}, nil
	}}
}

func TestCronWaitReadFillAndHistoryRevisions(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	p := cronData{jobs: []generated.CronJob{j}, historyRuns: []generated.CronRun{
		{UID: "history", Revision: 1, Status: "running", IssueUID: new("old")},
		{UID: "ended", Revision: 1, Status: "running", IssueUID: new("ended-issue")},
	}, runs: []generated.CronRun{{UID: "ended", Revision: 2, Status: "succeeded", IssueUID: new("ended-issue")}}}
	a := waitReadAPI(func(_ context.Context, job string) ([]generated.CronRun, error) {
		return []generated.CronRun{{UID: "fill", JobUID: new(job), Status: "running", IssueUID: new("fill-issue")}}, nil
	})
	cronEnrich(t.Context(), a, 7, &p)
	require.Equal(t, map[string]int{"old": 1, "fill-issue": 1}, a.waitCalls)
	require.Contains(t, p.waits, "old")
	// Fresh reads must discard prior wait evidence, when no runs remain.
	p.runs = nil
	p.historyRuns = nil
	p.jobs = nil
	cronEnrich(t.Context(), a, 7, &p)
	require.Empty(t, p.waits)
}

func TestCronWaitReadsBoundedWithDeadlineAndCancellation(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	p := cronData{}
	for i := range 8 {
		j.UID = fmt.Sprint(i)
		p.jobs = append(p.jobs, j)
	}
	var fillDeadline time.Time
	var deadlineMu sync.Mutex
	a := waitReadAPI(func(ctx context.Context, job string) ([]generated.CronRun, error) {
		deadlineMu.Lock()
		var ok bool
		fillDeadline, ok = ctx.Deadline()
		deadlineMu.Unlock()
		if !ok {
			return nil, errors.New("fill context has no deadline")
		}
		return []generated.CronRun{{UID: job, JobUID: new(job), Status: "running", IssueUID: new(job)}}, nil
	})
	entered := make(chan struct{}, 20)
	a.wait = func(ctx context.Context, _ string) (cronParent, error) {
		entered <- struct{}{}
		got, ok := ctx.Deadline()
		if !ok {
			return cronParent{}, errors.New("wait context has no deadline")
		}
		deadlineMu.Lock()
		want := fillDeadline
		deadlineMu.Unlock()
		if !got.Equal(want) {
			return cronParent{}, fmt.Errorf("wait received a different deadline")
		}
		<-ctx.Done()
		return cronParent{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { cronEnrich(ctx, a, 7, &p); close(done) }()
	select {
	case <-entered:
	case <-done:
		t.Fatal("no attention read was admitted")
	case <-time.After(defaultHTTPTimeout):
		t.Fatal("attention read did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("enrichment did not cancel")
	}
	// Fills precede waits; this bounds each stage, not overlapping pools.
	require.LessOrEqual(t, a.peak, 4)
	require.NotEmpty(t, a.waitCalls)
	for uid := range a.waitCalls {
		require.ErrorIs(t, p.waitErr[uid], context.Canceled)
		require.NotContains(t, p.waits, uid)
	}
}

func TestCronWaitRefreshCapturesLoadedHistory(t *testing.T) {
	m := cronFixture(t)
	r := generated.CronRun{UID: "old-running", Status: "running", IssueUID: new("history-issue")}
	m.cron.data.lookup = map[string]generated.CronRun{r.UID: r}
	m.cron.jobPage.runs = []generated.CronRun{r}
	a := waitReadAPI(func(context.Context, string) ([]generated.CronRun, error) { return nil, nil })
	// fetchCron also reads jobs/workflows/head, so delegate those to the fixture API.
	m.api = &cronWaitFetchAPI{cronReadTestAPI: &cronReadTestAPI{data: *m.cron.data}, waiter: a}
	msg := m.fetchCron()().(cronLoadedMsg)
	require.Contains(t, msg.waits, "history-issue")
}

type cronWaitFetchAPI struct {
	*cronReadTestAPI
	waiter *cronWaitReadAPI
}

func (a *cronWaitFetchAPI) readCronParent(ctx context.Context, id int64, uid string) (cronParent, error) {
	return a.waiter.readCronParent(ctx, id, uid)
}

// A fresh unavailable read cannot reuse a preceding successful wait snapshot.
func TestCronWaitRefreshUnavailable(t *testing.T) {
	uid := "issue-example"
	r := generated.CronRun{UID: "run-example", Status: "running", Actor: "operator", IssueUID: new(uid)}
	metadata := map[string]any{"work.attention": "stuck", notification.MetadataKey("operator"): map[string]any{"from": "worker", "message": "review"}}
	p := cronData{runs: []generated.CronRun{r}, waits: map[string]cronParent{uid: {open: true, metadata: metadata}}}
	require.Equal(t, "blocked", cronRunHealth(p, r).state)
	a := waitReadAPI(nil)
	a.wait = func(context.Context, string) (cronParent, error) { return cronParent{}, errors.New("offline") }
	cronEnrich(t.Context(), a, 7, &p)
	got := cronRunHealth(p, r)
	require.Equal(t, "running", got.state)
	require.Equal(t, "attention unavailable", got.reason)
}

// Production installs undoClient around the daemon API; shared issue reads
// must preserve attention evidence through that wrapper.
func TestCronWaitSharedIssueThroughUndoClient(t *testing.T) {
	j := cronFixture(t).cron.data.jobs[0]
	nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))})(&j.Definition)
	p := cronData{jobs: []generated.CronJob{j}, runs: []generated.CronRun{{UID: "adapter-run", Status: "running", Actor: "operator", IssueUID: new(nextParentUID)}}}
	var reads atomic.Int32
	requests := make(chan string, 4)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		if r.URL.Path == "/api/v1/projects/7/issues/"+nextParentUID {
			reads.Add(1)
			respondJSON(t, w, map[string]any{"issue": map[string]any{"status": "open", "metadata": map[string]any{"work.attention": "needs-human"}}})
		} else {
			respondJSON(t, w, map[string]any{"runs": []any{}})
		}
	})
	cronEnrich(t.Context(), newUndoClient(c), 7, &p)
	close(requests)
	var paths []string
	for path := range requests {
		paths = append(paths, path)
	}
	require.ElementsMatch(t, []string{"/api/v1/projects/7/issues/" + nextParentUID, "/api/v1/projects/7/cron/runs"}, paths)
	require.Equal(t, int32(1), reads.Load())
	require.True(t, p.parentOpen[nextParentUID])
	require.Equal(t, "blocked", cronRunHealth(p, p.runs[0]).state)

}

// Contract: at most 20 attention reads, one fetch per shared issue, with each
// actor evaluated independently and excess/missing evidence explicitly unknown.
func TestCronWaitUniqueAdmissionProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers(0, 60))
		pool := hegel.Draw(ht, hegel.Integers(1, 31))
		p := cronData{}
		nativeCount := hegel.Draw(ht, hegel.Integers[int](0, pool))
		for i := range nativeCount {
			j := cronFixture(t).cron.data.jobs[0]
			j.UID = fmt.Sprintf("native-%d", i)
			nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(3600))})(&j.Definition)
			j.Definition.Issue.ParentUID = new(fmt.Sprint(i))
			p.runs = append(p.runs, generated.CronRun{UID: "last-" + j.UID, JobUID: new(j.UID), Status: "succeeded"})
			p.jobs = append(p.jobs, j)
		}
		unique := map[string]bool{}
		for i := range count {
			issue := fmt.Sprint(i % pool)
			r := generated.CronRun{UID: fmt.Sprint(i), Status: "running", IssueUID: new(issue), Actor: "operator"}
			if i%2 == 0 {
				p.runs = append(p.runs, r)
			} else {
				p.historyRuns = append(p.historyRuns, r)
			}
			unique[issue] = true
		}
		a := waitReadAPI(nil)
		cronEnrich(t.Context(), a, 7, &p)
		shared := 0
		for i := range nativeCount {
			if unique[fmt.Sprint(i)] {
				shared++
			}
		}
		if len(a.waitCalls) != nativeCount+min(20, len(unique)-shared) {
			ht.Fatalf("count %d pool %d: fetched %v", count, pool, a.waitCalls)
		}
		for uid, n := range a.waitCalls {
			if p.waitErr[uid] != nil {
				ht.Fatalf("fetched issue %q has cap error: %v", uid, p.waitErr[uid])
			}
			if n != 1 {
				ht.Fatalf("issue %q fetched %d times", uid, n)
			}
		}
	}, hegel.WithTestCases(150))

	t.Run("shared actor and overflow outcomes", func(t *testing.T) {
		p := cronData{}
		for i := range 26 {
			p.runs = append(p.runs, generated.CronRun{UID: fmt.Sprint(i), Status: "running", Actor: "operator", IssueUID: new(fmt.Sprint(i)), CreatedAt: snapshotFixedNow.Add(-time.Duration(i) * time.Minute)})
		}
		p.runs = append(p.runs, generated.CronRun{UID: "other-actor", Status: "running", Actor: "other", IssueUID: new("0")})
		a := waitReadAPI(nil)
		cronEnrich(t.Context(), a, 7, &p)
		require.Len(t, a.waitCalls, 20)
		require.Equal(t, 1, a.waitCalls["0"], "two actors sharing an issue must not issue a second read")
		require.Equal(t, "blocked", cronRunHealth(p, p.runs[0]).state)
		require.Equal(t, "running", cronRunHealth(p, p.runs[26]).state)
		require.Contains(t, cronRunHealth(p, p.runs[25]).reason, "attention unavailable")
	})
}
