package tui

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

// Contract: at most 20 distinct issues, one fetch per shared issue, with each
// actor evaluated independently and excess/missing evidence explicitly unknown.
func TestCronWaitReadCapAndSharedIssue(t *testing.T) {
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

func TestCronWaitReadsShareWorkersAndDeadline(t *testing.T) {
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

// Contract: head and retained rows share a unique-issue admission cap, and a
// fresh unavailable read cannot reuse a preceding successful wait snapshot.
func TestCronWaitUniqueAdmissionProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		count := int(hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0))) % 61)
		pool := 1 + int(hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0)))%31)
		p := cronData{}
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
		if len(a.waitCalls) != min(20, len(unique)) {
			ht.Fatalf("count %d pool %d: fetched %v", count, pool, a.waitCalls)
		}
		for uid, n := range a.waitCalls {
			if n != 1 {
				ht.Fatalf("issue %q fetched %d times", uid, n)
			}
		}
	}, hegel.WithTestCases(150))
}

func TestCronWaitRefreshUnavailableProperty(t *testing.T) {
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
