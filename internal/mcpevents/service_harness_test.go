package mcpevents_test

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.kenn.io/kata/internal/mcpevents"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

const (
	testProjectA = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	testProjectB = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	testIssue    = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	testCallback = "https://example.com/events?route=inbox"
	testDaemon   = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
)

// opener returns a fresh, empty store for the calling (sub)test.
type opener func(t *testing.T) db.Storage

// forEachBackendOpener runs fn against a real SQLite store and, when a
// container runtime or KATA_TEST_POSTGRES_DSN is available, a real PostgreSQL
// store. Each call to open yields an isolated store, so daemon-wide counts
// and consent never leak between subtests.
func forEachBackendOpener(t *testing.T, fn func(t *testing.T, open opener)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { fn(t, openSQLite) })
	t.Run("postgres", func(t *testing.T) {
		if testing.Short() {
			t.Skip("requires postgres")
		}
		fn(t, func(t *testing.T) db.Storage {
			t.Helper()
			dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
			t.Cleanup(cleanup)
			store, err := pgstore.Open(t.Context(), dsn)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			return store
		})
	})
}

// forEachBackend runs fn once per backend against one fresh store.
// openSQLite opens a fresh SQLite store, for tests whose subject never
// reaches storage and so gains nothing from a second backend.
func openSQLite(t *testing.T) db.Storage {
	t.Helper()
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func forEachBackend(t *testing.T, fn func(t *testing.T, store db.Storage)) {
	t.Helper()
	forEachBackendOpener(t, func(t *testing.T, open opener) { fn(t, open(t)) })
}

// receive waits for a signal, failing the test instead of hanging.
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(15 * time.Second):
		require.FailNow(t, "timed out waiting for a signal")
		panic("unreachable")
	}
}

// testClock is a settable service clock.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newTestClock() *testClock { return &testClock{at: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// memKeys is a KeyProvider that counts creations.
type memKeys struct {
	mu      sync.Mutex
	key     []byte
	loadErr error
	creates int
}

func newMemKeys(b byte) *memKeys {
	key := make([]byte, 32)
	for i := range key {
		key[i] = b
	}
	return &memKeys{key: key}
}

func (m *memKeys) Load(context.Context) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	if m.key == nil {
		return nil, mcpevents.ErrKeyMissing
	}
	return append([]byte(nil), m.key...), nil
}

func (m *memKeys) Create(context.Context) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.key == nil {
		m.creates++
		m.key = make([]byte, 32)
		for i := range m.key {
			m.key[i] = 0x42
		}
	}
	return append([]byte(nil), m.key...), nil
}

// fakeAuthorized is a controllable authority snapshot. The subscription rows
// under test live in a real store.
type fakeAuthorized struct {
	mu          sync.Mutex
	projects    []mcpevents.Project
	hiddenIssue map[string]bool
	// hiddenProject hides a project from AuthorizeOccurrence.
	hiddenProject map[string]bool
	// unrequested lists issues that carry no request for any recipient.
	unrequested map[string]bool
	scoped      bool
}

func (f *fakeAuthorized) LifecycleMayChangeInbox(_ context.Context, issueUID, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.unrequested[issueUID], nil
}

func (f *fakeAuthorized) Scoped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scoped
}

func (f *fakeAuthorized) VisibleProjects(_ context.Context, selection []string) ([]mcpevents.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mcpevents.Project
	for _, p := range f.projects {
		if selection == nil {
			out = append(out, p)
			continue
		}
		for _, uid := range selection {
			if uid == p.UID {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

func (f *fakeAuthorized) AuthorizeOccurrence(_ context.Context, o mcpevents.Occurrence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hiddenIssue[o.IssueUID] || f.hiddenProject[o.ProjectUID] {
		return mcpevents.ErrOccurrenceHidden
	}
	return nil
}

func (f *fakeAuthorized) setProjects(projects ...mcpevents.Project) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects = projects
}

type fakeAuthority struct {
	mu         sync.Mutex
	authorized *fakeAuthorized
	err        error
	// revokedActor revokes only principals acting as this actor.
	revokedActor string
	calls        atomic.Int64
}

func (a *fakeAuthority) Reacquire(_ context.Context, p mcpevents.Principal) (mcpevents.Authorized, error) {
	a.calls.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return nil, a.err
	}
	if a.revokedActor != "" && p.Actor == a.revokedActor {
		return nil, mcpevents.ErrPrincipalRevoked
	}
	return a.authorized, nil
}

func (a *fakeAuthority) fail(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = err
}

// receiver is a TLS webhook endpoint that answers verification challenges.
type receiver struct {
	challenges atomic.Int64
	status     atomic.Int64 // challenge response status; 0 means 200
	gate       chan struct{}
	entered    chan struct{}
	client     *mcpevents.WebhookClient
	// deliver, when set before the first request, answers every request that
	// is not a verification challenge.
	deliver func(http.ResponseWriter, *http.Request, []byte)
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{}
	client, _ := mcpevents.FixtureWebhook(t, func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
		}
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &body)
		if body.Type != "verification" {
			if r.deliver != nil {
				r.deliver(w, req, raw)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		r.challenges.Add(1)
		if r.entered != nil {
			select {
			case r.entered <- struct{}{}:
			default:
			}
		}
		if r.gate != nil {
			<-r.gate
		}
		if s := int(r.status.Load()); s != 0 {
			w.WriteHeader(s)
			return
		}
		_ = json.MarshalWrite(w, map[string]string{"challenge": body.Challenge})
	})
	r.client = client
	return r
}

type harness struct {
	t     *testing.T
	store db.Storage
	svc   *mcpevents.Service
	auth  *fakeAuthority
	fake  *fakeAuthorized
	keys  *memKeys
	clock *testClock
	recv  *receiver
	key   []byte
}

func newHarness(t *testing.T, store db.Storage, limits ...mcpevents.Limits) *harness {
	t.Helper()
	return newHarnessWith(t, store, func(cfg *mcpevents.ServiceConfig) {
		if len(limits) > 0 {
			cfg.Limits = limits[0]
		}
	})
}

// newHarnessWith builds a harness whose ServiceConfig the caller adjusts.
func newHarnessWith(t *testing.T, store db.Storage, mutate func(*mcpevents.ServiceConfig)) *harness {
	t.Helper()
	h := &harness{
		t: t, store: store, keys: newMemKeys(0x11), clock: newTestClock(), recv: newReceiver(t),
		fake: &fakeAuthorized{projects: []mcpevents.Project{{UID: testProjectA, ID: 1, Name: "hub-project"}}},
	}
	h.auth = &fakeAuthority{authorized: h.fake}
	cfg := mcpevents.ServiceConfig{
		Store: store, Keys: h.keys, Authority: h.auth, Webhook: h.recv.client, Now: h.clock.Now,
		Jitter: func() float64 { return 0 }, RenewInterval: time.Hour, PollInterval: time.Hour,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	svc, err := mcpevents.NewService(t.Context(), cfg)
	require.NoError(t, err)
	require.True(t, svc.Availability().Enabled, svc.Availability().Reason)
	h.svc = svc
	h.key = h.keys.key
	return h
}

var (
	principalA = mcpevents.Principal{Authority: mcpevents.AuthorityDBToken, Subject: "7", Actor: "example-user"}
	allScope   = mcpevents.ForwarderScope{All: true}
)

func testSecret(b byte) string { return mcpevents.FixtureSecret(b, 32) }

func (h *harness) request(recipient string) mcpevents.SubscribeRequest {
	return mcpevents.SubscribeRequest{
		Name: mcpevents.EventName, Arguments: mcpevents.Arguments{For: recipient},
		Delivery: mcpevents.Delivery{Mode: "webhook", URL: testCallback, Secret: testSecret(1)},
	}
}

func (h *harness) subscribe(p mcpevents.Principal, req mcpevents.SubscribeRequest) (mcpevents.SubscribeResult, error) {
	h.t.Helper()
	return h.svc.Subscribe(h.t.Context(), p, allScope, req)
}

func (h *harness) mustSubscribe(req mcpevents.SubscribeRequest) mcpevents.SubscribeResult {
	h.t.Helper()
	result, err := h.subscribe(principalA, req)
	require.NoError(h.t, err)
	return result
}

func (h *harness) row(id string) db.MCPEventSubscription {
	h.t.Helper()
	row, err := h.store.GetMCPEventSubscription(h.t.Context(), id)
	require.NoError(h.t, err)
	return row
}

func (h *harness) unsubscribe(req mcpevents.SubscribeRequest) error {
	return h.svc.Unsubscribe(h.t.Context(), principalA, mcpevents.UnsubscribeRequest{Name: req.Name, Arguments: req.Arguments, Delivery: mcpevents.Delivery{URL: req.Delivery.URL}})
}

// touch marks a subscription dirty, as an archive change that concerns it
// would.
func (h *harness) touch(id string) {
	h.t.Helper()
	require.NoError(h.t, h.store.MarkMCPEventSubscriptionsDirty(h.t.Context(), []string{id}))
}
