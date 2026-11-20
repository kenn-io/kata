package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestFederationPollWaitsKeepHalfTheControlAdmissionSlotsFree(t *testing.T) {
	waits := newFederationPollWaits(16)
	broadcaster := NewEventBroadcaster()
	project := db.Project{ID: 7, UID: "01HZNQ7VFPK1XGD8R5MABCD4EA"}
	signedCtx := withFederationControlAdmission(t.Context())
	for enrollment := range int64(8) {
		waiter, ok := waits.admit(signedCtx, broadcaster, enrollment, project)
		require.True(t, ok, "signed poll %d", enrollment)
		t.Cleanup(waiter.release)
	}
	_, ok := waits.admit(signedCtx, broadcaster, 100, project)
	assert.False(t, ok, "a signed poll beyond half the control slots must answer at once")
	unsigned, ok := waits.admit(t.Context(), broadcaster, 100, project)
	require.True(t, ok, "unsigned polls hold no control slot")
	unsigned.release()
}

func TestFederationPollWaitsBoundEveryHeldPoll(t *testing.T) {
	waits := newFederationPollWaits(16)
	broadcaster := NewEventBroadcaster()
	project := db.Project{ID: 7, UID: "01HZNQ7VFPK1XGD8R5MABCD4EA"}
	for enrollment := range int64(federationPollMaxWaiters) {
		waiter, ok := waits.admit(t.Context(), broadcaster, enrollment, project)
		require.True(t, ok)
		t.Cleanup(waiter.release)
	}
	_, ok := waits.admit(t.Context(), broadcaster, federationPollMaxWaiters, project)
	assert.False(t, ok)
}

func TestFederationPollWakeIgnoresItsOwnInstance(t *testing.T) {
	waits := newFederationPollWaits(16)
	woken, unsubscribe := waits.remote.subscribe()
	defer unsubscribe()
	waits.receive(waits.message())
	select {
	case <-woken:
		t.Fatal("a process must ignore the wakeups it published")
	default:
	}
	other := newFederationPollWaits(16)
	waits.receive(other.message())
	select {
	case <-woken:
	default:
		t.Fatal("another process's wakeup was not delivered")
	}
}

// recordingPollWakeBus records what a hub process publishes to the others.
type recordingPollWakeBus struct{ published chan string }

func (b recordingPollWakeBus) PublishFederationPollWake(_ context.Context, payload string) error {
	b.published <- payload
	return nil
}

func (recordingPollWakeBus) ListenFederationPollWake(ctx context.Context, _ func(), _ func(string)) error {
	<-ctx.Done()
	return ctx.Err()
}

// Every role that can connect to a shared PostgreSQL database can listen on
// the wake channel, so a wake names no project.
func TestFederationPollWakeNamesNoProject(t *testing.T) {
	waits := newFederationPollWaits(16)
	broadcaster := NewEventBroadcaster()
	bus := recordingPollWakeBus{published: make(chan string, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runFederationPollWakeBridge(ctx, bus, broadcaster, waits, slog.New(slog.DiscardHandler))
	}()
	defer func() { cancel(); <-done }()
	const movedFrom = "01HZNQ7VFPK1XGD8R5MABCD4EA"
	payload := fmt.Sprintf(`{"from_project_uid":%q}`, movedFrom)
	require.Eventually(t, func() bool {
		broadcaster.Broadcast(NewEventMsg(4242, db.Event{Type: "issue.moved", Payload: payload}))
		select {
		case published := <-bus.published:
			assert.Equal(t, waits.instance, published, "a wake carries only the publishing instance")
			return true
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond)
}

// closedPollWakeBus is a store that has been closed.
type closedPollWakeBus struct{}

func (closedPollWakeBus) PublishFederationPollWake(context.Context, string) error {
	return db.ErrFederationPollWakeClosed
}

func (closedPollWakeBus) ListenFederationPollWake(context.Context, func(), func(string)) error {
	return db.ErrFederationPollWakeClosed
}

func TestFederationPollWakeBridgeStopsWhenItsStoreCloses(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runFederationPollWakeBridge(t.Context(), closedPollWakeBus{}, NewEventBroadcaster(), newFederationPollWaits(16), slog.New(slog.DiscardHandler))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second): //nolint:kennlint // deadlinetest: bounds a goroutine that would otherwise run until the test context ends.
		t.Fatal("the wake bridge kept retrying a closed store")
	}
}

func (w *federationPollWaits) heldFor(enrollmentID int64) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.byCredential[enrollmentID]
}

func TestFederationPollWaitAnswersAtOnceWhenCredentialWaitersAreFull(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "hub.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "tester")
	require.NoError(t, err)
	enrollment, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token: "long-poll-token", SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EB",
		ProjectID: &project.ID, Capabilities: "pull", Actor: "tester",
	})
	require.NoError(t, err)
	hub := NewServer(ServerConfig{DB: store, StartedAt: time.Now().UTC()})
	t.Cleanup(func() { _ = hub.Close() })
	server := httptest.NewServer(hub.Handler())
	t.Cleanup(server.Close)

	var cursor int64
	poll := func(ctx context.Context, wait string) (int, time.Duration, error) {
		url := fmt.Sprintf("%s/api/v1/projects/%d/federation/events?after_id=%d&wait=%s", server.URL, project.ID, cursor, wait)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, 0, err
		}
		request.Header.Set("Authorization", "Bearer long-poll-token")
		started := time.Now()
		response, err := http.DefaultClient.Do(request) //nolint:gosec // test-owned loopback hub
		if err != nil {
			return 0, 0, err
		}
		defer func() { _ = response.Body.Close() }()
		var page struct {
			NextAfterID int64 `json:"next_after_id"`
		}
		if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
			return response.StatusCode, 0, err
		}
		if wait == "0" {
			cursor = page.NextAfterID
		}
		return response.StatusCode, time.Since(started), nil
	}
	initial, _, err := poll(ctx, "0")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, initial)
	require.Positive(t, cursor)

	held, release := context.WithCancel(ctx)
	defer release()
	for range federationPollMaxWaitersPerCredential {
		go func() { _, _, _ = poll(held, "45") }()
	}
	waits := hub.cfg.federationPollWaits
	id := enrollment.Enrollment.ID
	//nolint:kennlint // sleeptest: the polls reach the hub over real HTTP; the waiter count is the signal.
	require.Eventually(t, func() bool { return waits.heldFor(id) == federationPollMaxWaitersPerCredential },
		10*time.Second, 5*time.Millisecond, "the hub never held every poll")

	status, elapsed, err := poll(ctx, "45")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	assert.Less(t, elapsed, 5*time.Second, "a poll beyond the per-credential bound must answer like an ordinary poll")
	assert.Equal(t, federationPollMaxWaitersPerCredential, waits.heldFor(id))
}

func TestFederationPollWaitCapsOversizedWaits(t *testing.T) {
	wait, err := federationPollWait(t.Context(), api.OptionalInt{IsSet: true, Value: 9223372037})
	require.NoError(t, err)
	assert.Equal(t, federationPollWaitMax, wait)
}
