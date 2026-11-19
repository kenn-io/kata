package daemon_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type longPollFixture struct {
	env        *testenv.Env
	project    db.Project
	enrollment db.CreatedFederationEnrollment
}

func newLongPollFixture(t *testing.T) longPollFixture {
	t.Helper()
	env := testenv.New(t)
	project := createFederatedHubProject(t, env, "hub-project")
	enrollment, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:            "long-poll-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "pull",
		Actor:            "tester",
	})
	require.NoError(t, err)
	return longPollFixture{env: env, project: project, enrollment: enrollment}
}

type longPollResult struct {
	// err is set when the request itself failed; the other fields are then
	// empty.
	err     error
	status  int
	header  http.Header
	body    pollBodyForTest
	raw     []byte
	elapsed time.Duration
}

type pollBodyForTest struct {
	Events []struct {
		EventID int64  `json:"event_id"`
		Type    string `json:"type"`
	} `json:"events"`
	NextAfterID   int64 `json:"next_after_id"`
	ResetRequired bool  `json:"reset_required"`
}

func (f longPollFixture) poll(t *testing.T, afterID int64, wait string) longPollResult {
	t.Helper()
	path := fmt.Sprintf("%s/federation/events?after_id=%d", projectPath(f.project.ID), afterID)
	if wait != "" {
		path += "&wait=" + wait
	}
	started := time.Now()
	resp, raw := envDoRaw(t, f.env, http.MethodGet, path, nil, bearer("long-poll-token"))
	return longPollResultFrom(t, resp, raw, time.Since(started))
}

func decodeLongPollResponse(t testing.TB, resp *http.Response, elapsed time.Duration) longPollResult {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return longPollResultFrom(t, resp, raw, elapsed)
}

func longPollResultFrom(t testing.TB, resp *http.Response, raw []byte, elapsed time.Duration) longPollResult {
	t.Helper()
	out := longPollResult{status: resp.StatusCode, header: resp.Header, raw: raw, elapsed: elapsed}
	if resp.StatusCode == http.StatusOK {
		require.NoError(t, json.Unmarshal(raw, &out.body), "%s", raw)
	}
	return out
}

func (f longPollFixture) cursor(t *testing.T) int64 {
	t.Helper()
	result := f.poll(t, 0, "")
	require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
	return result.body.NextAfterID
}

// startLongPoll sends a poll from its own goroutine. It reports a failed
// request through the result, because require must stay on the test
// goroutine.
func startLongPoll(t *testing.T, f longPollFixture, afterID int64, wait string) <-chan longPollResult {
	t.Helper()
	done := make(chan longPollResult, 1)
	url := fmt.Sprintf("%s%s/federation/events?after_id=%d&wait=%s", f.env.URL, projectPath(f.project.ID), afterID, wait)
	go func() {
		done <- sendLongPoll(t.Context(), f.env.HTTP, url)
	}()
	return done
}

func sendLongPoll(ctx context.Context, client *http.Client, url string) longPollResult {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return longPollResult{err: err}
	}
	request.Header.Set("Authorization", "Bearer long-poll-token")
	started := time.Now()
	response, err := client.Do(request) //nolint:gosec // test-owned loopback daemon
	if err != nil {
		return longPollResult{err: err}
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	out := longPollResult{err: err, status: response.StatusCode, header: response.Header, raw: raw, elapsed: time.Since(started)}
	if err == nil && response.StatusCode == http.StatusOK {
		out.err = json.Unmarshal(raw, &out.body)
	}
	return out
}

// waitForHeldPoll gives the poll goroutine time to reach the hub and park.
// A commit that lands before the poll's first read is still returned at once,
// so this only keeps the test about the held path.
func waitForHeldPoll(t testing.TB, done <-chan longPollResult) {
	t.Helper()
	select {
	case result := <-done:
		t.Fatalf("poll returned before it was held: %d %s", result.status, result.raw)
	//nolint:kennlint // deadlinetest: a held poll is observable only as a response that has not arrived over real HTTP.
	case <-time.After(300 * time.Millisecond):
	}
}

func TestFederationPollAdvertisesWaitMaximum(t *testing.T) {
	f := newLongPollFixture(t)
	result := f.poll(t, 0, "")
	require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
	assert.Equal(t, "45", result.header.Get("X-Kata-Federation-Poll-Wait-Max"))
}

func TestFederationPollWithoutWaitAnswersAtOnce(t *testing.T) {
	f := newLongPollFixture(t)
	cursor := f.cursor(t)
	result := f.poll(t, cursor, "")
	require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
	assert.Empty(t, result.body.Events)
	assert.Less(t, result.elapsed, time.Second)
}

func TestFederationPollWaitHoldsUntilProjectEventCommits(t *testing.T) {
	f := newLongPollFixture(t)
	cursor := f.cursor(t)
	done := startLongPoll(t, f, cursor, "30")
	waitForHeldPoll(t, done)
	committed := time.Now()
	createIssueAs(t, f.env, f.project.ID, "tester", "Wakes the held poll")
	var result longPollResult
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("held poll did not return after a project event committed")
	}
	require.NoError(t, result.err)
	require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
	require.NotEmpty(t, result.body.Events)
	assert.Equal(t, "issue.created", result.body.Events[0].Type)
	assert.Greater(t, result.body.NextAfterID, cursor)
	assert.Less(t, time.Since(committed), 5*time.Second)
}

func TestFederationPollWaitReturnsEmptyWhenWaitExpires(t *testing.T) {
	f := newLongPollFixture(t)
	cursor := f.cursor(t)
	result := f.poll(t, cursor, "1")
	require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
	assert.Empty(t, result.body.Events)
	assert.Equal(t, cursor, result.body.NextAfterID)
	assert.GreaterOrEqual(t, result.elapsed, time.Second)
	assert.Less(t, result.elapsed, 5*time.Second)
}

func TestFederationPollWaitIgnoresOtherProjects(t *testing.T) {
	f := newLongPollFixture(t)
	other, err := f.env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	cursor := f.cursor(t)
	done := startLongPoll(t, f, cursor, "2")
	waitForHeldPoll(t, done)
	createIssueAs(t, f.env, other.ID, "tester", "Belongs to another project")
	result := <-done
	require.NoError(t, result.err)
	require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
	assert.Empty(t, result.body.Events)
	assert.GreaterOrEqual(t, result.elapsed, 2*time.Second)
}

func TestFederationPollWaitAnswersAtOnceWhenEventsArePending(t *testing.T) {
	f := newLongPollFixture(t)
	createIssueAs(t, f.env, f.project.ID, "tester", "Already committed")
	result := f.poll(t, 0, "30")
	require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
	assert.NotEmpty(t, result.body.Events)
	assert.Less(t, result.elapsed, 5*time.Second)
}

func TestFederationPollWaitReauthorizesAfterWake(t *testing.T) {
	f := newLongPollFixture(t)
	cursor := f.cursor(t)
	done := startLongPoll(t, f, cursor, "30")
	waitForHeldPoll(t, done)
	require.NoError(t, f.env.DB.RevokeFederationEnrollment(t.Context(), f.enrollment.Enrollment.ID))
	createIssueAs(t, f.env, f.project.ID, "tester", "Committed after revocation")
	var result longPollResult
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("held poll did not return after a project event committed")
	}
	require.NoError(t, result.err)
	assert.Equal(t, http.StatusForbidden, result.status, "%s", result.raw)
	assert.NotContains(t, string(result.raw), "Committed after revocation")
}

func TestFederationPollWaitReauthorizesWhenWaitExpires(t *testing.T) {
	f := newLongPollFixture(t)
	cursor := f.cursor(t)
	done := startLongPoll(t, f, cursor, "2")
	waitForHeldPoll(t, done)
	require.NoError(t, f.env.DB.RevokeFederationEnrollment(t.Context(), f.enrollment.Enrollment.ID))
	result := <-done
	require.NoError(t, result.err)
	assert.Equal(t, http.StatusForbidden, result.status, "%s", result.raw)
}

func TestFederationPollWaitRejectsInvalidWait(t *testing.T) {
	f := newLongPollFixture(t)
	for _, wait := range []string{"-1", "x"} {
		result := f.poll(t, 0, wait)
		assert.Equal(t, http.StatusBadRequest, result.status, "wait=%s: %s", wait, result.raw)
	}
}

func TestFederationPollWaitReleasesWaitersWhenClientsDisconnect(t *testing.T) {
	f := newLongPollFixture(t)
	cursor := f.cursor(t)
	ctx, cancel := context.WithCancel(t.Context())
	var held sync.WaitGroup
	for range 32 {
		held.Go(func() {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet,
				fmt.Sprintf("%s%s/federation/events?after_id=%d&wait=30", f.env.URL, projectPath(f.project.ID), cursor), nil)
			if !assert.NoError(t, err) {
				return
			}
			request.Header.Set("Authorization", "Bearer long-poll-token")
			response, err := http.DefaultClient.Do(request) //nolint:gosec // test-owned loopback daemon
			if err == nil {
				_ = response.Body.Close()
				t.Errorf("held poll answered before its client disconnected: %d", response.StatusCode)
			}
		})
	}
	//nolint:kennlint // sleeptest: the polls park in the hub over real HTTP; no event marks that.
	time.Sleep(500 * time.Millisecond)
	cancel()
	held.Wait()
	// Every bound slot is free again, so this poll is held rather than
	// answered at once.
	//nolint:kennlint // sleeptest: disconnect cleanup runs in hub handlers reached over real HTTP.
	require.Eventually(t, func() bool {
		return f.poll(t, cursor, "1").elapsed >= time.Second
	}, 5*time.Second, 50*time.Millisecond)
}
