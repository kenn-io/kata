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
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// federationReadProbe wraps a hub store to count the ReadFederation calls that
// have returned and to run a hook before a chosen call. Polls read on server
// goroutines while the test watches, so mu guards every field.
type federationReadProbe struct {
	db.Storage
	mu            sync.Mutex
	started       int
	returned      int
	authCalls     int
	revisionCalls int
	byProject     map[int64]int
	changed       chan struct{}
	before        func(read int) error
}

func (p *federationReadProbe) AuthorizeFederationToken(ctx context.Context, token string, projectID int64, capability string) (db.FederationEnrollment, error) {
	p.mu.Lock()
	p.authCalls++
	p.mu.Unlock()
	return p.Storage.AuthorizeFederationToken(ctx, token, projectID, capability)
}

func (p *federationReadProbe) ProjectAccessRevision(ctx context.Context) (int64, error) {
	p.mu.Lock()
	p.revisionCalls++
	p.mu.Unlock()
	return p.Storage.ProjectAccessRevision(ctx)
}

func (p *federationReadProbe) accessCalls() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.authCalls, p.revisionCalls
}

func newFederationReadProbe(store db.Storage) *federationReadProbe {
	return &federationReadProbe{Storage: store, changed: make(chan struct{}), byProject: make(map[int64]int)}
}

// storage returns the probe as the hub's store. A store shared by several hub
// processes must stay a wake bus, or the server never starts the bridge that
// carries their wakeups.
type pollWakeSecretBus interface {
	db.FederationPollWakeBus
	FederationPollWakeSecret(context.Context) ([]byte, error)
}

func (p *federationReadProbe) storage() db.Storage {
	if bus, ok := p.Storage.(pollWakeSecretBus); ok {
		return struct {
			*federationReadProbe
			pollWakeSecretBus
		}{p, bus}
	}
	return p
}

// beforeRead runs hook before each later ReadFederation call, with the call's
// 1-based number.
func (p *federationReadProbe) beforeRead(hook func(read int) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.before = hook
}

func (p *federationReadProbe) ReadFederation(ctx context.Context, params db.FederationReadParams) (db.FederationReadResult, error) {
	p.mu.Lock()
	p.started++
	read, before := p.started, p.before
	p.mu.Unlock()
	if before != nil {
		if err := before(read); err != nil {
			return db.FederationReadResult{}, err
		}
	}
	result, err := p.Storage.ReadFederation(ctx, params)
	p.mu.Lock()
	p.returned++
	p.byProject[params.ProjectID]++
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
	return result, err
}

func (p *federationReadProbe) readsReturned() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.returned
}

// waitForReads blocks until n ReadFederation calls have returned.
func (p *federationReadProbe) waitForReads(t testing.TB, n int) {
	t.Helper()
	//nolint:kennlint // deadlinetest: bounds a read made by a hub handler reached over real HTTP.
	deadline := time.After(10 * time.Second)
	for {
		p.mu.Lock()
		returned, changed := p.returned, p.changed
		p.mu.Unlock()
		if returned >= n {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("the hub returned %d federation reads, want %d", returned, n)
		}
	}
}

// projectReads reports the reads completed for one project.
func (p *federationReadProbe) projectReads(projectID int64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.byProject[projectID]
}

// waitForProjectReads waits for a specific project's reads to finish.
func (p *federationReadProbe) waitForProjectReads(t testing.TB, projectID int64, n int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		p.mu.Lock()
		returned, changed := p.byProject[projectID], p.changed
		p.mu.Unlock()
		if returned >= n {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("project %d returned %d federation reads, want %d", projectID, returned, n)
		}
	}
}

// withFederationReadProbe installs a probe around the test daemon's store.
func withFederationReadProbe(probe **federationReadProbe) testenv.Option {
	return func(cfg *daemon.ServerConfig) {
		*probe = newFederationReadProbe(cfg.DB)
		cfg.DB = (*probe).storage()
	}
}

type longPollFixture struct {
	env        *testenv.Env
	reads      *federationReadProbe
	project    db.Project
	enrollment db.CreatedFederationEnrollment
}

func newLongPollFixture(t *testing.T) longPollFixture {
	t.Helper()
	var reads *federationReadProbe
	env := testenv.New(t, withFederationReadProbe(&reads))
	project := createFederatedHubProject(t, env, "hub-project")
	enrollment, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:            "long-poll-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "pull",
		Actor:            "tester",
	})
	require.NoError(t, err)
	return longPollFixture{env: env, reads: reads, project: project, enrollment: enrollment}
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

// startHeldPoll sends a held poll and returns once the hub has finished the
// poll's first read. The poll subscribes to wakeups before that read, so a
// commit the test makes afterwards reaches the poll only by waking it.
func (f longPollFixture) startHeldPoll(t *testing.T, afterID int64, wait string) <-chan longPollResult {
	t.Helper()
	reads := f.reads.readsReturned()
	done := startLongPoll(t, f, afterID, wait)
	f.reads.waitForReads(t, reads+1)
	return done
}

func TestFederationPollWithoutWaitAnswersAtOnce(t *testing.T) {
	f := newLongPollFixture(t)
	cursor := f.cursor(t)
	result := f.poll(t, cursor, "")
	require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
	assert.Empty(t, result.body.Events)
	assert.Equal(t, "45", result.header.Get("X-Kata-Federation-Poll-Wait-Max"))
	assert.Less(t, result.elapsed, time.Second)
}

func TestFederationPollWaitHoldsUntilProjectEventCommits(t *testing.T) {
	f := newLongPollFixture(t)
	cursor := f.cursor(t)
	done := f.startHeldPoll(t, cursor, "30")
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
	t.Run("no writes", func(t *testing.T) {
		f := newLongPollFixture(t)
		cursor := f.cursor(t)
		result := f.poll(t, cursor, "1")
		require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
		assert.Empty(t, result.body.Events)
		assert.Equal(t, cursor, result.body.NextAfterID)
		assert.GreaterOrEqual(t, result.elapsed, time.Second)
		assert.Less(t, result.elapsed, 5*time.Second)
	})
	t.Run("another project writes", func(t *testing.T) {
		f := newLongPollFixture(t)
		other, err := f.env.DB.CreateProject(t.Context(), "other-project")
		require.NoError(t, err)
		cursor := f.cursor(t)
		reads := f.reads.readsReturned()
		done := f.startHeldPoll(t, cursor, "2")
		createIssueAs(t, f.env, other.ID, "tester", "Belongs to another project")
		result := <-done
		require.NoError(t, result.err)
		require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
		assert.Empty(t, result.body.Events)
		assert.GreaterOrEqual(t, result.elapsed, 2*time.Second)
		// The poll reads once before it waits. A wakeup for the other project
		// or the timeout would add a read.
		assert.Equal(t, reads+1, f.reads.readsReturned())
	})
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
	for _, tc := range []struct {
		name, wait string
		wake       bool
	}{{"wake", "30", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLongPollFixture(t)
			cursor := f.cursor(t)
			done := f.startHeldPoll(t, cursor, tc.wait)
			reads := f.reads.readsReturned()
			require.NoError(t, f.env.DB.RevokeFederationEnrollment(t.Context(), f.enrollment.Enrollment.ID))
			if tc.wake {
				createIssueAs(t, f.env, f.project.ID, "tester", "Committed after revocation")
			}
			var result longPollResult
			select {
			case result = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("held poll did not return")
			}
			require.NoError(t, result.err)
			assert.Equal(t, http.StatusForbidden, result.status, "%s", result.raw)
			assert.NotContains(t, string(result.raw), "Committed after revocation")
			assert.Equal(t, reads, f.reads.readsReturned())
		})
	}
}

func TestFederationPollWaitDefersRevocationUntilNextPoll(t *testing.T) {
	f := newLongPollFixture(t)
	cursor := f.cursor(t)
	done := f.startHeldPoll(t, cursor, "2")
	require.NoError(t, f.env.DB.RevokeFederationEnrollment(t.Context(), f.enrollment.Enrollment.ID))
	result := <-done
	require.NoError(t, result.err)
	require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
	assert.Empty(t, result.body.Events)
	assert.Equal(t, cursor, result.body.NextAfterID)
	assert.Equal(t, http.StatusForbidden, f.poll(t, cursor, "").status)
}
