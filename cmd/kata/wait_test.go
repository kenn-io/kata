package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

// --- pure condition-evaluation unit tests -------------------------------

func TestWaitEvalCondition(t *testing.T) {
	cases := []struct {
		name       string
		mode       waitMode
		st         issueState
		wantSat    bool
		wantReason string
	}{
		{"closed-mode-open-pending", waitClosed, issueState{status: "open"}, false, ""},
		{"closed-mode-closed", waitClosed, issueState{status: "closed"}, true, "closed"},
		{"needs-human-open-no-attention", waitNeedsHuman, issueState{status: "open"}, false, ""},
		{"needs-human-open-ok", waitNeedsHuman, issueState{status: "open", attention: "ok"}, false, ""},
		{"needs-human-match", waitNeedsHuman, issueState{status: "open", attention: "needs-human"}, true, "attention"},
		{"needs-human-stuck-no-match", waitNeedsHuman, issueState{status: "open", attention: "stuck"}, false, ""},
		{"stuck-match", waitStuck, issueState{status: "open", attention: "stuck"}, true, "attention"},
		{"stuck-needs-human-no-match", waitStuck, issueState{status: "open", attention: "needs-human"}, false, ""},
		{"attention-ok-no-match", waitAttention, issueState{status: "open", attention: "ok"}, false, ""},
		{"attention-empty-no-match", waitAttention, issueState{status: "open", attention: ""}, false, ""},
		{"attention-needs-human", waitAttention, issueState{status: "open", attention: "needs-human"}, true, "attention"},
		{"attention-stuck", waitAttention, issueState{status: "open", attention: "stuck"}, true, "attention"},
		{"attention-novel-level", waitAttention, issueState{status: "open", attention: "on-fire"}, true, "attention"},
		{"needs-human-but-closed", waitNeedsHuman, issueState{status: "closed"}, true, "closed"},
		{"attention-but-closed", waitAttention, issueState{status: "closed", attention: "ok"}, true, "closed"},
		{"stuck-but-closed", waitStuck, issueState{status: "closed", attention: "stuck"}, true, "closed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sat, reason := evalWait(c.mode, c.st)
			assert.Equal(t, c.wantSat, sat)
			assert.Equal(t, c.wantReason, reason)
		})
	}
}

func TestWaitClassifyFetchErr(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		wantPermanent bool
	}{
		{
			name:          "not-found-4xx-is-permanent",
			err:           apiErrFromBody(http.StatusNotFound, []byte(`{"error":{"code":"issue_not_found","message":"issue not found"}}`)),
			wantPermanent: true,
		},
		{
			name:          "bad-request-4xx-is-permanent",
			err:           apiErrFromBody(http.StatusBadRequest, []byte(`{"error":{"code":"validation","message":"bad ref"}}`)),
			wantPermanent: true,
		},
		{
			name:          "server-5xx-is-transient",
			err:           apiErrFromBody(http.StatusInternalServerError, []byte(`{"error":{"code":"internal","message":"boom"}}`)),
			wantPermanent: false,
		},
		{
			name:          "transport-error-is-transient",
			err:           errors.New("dial tcp 127.0.0.1:7777: connect: connection refused"),
			wantPermanent: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.wantPermanent, classifyFetchErr(c.err))
		})
	}
}

func TestWaitParseModeRejectsUnknown(t *testing.T) {
	_, err := parseWaitMode("banana")
	require.Error(t, err)
	_ = requireCLIError(t, err, ExitValidation)

	for _, ok := range []string{"closed", "attention", "needs-human", "stuck"} {
		m, err := parseWaitMode(ok)
		require.NoError(t, err)
		assert.Equal(t, waitMode(ok), m)
	}
}

// --- integration tests --------------------------------------------------

const (
	waitFastPoll  = "25ms"
	waitSafetyNet = "5s"
	waitMutDelay  = 120 * time.Millisecond
)

func TestWaitAlreadyClosedReturnsImmediately(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "finished work")
	require.NoError(t, closeIssueHTTP(env, pid, ref))

	start := time.Now()
	stdout, _, err := runCLIWithErr(t, env, dir,
		"wait", ref, "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 2*time.Second, "already-closed should not enter the poll loop")
	assert.Contains(t, stdout, ref)
	assert.Contains(t, stdout, "closed")
}

func TestWaitClosesAfterDelay(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "will close soon")

	errc := make(chan error, 1)
	go func() {
		time.Sleep(waitMutDelay)
		errc <- closeIssueHTTP(env, pid, ref)
	}()

	stdout, _, err := runCLIWithErr(t, env, dir,
		"wait", ref, "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, <-errc)
	require.NoError(t, err)
	assert.Contains(t, stdout, ref)
	assert.Contains(t, stdout, "closed")
}

func TestWaitUntilNeedsHumanSurfacesMessage(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "needs a human")

	errc := make(chan error, 1)
	go func() {
		time.Sleep(waitMutDelay)
		errc <- setAttentionHTTP(env, pid, ref, "needs-human", "blocked on database migration")
	}()

	stdout, _, err := runCLIWithErr(t, env, dir,
		"wait", ref, "--until", "needs-human", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, <-errc)
	require.NoError(t, err)
	assert.Contains(t, stdout, "needs-human")
	assert.Contains(t, stdout, "blocked on database migration")
}

func TestWaitUntilAttentionFiresOnStuck(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "stuck work")

	errc := make(chan error, 1)
	go func() {
		time.Sleep(waitMutDelay)
		errc <- setAttentionHTTP(env, pid, ref, "stuck", "")
	}()

	stdout, _, err := runCLIWithErr(t, env, dir,
		"wait", ref, "--until", "attention", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, <-errc)
	require.NoError(t, err)
	assert.Contains(t, stdout, "stuck")
}

func TestWaitUntilAttentionFiresOnNovelLevel(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "novel level")

	errc := make(chan error, 1)
	go func() {
		time.Sleep(waitMutDelay)
		errc <- setAttentionHTTP(env, pid, ref, "on-fire", "")
	}()

	stdout, _, err := runCLIWithErr(t, env, dir,
		"wait", ref, "--until", "attention", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, <-errc)
	require.NoError(t, err)
	assert.Contains(t, stdout, "on-fire")
}

func TestWaitAttentionModeCompletesOnClose(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "closes instead of flagging")

	errc := make(chan error, 1)
	go func() {
		time.Sleep(waitMutDelay)
		errc <- closeIssueHTTP(env, pid, ref)
	}()

	stdout, _, err := runCLIWithErr(t, env, dir,
		"wait", ref, "--until", "needs-human", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, <-errc)
	require.NoError(t, err)
	assert.Contains(t, stdout, "closed")
}

func TestWaitAnyReturnsOnFirst(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref1 := createIssue(t, env, pid, "first ref")
	ref2 := createIssue(t, env, pid, "second ref never closes")

	errc := make(chan error, 1)
	go func() {
		time.Sleep(waitMutDelay)
		errc <- closeIssueHTTP(env, pid, ref1)
	}()

	// --timeout is a safety net; if --any did not short-circuit, ref2 never
	// closes and the command would time out (nonzero exit) instead.
	stdout, _, err := runCLIWithErr(t, env, dir,
		"wait", ref1, ref2, "--any", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, <-errc)
	require.NoError(t, err)
	assert.Contains(t, stdout, ref1)
}

func TestWaitAllWaitsForEvery(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref1 := createIssue(t, env, pid, "first of two")
	ref2 := createIssue(t, env, pid, "second of two")

	errc := make(chan error, 1)
	go func() {
		e1 := (error)(nil)
		time.Sleep(waitMutDelay)
		e1 = closeIssueHTTP(env, pid, ref1)
		time.Sleep(waitMutDelay)
		e2 := closeIssueHTTP(env, pid, ref2)
		errc <- errors.Join(e1, e2)
	}()

	stdout, _, err := runCLIWithErr(t, env, dir,
		"wait", ref1, ref2, "--all", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, <-errc)
	require.NoError(t, err)
	assert.Contains(t, stdout, ref1)
	assert.Contains(t, stdout, ref2)
}

func TestWaitTimeoutReportsPendingAndExitCode(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "never changes")

	_, stderr, err := runCLIWithErr(t, env, dir,
		"wait", ref, "--poll-interval", waitFastPoll, "--timeout", "200ms")
	_ = requireCLIError(t, err, ExitWaitTimeout)
	assert.Contains(t, stderr, "pending")
	assert.Contains(t, stderr, ref)
}

func TestWaitTimeoutJSONEmitsObject(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "never changes json")

	stdout, _, err := runCLIWithErr(t, env, dir,
		"--json", "wait", ref, "--poll-interval", waitFastPoll, "--timeout", "200ms")
	_ = requireCLIError(t, err, ExitWaitTimeout)

	obj := parseWaitJSON(t, stdout)
	assert.True(t, obj.TimedOut)
	assert.Contains(t, obj.Pending, ref)
	assert.Empty(t, obj.Results)
}

func TestWaitUsageAnyAllConflict(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "conflict")

	_, err := runCLICapture(t, env, dir, "wait", ref, "--any", "--all")
	_ = requireCLIError(t, err, ExitUsage)
}

func TestWaitUsageZeroPollInterval(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "bad poll")

	_, err := runCLICapture(t, env, dir, "wait", ref, "--poll-interval", "0s")
	_ = requireCLIError(t, err, ExitValidation)
}

// TestWaitUsageNegativeTimeout: a negative --timeout must be rejected, not
// silently treated like 0 (wait forever) because the poll loop only arms a
// deadline when timeout > 0. A typo like --timeout=-1s must fail fast.
func TestWaitUsageNegativeTimeout(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "bad timeout")

	_, err := runCLICapture(t, env, dir, "wait", ref, "--timeout", "-1s")
	_ = requireCLIError(t, err, ExitValidation)
}

// TestWaitUsageBareTimeoutSuggestsUnit: "--timeout 1800" (no unit) is the
// first mistake real agents make (kenn-io/kata#159). It must fail at flag
// parsing with an error suggesting the "1800s" spelling, not Go's stock
// parse failure.
func TestWaitUsageBareTimeoutSuggestsUnit(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "bare timeout")

	_, err := runCLICapture(t, env, dir, "wait", ref, "--timeout", "1800")
	if err == nil {
		t.Fatal("bare --timeout 1800: want error, got nil")
	}
	if !strings.Contains(err.Error(), `"1800s"`) {
		t.Fatalf("bare --timeout error %q does not suggest \"1800s\"", err)
	}
}

// TestWaitTimeoutBoundsHungFetch: a daemon state fetch that never returns must
// be bounded by --timeout. Ref resolution answers immediately, but the issue
// GET hangs; the command must still return an ExitWaitTimeout well before the
// server's hard cap rather than blocking on the stuck request.
func TestWaitTimeoutBoundsHungFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/projects/resolve":
			_, _ = w.Write([]byte(`{"project":{"id":1,"name":"kata"}}`))
		case strings.HasPrefix(r.URL.Path, "/api/v1/projects/1/issues/"):
			// Hang until the client cancels (its deadline) or a hard cap that
			// only trips if the fetch was never bounded.
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	resetFlags(t)
	start := time.Now()
	_, _, err := executeRootCapture(t, contextWithBaseURL(context.Background(), srv.URL),
		"wait", "kata#abcd", "--timeout", "300ms", "--poll-interval", "50ms")
	elapsed := time.Since(start)

	_ = requireCLIError(t, err, ExitWaitTimeout)
	assert.Less(t, elapsed, 2*time.Second,
		"a hung daemon fetch must be bounded by --timeout, not block on the stuck request")
}

// TestWaitTimeoutBoundsRefResolution: --timeout is the whole command's
// wall-clock budget, not only the post-resolution poll loop. If project/ref
// resolution stalls, the command must return ExitWaitTimeout promptly instead
// of waiting for the default per-request HTTP timeout.
func TestWaitTimeoutBoundsRefResolution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/resolve":
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
				_, _ = w.Write([]byte(`{"project":{"id":1,"name":"kata"}}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	resetFlags(t)
	start := time.Now()
	_, _, err := executeRootCapture(t, contextWithBaseURL(context.Background(), srv.URL),
		"wait", "kata#abcd", "--timeout", "300ms", "--poll-interval", "50ms")
	elapsed := time.Since(start)

	_ = requireCLIError(t, err, ExitWaitTimeout)
	assert.Less(t, elapsed, 2*time.Second,
		"a stalled ref resolution must be bounded by --timeout, not the default HTTP timeout")
}

// TestWaitTimeoutAfterTransientFailsReportsTimeout: a fetch cut short by the
// --timeout deadline must not be counted toward the consecutive-failure budget.
// With two prior transient (5xx) failures, a third deadline-canceled fetch must
// still surface as ExitWaitTimeout, not ExitInternal.
func TestWaitTimeoutAfterTransientFailsReportsTimeout(t *testing.T) {
	var issueGets int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/projects/resolve":
			_, _ = w.Write([]byte(`{"project":{"id":1,"name":"kata"}}`))
		case strings.HasPrefix(r.URL.Path, "/api/v1/projects/1/issues/"):
			mu.Lock()
			issueGets++
			n := issueGets
			mu.Unlock()
			if n <= 2 {
				// Two transient failures prime the consecutive-fail counter.
				http.Error(w, "transient", http.StatusInternalServerError)
				return
			}
			// The next fetch hangs until the wait deadline cancels it.
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	resetFlags(t)
	_, _, err := executeRootCapture(t, contextWithBaseURL(context.Background(), srv.URL),
		"wait", "kata#abcd", "--timeout", "500ms", "--poll-interval", "20ms")

	// A deadline-canceled fetch is a timeout, not the 3rd consecutive failure.
	_ = requireCLIError(t, err, ExitWaitTimeout)
}

// TestWaitParentDeadlineDuringResolutionIsNotWaitTimeout: when the command runs
// under a parent context whose deadline fires before --timeout (e.g. a caller
// using ExecuteContext with its own budget), a ref resolution cut short by that
// parent deadline must surface as the resolution error, not be misclassified as
// ExitWaitTimeout with timed_out=true — the wait command's own timeout budget
// was never exhausted.
func TestWaitParentDeadlineDuringResolutionIsNotWaitTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/resolve":
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
				_, _ = w.Write([]byte(`{"project":{"id":1,"name":"kata"}}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	resetFlags(t)
	parentCtx, cancel := context.WithTimeout(
		contextWithBaseURL(context.Background(), srv.URL), 200*time.Millisecond)
	defer cancel()
	_, _, err := executeRootCapture(t, parentCtx,
		"wait", "kata#abcd", "--timeout", "5s", "--poll-interval", "50ms")

	require.Error(t, err)
	var ce *cliError
	if errors.As(err, &ce) {
		assert.NotEqual(t, ExitWaitTimeout, ce.ExitCode,
			"a parent-context deadline during resolution must not be reported as the wait's own --timeout")
	}
}

// TestWaitAnyInitialPassStopsAfterJoinMet: in --any the initial pass must stop
// fetching once a ref already satisfies the join. Otherwise a later stalled
// fetch (with no --timeout to bound it) hangs even though the wait is already
// met.
func TestWaitAnyInitialPassStopsAfterJoinMet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/projects/resolve":
			_, _ = w.Write([]byte(`{"project":{"id":1,"name":"kata"}}`))
		case strings.HasSuffix(r.URL.Path, "/issues/aaaa"):
			_, _ = w.Write([]byte(`{"issue":{"short_id":"aaaa","status":"closed","metadata":{},"revision":1}}`))
		case strings.HasSuffix(r.URL.Path, "/issues/bbbb"):
			// A stalled later ref: it must never be fetched once aaaa has
			// already satisfied the --any join.
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			_, _ = w.Write([]byte(`{"issue":{"short_id":"bbbb","status":"open","metadata":{},"revision":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	resetFlags(t)
	start := time.Now()
	stdout, _, err := executeRootCapture(t, contextWithBaseURL(context.Background(), srv.URL),
		"wait", "kata#aaaa", "kata#bbbb", "--any", "--poll-interval", "50ms")
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Contains(t, stdout, "aaaa")
	assert.Less(t, elapsed, 2*time.Second,
		"--any must stop the initial pass once the join is met, not block on a stalled later ref")
}

func TestWaitBadRefFailsFast(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)

	start := time.Now()
	_, err := runCLICapture(t, env, dir,
		"wait", "zzzz", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.Error(t, err)
	_ = requireCLIError(t, err, ExitNotFound)
	assert.Less(t, time.Since(start), 2*time.Second, "bad ref must fail before entering the poll loop")
}

func TestWaitAnySucceedsWhenOtherRefAlreadySatisfiedBadRefLast(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "already closed, bad ref last")
	require.NoError(t, closeIssueHTTP(env, pid, ref))

	start := time.Now()
	stdout, _, err := runCLIWithErr(t, env, dir,
		"wait", ref, "zzzz", "--any", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 2*time.Second, "already-satisfied --any join should not enter the poll loop")
	assert.Contains(t, stdout, ref)
	assert.Contains(t, stdout, "closed")
}

func TestWaitAnySucceedsWhenOtherRefAlreadySatisfiedBadRefFirst(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "already closed, bad ref first")
	require.NoError(t, closeIssueHTTP(env, pid, ref))

	start := time.Now()
	stdout, _, err := runCLIWithErr(t, env, dir,
		"wait", "zzzz", ref, "--any", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 2*time.Second, "already-satisfied --any join should not enter the poll loop")
	assert.Contains(t, stdout, ref)
	assert.Contains(t, stdout, "closed")
}

func TestWaitAnyStillFailsFastOnBadRefWhenJoinUnsatisfied(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "still open, never closes")

	start := time.Now()
	_, err := runCLICapture(t, env, dir,
		"wait", ref, "zzzz", "--any", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.Error(t, err)
	_ = requireCLIError(t, err, ExitNotFound)
	assert.Less(t, time.Since(start), 2*time.Second, "bad ref must fail before entering the poll loop")
}

func TestWaitAllStillFailsOnBadRefEvenWhenOtherRefAlreadySatisfied(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "already closed, but --all with bad ref")
	require.NoError(t, closeIssueHTTP(env, pid, ref))

	_, err := runCLICapture(t, env, dir,
		"wait", ref, "zzzz", "--all", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.Error(t, err)
	_ = requireCLIError(t, err, ExitNotFound)
}

// TestWaitAllAbortsOnDeletedRefMidWait: a ref deleted while --all is polling
// is a permanent 404, so the wait aborts promptly with the daemon's not-found
// exit code (4) rather than treating it as a transient blip and eventually
// failing with a generic internal error.
func TestWaitAllAbortsOnDeletedRefMidWait(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "deleted mid-wait")

	errc := make(chan error, 1)
	go func() {
		time.Sleep(waitMutDelay)
		errc <- deleteIssueHTTP(env, pid, ref)
	}()

	start := time.Now()
	_, err := runCLICapture(t, env, dir,
		"wait", ref, "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, <-errc)
	_ = requireCLIError(t, err, ExitNotFound)
	assert.Less(t, time.Since(start), 3*time.Second,
		"a permanent 404 should abort promptly, not burn the whole timeout")
}

// TestWaitAnyAbandonsDeletedRefCompletesOnOther: in --any mode a ref that is
// deleted mid-wait is abandoned (reported, no longer polled) while the wait
// keeps running and completes on the second ref that closes later.
func TestWaitAnyAbandonsDeletedRefCompletesOnOther(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref1 := createIssue(t, env, pid, "abandoned ref")
	ref2 := createIssue(t, env, pid, "closes later")

	errc := make(chan error, 2)
	go func() {
		time.Sleep(waitMutDelay)
		errc <- deleteIssueHTTP(env, pid, ref1)
		time.Sleep(waitMutDelay)
		errc <- closeIssueHTTP(env, pid, ref2)
	}()

	stdout, _, err := runCLIWithErr(t, env, dir,
		"wait", ref1, ref2, "--any", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, <-errc)
	require.NoError(t, <-errc)
	require.NoError(t, err)
	assert.Contains(t, stdout, ref2)
	assert.Contains(t, stdout, "closed")
	// The abandoned ref is surfaced on its own line noting the error.
	assert.Contains(t, stdout, ref1)
	assert.Contains(t, stdout, "error")
}

// TestWaitAnyAbandonedAgentUsesNonErrorRow: in --agent --any mode a ref
// abandoned mid-wait must not emit an ERR line (the agent contract reserves
// ERR for command failure) when the overall command later succeeds on another
// ref. The abandoned ref is surfaced as a non-error row instead.
func TestWaitAnyAbandonedAgentUsesNonErrorRow(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref1 := createIssue(t, env, pid, "abandoned agent ref")
	ref2 := createIssue(t, env, pid, "closes later agent")

	errc := make(chan error, 2)
	go func() {
		time.Sleep(waitMutDelay)
		errc <- deleteIssueHTTP(env, pid, ref1)
		time.Sleep(waitMutDelay)
		errc <- closeIssueHTTP(env, pid, ref2)
	}()

	stdout, _, err := runCLIWithErr(t, env, dir,
		"--agent", "wait", ref1, ref2, "--any", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, <-errc)
	require.NoError(t, <-errc)
	require.NoError(t, err)

	// Command succeeded, so no streamed line may carry the ERR failure prefix.
	assert.NotContains(t, stdout, "ERR ")
	// The completed ref is a normal OK completion row.
	assert.Contains(t, stdout, "reason=closed")
	assert.Contains(t, stdout, ref2)
	// The abandoned ref is surfaced as a non-error row keyed by reason.
	assert.Contains(t, stdout, "reason=abandoned")
	assert.Contains(t, stdout, ref1)
}

// TestWaitAnyAbandonedRefAppearsInJSON: the --json payload carries an
// abandoned ref in its own per-ref list (not in results or pending).
func TestWaitAnyAbandonedRefAppearsInJSON(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref1 := createIssue(t, env, pid, "abandoned json ref")
	ref2 := createIssue(t, env, pid, "closes later json")

	errc := make(chan error, 2)
	go func() {
		time.Sleep(waitMutDelay)
		errc <- deleteIssueHTTP(env, pid, ref1)
		time.Sleep(waitMutDelay)
		errc <- closeIssueHTTP(env, pid, ref2)
	}()

	stdout, _, err := runCLIWithErr(t, env, dir,
		"--json", "wait", ref1, ref2, "--any", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, <-errc)
	require.NoError(t, <-errc)
	require.NoError(t, err)

	obj := parseWaitJSON(t, stdout)
	assert.False(t, obj.TimedOut)
	assert.NotContains(t, obj.Pending, ref1, "abandoned ref must not be reported as pending")
	require.Len(t, obj.Results, 1)
	assert.Equal(t, ref2, obj.Results[0].Ref)
	require.Len(t, obj.Abandoned, 1)
	assert.Equal(t, ref1, obj.Abandoned[0].Ref)
	assert.Equal(t, "error", obj.Abandoned[0].Reason)
}

// TestWaitAnyJSONSuccessLeavesPendingEmpty: a successful --any --json wait
// must not report the un-needed second ref in `pending`. `pending` is
// documented as timeout-only (refs still unmet when the wait times out); on a
// successful join it must be empty so the result does not look incomplete.
func TestWaitAnyJSONSuccessLeavesPendingEmpty(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref1 := createIssue(t, env, pid, "closes first json any")
	ref2 := createIssue(t, env, pid, "never satisfied json any")

	errc := make(chan error, 1)
	go func() {
		time.Sleep(waitMutDelay)
		errc <- closeIssueHTTP(env, pid, ref1)
	}()

	stdout, _, err := runCLIWithErr(t, env, dir,
		"--json", "wait", ref1, ref2, "--any", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, <-errc)
	require.NoError(t, err)

	obj := parseWaitJSON(t, stdout)
	assert.False(t, obj.TimedOut)
	require.Len(t, obj.Results, 1)
	assert.Equal(t, ref1, obj.Results[0].Ref)
	assert.Empty(t, obj.Pending,
		"a successful --any wait must not list the un-needed ref as pending")
}

// TestWaitAnyAllRefsDeletedReturnsFirstError: when every ref is deleted
// mid-wait in --any mode, the wait returns the first permanent error rather
// than spinning forever.
func TestWaitAnyAllRefsDeletedReturnsFirstError(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref1 := createIssue(t, env, pid, "both deleted 1")
	ref2 := createIssue(t, env, pid, "both deleted 2")

	errc := make(chan error, 2)
	go func() {
		time.Sleep(waitMutDelay)
		errc <- deleteIssueHTTP(env, pid, ref1)
		errc <- deleteIssueHTTP(env, pid, ref2)
	}()

	_, err := runCLICapture(t, env, dir,
		"wait", ref1, ref2, "--any", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, <-errc)
	require.NoError(t, <-errc)
	_ = requireCLIError(t, err, ExitNotFound)
}

func TestWaitAgentOutput(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "agent output")
	require.NoError(t, closeIssueHTTP(env, pid, ref))

	stdout, _, err := runCLIWithErr(t, env, dir,
		"--agent", "wait", ref, "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, err)
	assert.Contains(t, stdout, "OK wait")
	assert.Contains(t, stdout, ref)
	assert.Contains(t, stdout, "reason=closed")
}

func TestWaitJSONOutput(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "json output")
	require.NoError(t, setAttentionHTTP(env, pid, ref, "needs-human", "please look"))

	stdout, _, err := runCLIWithErr(t, env, dir,
		"--json", "wait", ref, "--until", "needs-human", "--poll-interval", waitFastPoll, "--timeout", waitSafetyNet)
	require.NoError(t, err)

	obj := parseWaitJSON(t, stdout)
	assert.False(t, obj.TimedOut)
	assert.Empty(t, obj.Pending)
	require.Len(t, obj.Results, 1)
	assert.Equal(t, ref, obj.Results[0].Ref)
	assert.Equal(t, "attention", obj.Results[0].Reason)
	assert.Equal(t, "needs-human", obj.Results[0].Attention)
	assert.Equal(t, "please look", obj.Results[0].AttentionMsg)
	assert.GreaterOrEqual(t, obj.Results[0].WaitedMs, int64(0))
}

// --- test-local HTTP seeding helpers (goroutine-safe: no *testing.T) ----

// parseWaitJSON decodes the --json wait payload from stdout.
func parseWaitJSON(t *testing.T, stdout string) waitJSONOutput {
	t.Helper()
	var obj waitJSONOutput
	require.NoErrorf(t, json.Unmarshal([]byte(strings.TrimSpace(stdout)), &obj),
		"wait --json output must be parseable; got %q", stdout)
	return obj
}

// closeIssueHTTP closes an issue via the daemon's close action using the TUI
// bypass (reason=done, no evidence). Returns an error instead of failing a
// *testing.T so it is safe to call from a background goroutine.
func closeIssueHTTP(env *testenv.Env, pid int64, ref string) error {
	return postJSONExpectOK(
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/close", env.URL, pid, ref),
		map[string]any{"actor": "tester", "source": "tui", "reason": "done"}, "")
}

// deleteIssueHTTP soft-deletes an issue via the daemon's delete action,
// supplying the X-Kata-Confirm header the daemon requires. The workspace binds
// the project name "kata", so the confirm value is "DELETE kata#<short_id>".
// After this returns, a GET on the ref yields a permanent 404. Goroutine-safe
// (returns error, no *testing.T).
func deleteIssueHTTP(env *testenv.Env, pid int64, ref string) error {
	bs, err := json.Marshal(map[string]any{"actor": "tester"})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/delete", env.URL, pid, ref),
		bytes.NewReader(bs))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kata-Confirm", "DELETE kata#"+ref)
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // test-only loopback
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("DELETE %s: %d %s", ref, resp.StatusCode, body)
	}
	return nil
}

// setAttentionHTTP patches the work.attention (and optional work.attention_msg)
// metadata keys. It first reads the current revision for the required If-Match
// precondition. Goroutine-safe (returns error, no *testing.T).
func setAttentionHTTP(env *testenv.Env, pid int64, ref, value, msg string) error {
	rev, err := issueRevisionHTTP(env, pid, ref)
	if err != nil {
		return err
	}
	patch := map[string]any{"work.attention": value}
	if msg != "" {
		patch["work.attention_msg"] = msg
	}
	return postJSONExpectOK(
		fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/metadata", env.URL, pid, ref),
		map[string]any{"actor": "tester", "patch": patch},
		fmt.Sprintf(`"rev-%d"`, rev))
}

// issueRevisionHTTP GETs an issue and returns its current revision.
func issueRevisionHTTP(env *testenv.Env, pid int64, ref string) (int64, error) {
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", env.URL, pid, ref)) //nolint:noctx,gosec // test-only loopback
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("GET issue: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Issue struct {
			Revision int64 `json:"revision"`
		} `json:"issue"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.Issue.Revision, nil
}

// postJSONExpectOK POSTs body as JSON, optionally with an If-Match header, and
// returns an error unless the daemon answers 200.
func postJSONExpectOK(url string, body any, ifMatch string) error {
	bs, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(bs))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // test-only loopback
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s: %d %s", url, resp.StatusCode, respBody)
	}
	return nil
}
