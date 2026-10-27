package kata

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/mcpevents"
	"go.kenn.io/kata/internal/notification"
)

const (
	embeddedEventsToken  = "embedded-token"
	embeddedEventsSecret = "whsec_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	embeddedEventsURL    = "https://example.com/events?route=inbox"
)

type memoryEventsKey struct {
	mu  sync.Mutex
	key []byte
}

func (k *memoryEventsKey) Load(context.Context) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.key == nil {
		return nil, ErrMCPEventsKeyMissing
	}
	return append([]byte(nil), k.key...), nil
}

func (k *memoryEventsKey) Create(context.Context) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.key == nil {
		k.key = make([]byte, 32)
		if _, err := rand.Read(k.key); err != nil {
			return nil, err
		}
	}
	return append([]byte(nil), k.key...), nil
}

// eventsReceiver is a TLS receiver that echoes challenges and records signed
// event deliveries.
type eventsReceiver struct {
	webhook    *mcpevents.WebhookClient
	mu         sync.Mutex
	deliveries []eventsDelivery
}

type eventsDelivery struct {
	headers  http.Header
	envelope mcpevents.Envelope
}

func newEventsReceiver(t *testing.T) *eventsReceiver {
	t.Helper()
	r := &eventsReceiver{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Challenge string `json:"challenge"`
			mcpevents.Envelope
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		if body.Challenge != "" {
			_ = json.NewEncoder(w).Encode(map[string]string{"challenge": body.Challenge})
			return
		}
		r.mu.Lock()
		r.deliveries = append(r.deliveries, eventsDelivery{headers: req.Header.Clone(), envelope: body.Envelope})
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	r.webhook = mcpevents.NewWebhookClient(mcpevents.WebhookOptions{
		Resolve: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
		RootCAs: roots,
	})
	return r
}

func (r *eventsReceiver) received() []eventsDelivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]eventsDelivery(nil), r.deliveries...)
}

func embeddedEventsCall(t *testing.T, service *Service, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = strings.NewReader(string(raw))
	} else {
		reader = strings.NewReader("")
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Authorization", "Bearer "+embeddedEventsToken)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	service.Handler().ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.Bytes()
}

func TestServiceMCPEventsWithoutAKeyProviderDisablesOnlyEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	databasePath := filepath.Join(t.TempDir(), "service.db")
	service, err := New(ctx, Config{
		DSN: databasePath, Auth: AuthConfig{Token: embeddedEventsToken},
		MCPEvents: MCPEventsConfig{Enabled: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	require.NotNil(t, service.mcpEvents)
	assert.False(t, service.mcpEvents.Availability().Enabled)
	assert.Equal(t, mcpevents.ReasonNotConfigured, service.mcpEvents.Availability().Reason)

	status, raw := embeddedEventsCall(t, service, http.MethodGet, "/api/v1/mcp-events/subscriptions", nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var statuses struct {
		Enabled bool   `json:"enabled"`
		Reason  string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(raw, &statuses))
	assert.False(t, statuses.Enabled)
	assert.Equal(t, mcpevents.ReasonNotConfigured, statuses.Reason)

	// The other workers keep running: an expired timed claim is still swept.
	issue := seedExpiredWorkerClaim(ctx, t, service)
	inspection, err := sql.Open("sqlite", "file:"+databasePath+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspection.Close()) })
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(ctx) }()
	//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
	require.Eventually(t, func() bool {
		var released bool
		err := inspection.QueryRowContext(ctx,
			`SELECT released_at IS NOT NULL FROM issue_claims WHERE issue_uid = ?`, issue.UID).Scan(&released)
		return err == nil && released
	}, 15*time.Second, 50*time.Millisecond, "the timed-claim worker runs without Events")
	select {
	case runErr := <-runDone:
		require.FailNow(t, "Run ended although only Events is disabled", "%v", runErr)
	default:
	}
	cancel()
	assert.NoError(t, <-runDone)
}

func TestServiceMCPEventsDisabledInConfigServesNoEvents(t *testing.T) {
	service, err := New(t.Context(), Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{Token: embeddedEventsToken}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	assert.Nil(t, service.mcpEvents)
	status, raw := embeddedEventsCall(t, service, http.MethodGet, "/api/v1/mcp-events/subscriptions", nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Contains(t, string(raw), `"enabled":false`)
}

func TestServiceMCPEventsDeliversUnderTheWorkerTransactionFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	receiver := newEventsReceiver(t)
	key := &memoryEventsKey{}
	service, err := newService(ctx, Config{
		DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{Token: embeddedEventsToken},
		MCPEvents:              MCPEventsConfig{Enabled: true, Key: key},
		WorkerTransactionFence: func(context.Context, Transaction) error { return nil },
	}, serviceDeps{mcpEventsWebhook: receiver.webhook})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	require.True(t, service.mcpEvents.Availability().Enabled, service.mcpEvents.Availability().Reason)
	// Only the Events worker receives this wrapper. Other background workers
	// cannot satisfy the context observations on its claim and delivery writes.
	observed := &eventsWorkerContextStore{Storage: service.store}
	service.mcpEvents = newMCPEventsService(MCPEventsConfig{Enabled: true, Key: key},
		daemon.ServerConfig{DB: observed, Auth: config.AuthConfig{Token: embeddedEventsToken}},
		service.broadcaster, receiver.webhook, service.logger)
	require.True(t, service.mcpEvents.Availability().Enabled, service.mcpEvents.Availability().Reason)

	status, raw := embeddedEventsCall(t, service, http.MethodPost, "/api/v1/mcp-events/subscribe", map[string]any{
		"name":      mcpevents.EventName,
		"arguments": map[string]any{"for": "example-user"},
		"delivery":  map[string]any{"mode": "webhook", "url": embeddedEventsURL, "secret": embeddedEventsSecret},
	})
	require.Equal(t, http.StatusOK, status, string(raw))

	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(ctx) }()

	project, err := service.store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	notify, err := json.Marshal(notification.Value{From: "coordinator", Message: "please review"})
	require.NoError(t, err)
	_, event, err := service.store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Review", Author: "coordinator",
		Metadata: map[string]json.RawMessage{notification.MetadataKey("example-user"): notify},
	})
	require.NoError(t, err)
	service.publishWorkerEvents(project.ID, []db.Event{event})

	var delivered eventsDelivery
	//nolint:kennlint // Native database and HTTP worker progress requires real I/O; bounded polling observes retained state.
	require.Eventually(t, func() bool {
		for _, delivery := range receiver.received() {
			if delivery.envelope.Name == mcpevents.EventName && delivery.envelope.Data.SourceEventUID == event.UID {
				delivered = delivery
				return true
			}
		}
		return false
	}, 15*time.Second, 25*time.Millisecond, "the embedded worker delivers the source inbox event")
	headers := delivered.headers
	assert.NotEmpty(t, headers.Get("webhook-id"))
	assert.NotEmpty(t, headers.Get("webhook-timestamp"))
	assert.NotEmpty(t, headers.Get("webhook-signature"))
	assert.Positive(t, observed.claims.Load(), "observed an actual Events claim")
	assert.Positive(t, observed.updates.Load(), "observed an actual Events delivery update")
	assert.Zero(t, observed.unfenced.Load(), "all Events claim and delivery contexts carry the worker transaction fence")

	cancel()
	assert.NoError(t, <-runDone)
}

type eventsWorkerContextStore struct {
	db.Storage
	claims, updates, unfenced atomic.Int64
}

func (s *eventsWorkerContextStore) ClaimMCPEventSubscription(ctx context.Context, in db.MCPEventClaim) (bool, error) {
	s.claims.Add(1)
	if !db.HasTransactionFence(ctx) {
		s.unfenced.Add(1)
	}
	return s.Storage.ClaimMCPEventSubscription(ctx, in)
}

func (s *eventsWorkerContextStore) UpdateMCPEventDelivery(ctx context.Context, in db.MCPEventDeliveryUpdate) (bool, error) {
	s.updates.Add(1)
	if !db.HasTransactionFence(ctx) {
		s.unfenced.Add(1)
	}
	return s.Storage.UpdateMCPEventDelivery(ctx, in)
}

type eventsAccessLease func(context.Context) error

func (l eventsAccessLease) Revalidate(ctx context.Context) error { return l(ctx) }

type eventsLeaseController struct{ lease AccessLease }

func (c eventsLeaseController) Authorize(context.Context, AccessRequest) (AccessDecision, error) {
	return AccessDecision{Lease: c.lease}, nil
}

func TestMCPEventsHostLeaseAdapterPreservesErrorIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		leaseErr error
		denied   bool
	}{
		{name: "wrapped denial", leaseErr: fmt.Errorf("lease ended: %w", ErrAccessDenied), denied: true},
		{name: "temporary outage", leaseErr: errors.New("controller offline")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := hostAccessControllerAdapter{controller: eventsLeaseController{
				lease: eventsAccessLease(func(context.Context) error { return tc.leaseErr }),
			}}
			decision, err := adapter.Authorize(t.Context(), daemon.HostAccessRequest{})
			require.NoError(t, err)
			require.NotNil(t, decision.Revalidate)
			err = decision.Revalidate(t.Context())
			assert.ErrorIs(t, err, tc.leaseErr)
			if tc.denied {
				assert.ErrorIs(t, err, daemon.ErrHostAccessDenied)
			} else {
				assert.NotErrorIs(t, err, daemon.ErrHostAccessDenied)
			}
		})
	}
}

func TestServiceMCPEventsAppliesTheConfiguredGrantPolicy(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	receiver := newEventsReceiver(t)
	service, err := newService(ctx, Config{
		DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{Token: embeddedEventsToken},
		MCPEvents: MCPEventsConfig{Enabled: true, Key: &memoryEventsKey{}, MaxTTL: 2 * time.Hour, RefuseNoExpiry: true},
	}, serviceDeps{mcpEventsWebhook: receiver.webhook})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	before := time.Now()
	status, raw := embeddedEventsCall(t, service, http.MethodPost, "/api/v1/mcp-events/subscribe", map[string]any{
		"name":      mcpevents.EventName,
		"arguments": map[string]any{"for": "example-user"},
		"delivery":  map[string]any{"mode": "webhook", "url": embeddedEventsURL, "secret": embeddedEventsSecret},
		"no_expiry": true,
	})
	require.Equal(t, http.StatusOK, status, string(raw))
	var result struct {
		RefreshBefore *time.Time `json:"refresh_before"`
	}
	require.NoError(t, json.Unmarshal(raw, &result))
	require.NotNil(t, result.RefreshBefore, "a refused no-expiry request receives a finite grant")
	assert.WithinRange(t, *result.RefreshBefore, before.Add(2*time.Hour), time.Now().Add(2*time.Hour))
}

func TestServiceRejectsAnMCPEventsMaxTTLBelowTheMinimumGrant(t *testing.T) {
	for _, maxTTL := range []time.Duration{30 * time.Second, -time.Hour} {
		t.Run(maxTTL.String(), func(t *testing.T) {
			_, err := newService(t.Context(), Config{
				DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{Token: embeddedEventsToken},
				MCPEvents: MCPEventsConfig{Enabled: true, Key: &memoryEventsKey{}, MaxTTL: maxTTL},
			}, serviceDeps{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "MCPEvents.MaxTTL")
		})
	}
}
