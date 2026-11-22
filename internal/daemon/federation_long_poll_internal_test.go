package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestFederationPollWaitsKeepHalfTheControlAdmissionSlotsFree(t *testing.T) {
	waits := newFederationPollWaits(16, 8)
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
	waits := newFederationPollWaits(16, 8)
	broadcaster := NewEventBroadcaster()
	project := db.Project{ID: 7, UID: "01HZNQ7VFPK1XGD8R5MABCD4EA"}
	for range federationPollMaxWaitersPerCredential {
		waiter, ok := waits.admit(t.Context(), broadcaster, -1, project)
		require.True(t, ok)
		t.Cleanup(waiter.release)
	}
	_, ok := waits.admit(t.Context(), broadcaster, -1, project)
	assert.False(t, ok)

	for _, tc := range []struct{ cpus, limit int }{{1, 32}, {16, 256}} {
		t.Run(fmt.Sprintf("%d CPUs", tc.cpus), func(t *testing.T) {
			waits := newFederationPollWaits(16, tc.cpus)
			broadcaster := NewEventBroadcaster()
			project := db.Project{ID: 7, UID: "01HZNQ7VFPK1XGD8R5MABCD4EA"}
			for enrollment := range int64(tc.limit) {
				waiter, ok := waits.admit(t.Context(), broadcaster, enrollment, project)
				require.True(t, ok)
				t.Cleanup(waiter.release)
			}
			_, ok := waits.admit(t.Context(), broadcaster, int64(tc.limit), project)
			assert.False(t, ok)
		})
	}
}

func TestFederationPollWaitsReserveCapacityForOtherCredentials(t *testing.T) {
	waits := newFederationPollWaits(16, 1)
	broadcaster := NewEventBroadcaster()
	project := db.Project{ID: 7, UID: "01HZNQ7VFPK1XGD8R5MABCD4EA"}
	for range 16 {
		waiter, ok := waits.admit(t.Context(), broadcaster, 1, project)
		require.True(t, ok)
		t.Cleanup(waiter.release)
	}
	_, ok := waits.admit(t.Context(), broadcaster, 1, project)
	assert.False(t, ok)
	other, ok := waits.admit(t.Context(), broadcaster, 2, project)
	require.True(t, ok)
	other.release()
}

func TestFederationPollWakeIgnoresItsOwnInstance(t *testing.T) {
	waits := newFederationPollWaits(16, 8)
	woken, unsubscribe := waits.remote.subscribe(SubFilter{})
	defer unsubscribe()
	waits.remote.setSecret([]byte("test secret"))
	waits.receive(waits.messageFor(NewEventMsg(1, db.Event{ProjectUID: "project"})))
	select {
	case <-woken:
		t.Fatal("a process must ignore the wakeups it published")
	default:
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

func TestFederationPollRemoteWakeMatchesProjectAndMoveSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		writer, reader := newFederationPollWaits(16, 8), newFederationPollWaits(16, 8)
		writer.remote.setSecret([]byte("test secret"))
		reader.remote.setSecret([]byte("test secret"))
		broadcaster := NewEventBroadcaster()
		bus := recordingPollWakeBus{published: make(chan string, 1)}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			publishFederationPollWakes(ctx, bus, broadcaster, writer, slog.New(slog.DiscardHandler))
		}()
		defer func() { cancel(); <-done }()
		var waiters []*federationPollWaiter
		for i, uid := range []string{"source", "destination", "unrelated"} {
			waiter, ok := reader.admit(ctx, NewEventBroadcaster(), int64(i+1), db.Project{ID: int64(i + 1), UID: uid})
			require.True(t, ok)
			defer waiter.release()
			waiters = append(waiters, waiter)
		}
		synctest.Wait()
		broadcaster.Broadcast(NewEventMsg(2, db.Event{ProjectUID: "destination", Type: "issue.moved", Payload: `{"from_project_uid":"source"}`}))
		synctest.Wait()
		published := <-bus.published
		reader.receive(published)
		for i, waiter := range waiters {
			select {
			case <-waiter.remote:
				if i == 2 {
					t.Error("unrelated project woke for a move")
				}
			default:
				if i != 2 {
					t.Errorf("project %d did not wake for a move", i+1)
				}
			}
		}
		broadcaster.Broadcast(NewEventMsg(3, db.Event{ProjectUID: "unrelated", Type: "issue.updated"}))
		synctest.Wait()
		reader.receive(<-bus.published)
		for i, waiter := range waiters {
			select {
			case <-waiter.remote:
				if i != 2 {
					t.Errorf("project %d woke for another project's event", i+1)
				}
			default:
				if i == 2 {
					t.Error("the affected project did not wake")
				}
			}
		}
	})
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
		runFederationPollWakeBridge(t.Context(), closedPollWakeBus{}, NewEventBroadcaster(), newFederationPollWaits(16, 8), slog.New(slog.DiscardHandler))
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

func (w *federationPollWaits) heldTotal() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total
}

// internalLongPollHub serves a hub over real HTTP with one pull enrollment
// for one federated project, and exposes the hub's held-poll bookkeeping.
type internalLongPollHub struct {
	waits        *federationPollWaits
	url          string
	projectID    int64
	enrollmentID int64
}

func newInternalLongPollHub(t *testing.T) internalLongPollHub {
	t.Helper()
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
	return internalLongPollHub{
		waits: hub.cfg.federationPollWaits, url: server.URL,
		projectID: project.ID, enrollmentID: enrollment.Enrollment.ID,
	}
}

// poll returns the response status, the page's next cursor and how long the
// hub took to answer.
func (h internalLongPollHub) poll(ctx context.Context, afterID int64, wait string) (int, int64, time.Duration, error) {
	url := fmt.Sprintf("%s/api/v1/projects/%d/federation/events?after_id=%d&wait=%s", h.url, h.projectID, afterID, wait)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, 0, err
	}
	request.Header.Set("Authorization", "Bearer long-poll-token")
	started := time.Now()
	response, err := http.DefaultClient.Do(request) //nolint:gosec // test-owned loopback hub
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() { _ = response.Body.Close() }()
	var page struct {
		NextAfterID int64 `json:"next_after_id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		return response.StatusCode, 0, 0, err
	}
	return response.StatusCode, page.NextAfterID, time.Since(started), nil
}

// holdEveryCredentialSlot parks as many held polls as one credential may hold
// and returns once the hub holds all of them. Canceling ctx disconnects them;
// the returned function waits for those polls to end.
func (h internalLongPollHub) holdEveryCredentialSlot(ctx context.Context, t *testing.T, cursor int64) func() {
	t.Helper()
	limit := min(federationPollMaxWaitersPerCredential, h.waits.maxTotal/2)
	var polls sync.WaitGroup
	for range limit {
		polls.Go(func() {
			if status, _, _, err := h.poll(ctx, cursor, "45"); err == nil {
				t.Errorf("held poll answered before its client disconnected: %d", status)
			}
		})
	}
	//nolint:kennlint // sleeptest: the polls reach the hub over real HTTP; the waiter count is the signal.
	require.Eventually(t, func() bool { return h.waits.heldFor(h.enrollmentID) == limit },
		10*time.Second, 5*time.Millisecond, "the hub never held every poll")
	return polls.Wait
}

func TestFederationPollWaitAnswersAtOnceWhenCredentialWaitersAreFull(t *testing.T) {
	ctx := t.Context()
	hub := newInternalLongPollHub(t)
	status, cursor, _, err := hub.poll(ctx, 0, "0")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Positive(t, cursor)

	held, release := context.WithCancel(ctx)
	ended := hub.holdEveryCredentialSlot(held, t, cursor)
	defer func() { release(); ended() }()

	status, _, elapsed, err := hub.poll(ctx, cursor, "45")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	assert.Less(t, elapsed, 5*time.Second, "a poll beyond the per-credential bound must answer like an ordinary poll")
}

func TestFederationPollWaitReleasesWaitersWhenClientsDisconnect(t *testing.T) {
	ctx := t.Context()
	hub := newInternalLongPollHub(t)
	status, cursor, _, err := hub.poll(ctx, 0, "0")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	held, disconnect := context.WithCancel(ctx)
	defer disconnect()
	ended := hub.holdEveryCredentialSlot(held, t, cursor)
	disconnect()
	ended()
	//nolint:kennlint // sleeptest: disconnect cleanup runs in hub handlers reached over real HTTP.
	require.Eventually(t, func() bool {
		return hub.waits.heldFor(hub.enrollmentID) == 0 && hub.waits.heldTotal() == 0
	}, 5*time.Second, 5*time.Millisecond, "the hub kept slots for polls whose clients disconnected")
}

func TestFederationPollWaitCapsOversizedWaits(t *testing.T) {
	wait, err := federationPollWait(t.Context(), api.OptionalInt{IsSet: true, Value: 9223372037})
	require.NoError(t, err)
	assert.Equal(t, federationPollWaitMax, wait)
	_, err = federationPollWait(t.Context(), api.OptionalInt{IsSet: true, Value: -1})
	assert.Error(t, err)
}

func TestFederationPollWakeTagsAndFallback(t *testing.T) {
	writer, reader := newFederationPollWaits(16), newFederationPollWaits(16)
	writer.remote.setSecret([]byte("first secret"))
	msg := NewEventMsg(123456, db.Event{ProjectUID: "destination-project-uid", Type: "issue.moved", Payload: `{"from_project_uid":"source-project-uid"}`})
	first := writer.messageFor(msg)
	assert.NotContains(t, first, "123456")
	assert.NotContains(t, first, "destination-project-uid")
	assert.NotContains(t, first, "source-project-uid")
	writer.remote.setSecret([]byte("another secret"))
	assert.NotEqual(t, first, writer.messageFor(msg))
	woken, unsubscribe := reader.remote.subscribe(SubFilter{ProjectID: 1, ProjectUID: "unrelated"})
	defer unsubscribe()
	reader.receive(first)
	select {
	case <-woken:
	default:
		t.Fatal("a tagged wake must wake a poll without a secret")
	}
	reader.remote.setSecret([]byte("first secret"))
	for _, payload := range []string{"other", "other invalid", "other 64", ""} {
		reader.receive(payload)
		select {
		case <-woken:
		default:
			t.Errorf("payload %q failed to wake an unrelated project", payload)
		}
	}
	writer.remote.setSecret(nil)
	assert.Equal(t, writer.instance, writer.messageFor(msg))
}
