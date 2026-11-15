package daemon_test

import (
	"context"
	"crypto/x509"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
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

func TestMCPEventsPrincipalFromContext(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct {
		name string
		p    daemon.Principal
		want mcpevents.Principal
	}{
		{"db token", daemon.Principal{Kind: daemon.PrincipalDBToken, TokenID: 42, Actor: "example-user"},
			mcpevents.Principal{Authority: "db_token", Subject: "42", Actor: "example-user"}},
		{"static token", daemon.Principal{Kind: daemon.PrincipalStaticToken, Subject: "credential-fingerprint"},
			mcpevents.Principal{Authority: "static_token", Subject: "credential-fingerprint"}},
		{"host keeps the exact subject bytes", daemon.Principal{Kind: daemon.PrincipalHost, Subject: " Tenant/One ", Actor: "example-user"},
			mcpevents.Principal{Authority: "host", Subject: " Tenant/One ", Actor: "example-user"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := daemon.MCPEventsPrincipalFromContext(daemon.WithPrincipal(ctx, tc.p))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
	for _, p := range []daemon.Principal{
		{Kind: daemon.PrincipalBootstrap},
		{Kind: daemon.PrincipalStaticToken},
		{Kind: daemon.PrincipalTrustedProxy, Actor: "example-user"},
		{Kind: daemon.PrincipalTrustedProxyAbsent},
		{Kind: daemon.PrincipalWebLocal},
		{Kind: daemon.PrincipalDBToken, Actor: "example-user"}, // no token id
		{Kind: daemon.PrincipalHost, Subject: " ", Actor: "example-user"},
		{Kind: daemon.PrincipalHost, Subject: "tenant"}, // no actor
	} {
		_, err := daemon.MCPEventsPrincipalFromContext(daemon.WithPrincipal(ctx, p))
		assert.ErrorIs(t, err, mcpevents.ErrForbidden, string(p.Kind))
	}
	_, err := daemon.MCPEventsPrincipalFromContext(ctx)
	assert.ErrorIs(t, err, mcpevents.ErrForbidden, "a request with no principal and no owner authority")

	a, _ := daemon.MCPEventsPrincipalFromContext(daemon.WithPrincipal(ctx, daemon.Principal{Kind: daemon.PrincipalHost, Subject: "tenant-1", Actor: "shared"}))
	b, _ := daemon.MCPEventsPrincipalFromContext(daemon.WithPrincipal(ctx, daemon.Principal{Kind: daemon.PrincipalHost, Subject: "tenant-2", Actor: "shared"}))
	assert.NotEqual(t, a.Key(), b.Key())
}

type hostAccess struct {
	err        error
	revalidate func(context.Context) error
	requests   []daemon.HostAccessRequest
}

func (h *hostAccess) Authorize(_ context.Context, req daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	h.requests = append(h.requests, req)
	return daemon.HostAccessDecision{Revalidate: h.revalidate}, h.err
}

func TestMCPEventsReacquire(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		future := time.Now().UTC().Add(time.Hour)
		live, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "live-token", Actor: "example-user", AdminActor: db.BootstrapActor})
		require.NoError(t, err)
		revoked, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "revoked-token", Actor: "example-user", AdminActor: db.BootstrapActor})
		require.NoError(t, err)
		_, _, err = store.RevokeAPIToken(ctx, revoked.ID, db.BootstrapActor)
		require.NoError(t, err)
		expiring, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "expiring-token", Actor: "example-user", AdminActor: db.BootstrapActor, ExpiresAt: &future}) //nolint:gosec // Deterministic fixture credential, never an operational secret.
		require.NoError(t, err)

		dbPrincipal := func(id int64) mcpevents.Principal {
			return mcpevents.Principal{Authority: mcpevents.AuthorityDBToken, Subject: itoa(id), Actor: "example-user"}
		}
		identity := func(cfg daemon.ServerConfig) mcpevents.Authority {
			cfg.DB = store
			return daemon.NewMCPEventsAuthority(cfg)
		}
		tokenCfg := daemon.ServerConfig{Auth: config.AuthConfig{Token: "configured", RequireTokenIdentity: true}}

		t.Run("db token", func(t *testing.T) {
			auth := identity(tokenCfg)
			_, err := auth.Reacquire(ctx, dbPrincipal(live.ID))
			require.NoError(t, err)
			_, err = auth.Reacquire(ctx, dbPrincipal(expiring.ID))
			require.NoError(t, err)
			for _, p := range []mcpevents.Principal{
				dbPrincipal(revoked.ID), dbPrincipal(live.ID + 1000),
				{Authority: mcpevents.AuthorityDBToken, Subject: "not-a-number"},
				{Authority: mcpevents.AuthorityDBToken, Subject: "0"},
				{Authority: "bootstrap", Subject: "x"},
			} {
				_, err := auth.Reacquire(ctx, p)
				assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked, p.Key())
			}
		})
		t.Run("a db token needs identity mode", func(t *testing.T) {
			// Outside identity mode database tokens no longer authenticate, the
			// counterpart of a static token downgraded to bootstrap.
			for _, cfg := range []daemon.ServerConfig{{}, {Auth: config.AuthConfig{Token: "configured"}}} {
				_, err := identity(cfg).Reacquire(ctx, dbPrincipal(live.ID))
				assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked)
			}
		})
		t.Run("static token follows the authentication mode", func(t *testing.T) {
			static := mcpevents.Principal{Authority: mcpevents.AuthorityStaticToken, Subject: "20158224750041d653cd8c737d64c2fdce258d4280c9bf2a49fa1db0a7f9f3ac"}
			_, err := identity(daemon.ServerConfig{Auth: config.AuthConfig{Token: "configured"}}).Reacquire(ctx, static)
			require.NoError(t, err)
			// Identity mode resolves the configured token to bootstrap, which
			// cannot subscribe.
			_, err = identity(tokenCfg).Reacquire(ctx, static)
			assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked)
			_, err = identity(daemon.ServerConfig{}).Reacquire(ctx, static)
			assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked, "no configured token")
		})
		t.Run("local follows the local mode", func(t *testing.T) {
			local := mcpevents.Principal{Authority: mcpevents.AuthorityLocal, Subject: "local"}
			_, err := identity(daemon.ServerConfig{}).Reacquire(ctx, local)
			require.NoError(t, err)
			_, err = identity(daemon.ServerConfig{Auth: config.AuthConfig{Token: "configured"}}).Reacquire(ctx, local)
			assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked)
			_, err = identity(daemon.ServerConfig{HostAccess: &hostAccess{}}).Reacquire(ctx, local)
			assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked)
		})
		t.Run("host asks the controller with the persisted subject and actor", func(t *testing.T) {
			host := mcpevents.Principal{Authority: mcpevents.AuthorityHost, Subject: "tenant-1", Actor: "example-user"}
			_, err := identity(daemon.ServerConfig{}).Reacquire(ctx, host)
			require.ErrorIs(t, err, mcpevents.ErrAuthorityUnavailable, "a missing controller fails closed")
			assert.NotErrorIs(t, err, mcpevents.ErrPrincipalRevoked)

			controller := &hostAccess{}
			_, err = identity(daemon.ServerConfig{HostAccess: controller}).Reacquire(ctx, host)
			require.NoError(t, err)
			require.Equal(t, []daemon.HostAccessRequest{{
				Subject: host.Subject, Actor: host.Actor,
				Operation: daemon.HostOperation{
					ID: "pollEvents", Method: http.MethodGet, Path: "/api/v1/events", AllProjects: true,
					Policy: daemon.HostOperationPolicy{Kind: "task_read", Capability: "read"},
				},
			}}, controller.requests)

			controller.err = daemon.ErrHostAccessDenied
			_, err = identity(daemon.ServerConfig{HostAccess: controller}).Reacquire(ctx, host)
			assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked)

			controller.err = errors.New("controller offline")
			_, err = identity(daemon.ServerConfig{HostAccess: controller}).Reacquire(ctx, host)
			require.ErrorIs(t, err, mcpevents.ErrAuthorityUnavailable)
			assert.NotErrorIs(t, err, mcpevents.ErrPrincipalRevoked)

			_, err = identity(daemon.ServerConfig{HostAccess: controller}).Reacquire(ctx, mcpevents.Principal{Authority: mcpevents.AuthorityHost, Subject: "tenant-1"})
			assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked, "a host principal needs an actor")
		})
		t.Run("a later host revocation is noticed on use", func(t *testing.T) {
			revoked := false
			controller := &hostAccess{revalidate: func(context.Context) error {
				if revoked {
					return daemon.ErrHostAccessDenied
				}
				return nil
			}}
			host := mcpevents.Principal{Authority: mcpevents.AuthorityHost, Subject: "tenant-1", Actor: "example-user"}
			authorized, err := identity(daemon.ServerConfig{HostAccess: controller}).Reacquire(ctx, host)
			require.NoError(t, err)
			_, err = authorized.VisibleProjects(ctx, nil)
			require.NoError(t, err)
			revoked = true
			_, err = authorized.VisibleProjects(ctx, nil)
			assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked)
		})
		t.Run("a temporary host outage remains retryable on use", func(t *testing.T) {
			var leaseErr error
			controller := &hostAccess{revalidate: func(context.Context) error { return leaseErr }}
			host := mcpevents.Principal{Authority: mcpevents.AuthorityHost, Subject: "tenant-1", Actor: "example-user"}
			authorized, err := identity(daemon.ServerConfig{HostAccess: controller}).Reacquire(ctx, host)
			require.NoError(t, err)
			_, err = authorized.VisibleProjects(ctx, nil)
			require.NoError(t, err)

			leaseErr = errors.New("controller offline")
			_, err = authorized.VisibleProjects(ctx, nil)
			require.ErrorIs(t, err, mcpevents.ErrAuthorityUnavailable)
			assert.NotErrorIs(t, err, mcpevents.ErrPrincipalRevoked)

			leaseErr = nil
			_, err = authorized.VisibleProjects(ctx, nil)
			require.NoError(t, err, "the same authorization remains usable after recovery")
		})
		t.Run("missing context never allows", func(t *testing.T) {
			_, err := daemon.NewMCPEventsAuthority(daemon.ServerConfig{}).Reacquire(ctx, mcpevents.Principal{Authority: mcpevents.AuthorityLocal, Subject: "local"})
			assert.ErrorIs(t, err, mcpevents.ErrAuthorityUnavailable, "no store")
		})
	})
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// eventsFixture has a public and a team-only project, each with an issue, and
// principals with different views of them.
type eventsFixture struct {
	store          db.Storage
	teamUID        string
	public, hidden db.Project
	publicIssue    db.Issue
	hiddenIssue    db.Issue
	member         mcpevents.Principal
	outsider       mcpevents.Principal
	scoped         mcpevents.Principal
	childIssue     db.Issue
	authority      mcpevents.Authority
}

func newEventsFixture(t *testing.T, store db.Storage) *eventsFixture {
	t.Helper()
	ctx := t.Context()
	f := &eventsFixture{store: store}
	var err error
	f.public, err = store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	f.hidden, err = store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	team, _, err := store.CreateTeam(ctx, "project-team", db.BootstrapActor)
	require.NoError(t, err)
	f.teamUID = team.UID
	_, err = store.SetTeamMembership(ctx, team.UID, "member-actor", true, db.BootstrapActor)
	require.NoError(t, err)
	_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: f.hidden.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, db.BootstrapActor)
	require.NoError(t, err)

	key := notification.MetadataKey("example-user")
	notify := map[string]jsontext.Value{key: jsontext.Value(`{"from":"example-user","message":"work"}`)}
	f.publicIssue, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "public", Author: "coordinator", Metadata: notify})
	require.NoError(t, err)
	f.hiddenIssue, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.hidden.ID, Title: "hidden", Author: "coordinator", Metadata: notify})
	require.NoError(t, err)
	f.childIssue, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "child", Author: "coordinator", Links: []db.InitialLink{{Type: "parent", ToNumber: f.publicIssue.ID}}})
	require.NoError(t, err)

	token := func(plain, actor string, scope *db.APITokenScope) mcpevents.Principal {
		in := db.CreateAPITokenParams{PlaintextToken: plain, Actor: actor, AdminActor: db.BootstrapActor, Scope: scope}
		if scope != nil {
			expiry := time.Now().UTC().Add(time.Hour)
			in.ExpiresAt = &expiry
		}
		created, _, err := store.CreateAPIToken(ctx, in)
		require.NoError(t, err)
		return mcpevents.Principal{Authority: mcpevents.AuthorityDBToken, Subject: itoa(created.ID), Actor: actor}
	}
	f.member = token("member-token", "member-actor", nil)
	f.outsider = token("outsider-token", "outsider-actor", nil)
	f.scoped = token("scoped-token", "scoped-actor", &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.public.UID, RootIssueUID: f.publicIssue.UID})
	f.authority = daemon.NewMCPEventsAuthority(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "configured", RequireTokenIdentity: true}})
	return f
}

func (f *eventsFixture) as(t *testing.T, p mcpevents.Principal) mcpevents.Authorized {
	t.Helper()
	authorized, err := f.authority.Reacquire(t.Context(), p)
	require.NoError(t, err)
	return authorized
}

func projectUIDs(projects []mcpevents.Project) []string {
	out := make([]string, 0, len(projects))
	for _, p := range projects {
		out = append(out, p.UID)
	}
	return out
}

func TestMCPEventsAuthorizedVisibleProjects(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		both := []string{f.public.UID, f.hidden.UID}
		if both[0] > both[1] {
			both[0], both[1] = both[1], both[0]
		}

		visible, err := f.as(t, f.member).VisibleProjects(ctx, nil)
		require.NoError(t, err)
		assert.Equal(t, both, projectUIDs(visible), "a team member sees both, sorted by UID")

		visible, err = f.as(t, f.outsider).VisibleProjects(ctx, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{f.public.UID}, projectUIDs(visible), "project policy hides the team project")

		visible, err = f.as(t, f.outsider).VisibleProjects(ctx, []string{f.hidden.UID})
		require.NoError(t, err)
		assert.Empty(t, visible, "an explicit selection cannot widen visibility")

		visible, err = f.as(t, f.member).VisibleProjects(ctx, []string{f.hidden.UID})
		require.NoError(t, err)
		assert.Equal(t, []string{f.hidden.UID}, projectUIDs(visible))

		visible, err = f.as(t, f.scoped).VisibleProjects(ctx, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{f.public.UID}, projectUIDs(visible), "a scoped credential sees only its project")
		visible, err = f.as(t, f.scoped).VisibleProjects(ctx, []string{f.hidden.UID})
		require.NoError(t, err)
		assert.Empty(t, visible)

		// The static-token and owner-local principals see every project.
		for _, cfg := range []struct {
			cfg daemon.ServerConfig
			p   mcpevents.Principal
		}{
			{daemon.ServerConfig{Auth: config.AuthConfig{Token: "configured"}}, mcpevents.Principal{Authority: mcpevents.AuthorityStaticToken, Subject: "20158224750041d653cd8c737d64c2fdce258d4280c9bf2a49fa1db0a7f9f3ac"}},
			{daemon.ServerConfig{}, mcpevents.Principal{Authority: mcpevents.AuthorityLocal, Subject: "local"}},
		} {
			cfg.cfg.DB = store
			authorized, err := daemon.NewMCPEventsAuthority(cfg.cfg).Reacquire(ctx, cfg.p)
			require.NoError(t, err)
			visible, err = authorized.VisibleProjects(ctx, nil)
			require.NoError(t, err)
			assert.Equal(t, both, projectUIDs(visible), cfg.p.Key())
		}

		// Archiving a project removes it from the catalog.
		_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: f.public.ID, Actor: db.BootstrapActor, Force: true})
		require.NoError(t, err)
		visible, err = f.as(t, f.outsider).VisibleProjects(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, visible)
	})
}

func TestMCPEventsAuthorizeOccurrence(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		for _, tc := range []struct {
			name   string
			who    mcpevents.Principal
			o      mcpevents.Occurrence
			hidden bool
		}{
			{"member sees the team issue", f.member, mcpevents.Occurrence{ProjectUID: f.hidden.UID, IssueUID: f.hiddenIssue.UID}, false},
			{"outsider cannot see the team issue", f.outsider, mcpevents.Occurrence{ProjectUID: f.hidden.UID, IssueUID: f.hiddenIssue.UID}, true},
			{"outsider cannot see the team project", f.outsider, mcpevents.Occurrence{ProjectUID: f.hidden.UID}, true},
			{"outsider cannot see the issue by uid alone", f.outsider, mcpevents.Occurrence{IssueUID: f.hiddenIssue.UID}, true},
			{"outsider sees the public issue", f.outsider, mcpevents.Occurrence{ProjectUID: f.public.UID, IssueUID: f.publicIssue.UID}, false},
			{"unknown issue", f.outsider, mcpevents.Occurrence{IssueUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"}, true},
			{"unknown project", f.member, mcpevents.Occurrence{ProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"}, true},
			{"scoped sees its subtree", f.scoped, mcpevents.Occurrence{ProjectUID: f.public.UID, IssueUID: f.childIssue.UID}, false},
			{"scoped sees its root", f.scoped, mcpevents.Occurrence{ProjectUID: f.public.UID, IssueUID: f.publicIssue.UID}, false},
			{"scoped cannot see another project", f.scoped, mcpevents.Occurrence{ProjectUID: f.hidden.UID}, true},
			{"scoped cannot see an issue outside the subtree", f.scoped, mcpevents.Occurrence{ProjectUID: f.hidden.UID, IssueUID: f.hiddenIssue.UID}, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				err := f.as(t, tc.who).AuthorizeOccurrence(ctx, tc.o)
				if tc.hidden {
					assert.ErrorIs(t, err, mcpevents.ErrOccurrenceHidden)
				} else {
					assert.NoError(t, err)
				}
			})
		}
		// Moving the issue out of the scoped subtree hides it from the credential.
		sibling, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "sibling", Author: "coordinator"})
		require.NoError(t, err)
		err = f.as(t, f.scoped).AuthorizeOccurrence(ctx, mcpevents.Occurrence{ProjectUID: f.public.UID, IssueUID: sibling.UID})
		assert.ErrorIs(t, err, mcpevents.ErrOccurrenceHidden)

		// Revoking a scoped credential is noticed on the next use.
		scoped := f.as(t, f.scoped)
		_, _, err = store.RevokeAPIToken(ctx, tokenID(t, f.scoped), db.BootstrapActor)
		require.NoError(t, err)
		_, err = scoped.VisibleProjects(ctx, nil)
		assert.ErrorIs(t, err, mcpevents.ErrPrincipalRevoked)
	})
}

func tokenID(t *testing.T, p mcpevents.Principal) int64 {
	t.Helper()
	id, err := strconv.ParseInt(p.Subject, 10, 64)
	require.NoError(t, err)
	return id
}

// serviceFixture wires a real Service over the daemon Authority and a TLS
// receiver, so subscriptions are authorized by the daemon's own decisions.
func newDaemonService(t *testing.T, store db.Storage, f *eventsFixture, onDelivery ...func(string)) (*mcpevents.Service, []byte) {
	t.Helper()
	return newDaemonServiceReceiving(t, store, f, func(raw []byte) {
		var body struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &body)
		for _, notify := range onDelivery {
			notify(body.Type)
		}
	})
}

// newDaemonServiceReceiving hands every non-challenge request body to receive.
func newDaemonServiceReceiving(t *testing.T, store db.Storage, f *eventsFixture, receive func([]byte)) (*mcpevents.Service, []byte) {
	t.Helper()
	return newDaemonServiceWithKeys(t, store, f, mcpevents.NewFileKeyProvider(filepath.Join(t.TempDir(), "mcp-events.key")),
		func(raw []byte) int { receive(raw); return http.StatusOK })
}

// newDaemonServiceWithKeys is newDaemonServiceReceiving with a caller-owned
// key file, so a second Service can stand in for a restarted daemon. respond
// sees every non-challenge body and picks the receiver's status; mutate
// adjusts the service configuration.
func newDaemonServiceWithKeys(t *testing.T, store db.Storage, f *eventsFixture, keys mcpevents.KeyProvider, respond func([]byte) int, mutate ...func(*mcpevents.ServiceConfig)) (*mcpevents.Service, []byte) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Challenge string `json:"challenge"`
		}
		_ = json.Unmarshal(raw, &body)
		if body.Challenge == "" {
			w.WriteHeader(respond(raw))
			return
		}
		_ = json.MarshalWrite(w, map[string]string{"challenge": body.Challenge})
	}))
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	webhook := mcpevents.NewWebhookClient(mcpevents.WebhookOptions{
		Resolve: func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		},
		RootCAs: roots,
	})
	cfg := mcpevents.ServiceConfig{Store: store, Keys: keys, Authority: f.authority, Webhook: webhook}
	for _, m := range mutate {
		m(&cfg)
	}
	service, err := mcpevents.NewService(t.Context(), cfg)
	require.NoError(t, err)
	require.True(t, service.Availability().Enabled, service.Availability().Reason)
	key, err := keys.Load(t.Context())
	require.NoError(t, err)
	return service, key
}

// pingRecorder counts the pings and terminated notices a receiver gets, and
// keeps the eventId of every ping attempt in order.
type pingRecorder struct {
	pings, terminated atomic.Int64
	mu                sync.Mutex
	recipientsSeen    []string
	attempts          []string
	// status, when set, answers each ping attempt; nil answers 200.
	status func(eventID string) int
}

func (r *pingRecorder) receive(raw []byte) { r.respond(raw) }

func (r *pingRecorder) respond(raw []byte) int {
	var body struct {
		Type string `json:"type"`
		mcpevents.Envelope
	}
	if json.Unmarshal(raw, &body) != nil {
		return http.StatusOK
	}
	switch {
	case body.Type == "terminated":
		r.terminated.Add(1)
	case body.Name == mcpevents.EventName:
		r.mu.Lock()
		r.attempts = append(r.attempts, body.EventID)
		status := r.status
		r.mu.Unlock()
		if status != nil {
			if code := status(body.EventID); code != http.StatusOK {
				return code
			}
		}
		r.pings.Add(1)
		r.mu.Lock()
		r.recipientsSeen = append(r.recipientsSeen, body.Data.Recipient)
		r.mu.Unlock()
	}
	return http.StatusOK
}

func (r *pingRecorder) pingAttempts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.attempts...)
}

func (r *pingRecorder) setStatus(status func(eventID string) int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

// eventsClock is a settable service clock.
type eventsClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *eventsClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *eventsClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func (r *pingRecorder) recipients() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.recipientsSeen...)
}

func subscribeEvents(t *testing.T, service *mcpevents.Service, p mcpevents.Principal, args mcpevents.Arguments) mcpevents.SubscribeResult {
	t.Helper()
	result, err := service.Subscribe(t.Context(), p, mcpevents.ForwarderScope{All: true}, mcpevents.SubscribeRequest{
		Name: mcpevents.EventName, Arguments: args,
		Delivery: mcpevents.Delivery{Mode: "webhook", URL: "https://example.com/events", Secret: "whsec_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
	})
	require.NoError(t, err)
	return result
}

// notifyIssue creates an issue that asks example-user for work.
func notifyIssue(t *testing.T, store db.Storage, projectID int64, title string) db.Issue {
	t.Helper()
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projectID, Title: title, Author: "coordinator",
		Metadata: map[string]jsontext.Value{notification.MetadataKey("example-user"): jsontext.Value(`{"from":"coordinator","message":"work"}`)}})
	require.NoError(t, err)
	return issue
}

func TestMCPEventsServiceUsesTheDaemonAuthority(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		service, _ := newDaemonService(t, store, f)
		ctx := t.Context()
		secret := "whsec_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
		request := func(uids ...string) mcpevents.SubscribeRequest {
			args := mcpevents.Arguments{For: "example-user"}
			if len(uids) > 0 {
				args.ProjectUIDs = uids
			}
			return mcpevents.SubscribeRequest{
				Name: mcpevents.EventName, Arguments: args,
				Delivery: mcpevents.Delivery{Mode: "webhook", URL: "https://example.com/events?route=inbox", Secret: secret},
			}
		}
		all := mcpevents.ForwarderScope{All: true}

		_, err := service.Subscribe(ctx, f.member, all, request())
		require.NoError(t, err)

		// An outsider cannot subscribe to a project policy hides from them.
		_, err = service.Subscribe(ctx, f.outsider, all, request(f.hidden.UID))
		var wire *mcpevents.Error
		require.ErrorAs(t, err, &wire)
		assert.Equal(t, mcpevents.CodeForbidden, wire.Code)
		assert.Equal(t, "scope_removed", wire.Reason)

		// A dynamic subscription for the same outsider is fine and sees no hidden project.
		_, err = service.Subscribe(ctx, f.outsider, all, request())
		require.NoError(t, err)

		// A scoped credential cannot name an issue outside its subtree.
		scopedRequest := request()
		scopedRequest.Arguments.IssueUID = f.hiddenIssue.UID
		_, err = service.Subscribe(ctx, f.scoped, all, scopedRequest)
		require.ErrorAs(t, err, &wire)
		assert.Equal(t, "issue_not_visible", wire.Reason)

		// Revoking the token ends the principal's authority.
		_, _, err = store.RevokeAPIToken(ctx, tokenID(t, f.member), db.BootstrapActor)
		require.NoError(t, err)
		_, err = service.Subscribe(ctx, f.member, all, request())
		require.ErrorAs(t, err, &wire)
		assert.Equal(t, mcpevents.CodeForbidden, wire.Code)
		assert.Equal(t, "principal_revoked", wire.Reason)
		statuses, err := service.Status(ctx, f.member)
		require.NoError(t, err)
		require.Len(t, statuses, 1)
		assert.Equal(t, "ended", statuses[0].State)
		assert.Equal(t, "principal_revoked", statuses[0].EndReason)
		assert.Equal(t, "terminated", statuses[0].PendingControl)
	})
}

func sortStrings(s []string) { sort.Strings(s) }

// authorizationBarrier inserts a committed access change between real daemon
// authorization operations, without timing or scheduler assumptions.
type authorizationBarrier struct {
	mcpevents.Authority
	beforeOccurrence func()
	afterOccurrence  func()
}

func (a *authorizationBarrier) Reacquire(ctx context.Context, p mcpevents.Principal) (mcpevents.Authorized, error) {
	auth, err := a.Authority.Reacquire(ctx, p)
	if err != nil {
		return nil, err
	}
	return &barrierAuthorized{Authorized: auth, barrier: a}, nil
}

type barrierAuthorized struct {
	mcpevents.Authorized
	barrier *authorizationBarrier
}

func (a *barrierAuthorized) AuthorizeOccurrence(ctx context.Context, o mcpevents.Occurrence) error {
	if hook := a.barrier.beforeOccurrence; hook != nil {
		a.barrier.beforeOccurrence = nil
		hook()
	}
	err := a.Authorized.AuthorizeOccurrence(ctx, o)
	if hook := a.barrier.afterOccurrence; hook != nil {
		a.barrier.afterOccurrence = nil
		hook()
	}
	return err
}

// A project grant is narrower than the credential that owns the subscription.
func TestMCPEventsProjectAccessNarrowsWithoutRevokingPrincipal(t *testing.T) {
	for _, selection := range []string{"remaining", "removed", "dynamic-empty"} {
		t.Run(selection, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				f := newEventsFixture(t, store)
				ctx := t.Context()
				args := mcpevents.Arguments{For: "example-user"}
				switch selection {
				case "remaining":
					args.ProjectUIDs = []string{f.public.UID}
				case "removed":
					args.ProjectUIDs = []string{f.hidden.UID}
				case "dynamic-empty":
					// Make every visible project depend on the membership below.
					projects, err := store.ListProjects(ctx)
					require.NoError(t, err)
					for _, project := range projects {
						_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{f.teamUID}}, db.BootstrapActor)
						require.NoError(t, err)
					}
				}
				authorized := f.as(t, f.member)
				before, err := mcpevents.ResolveProjects(ctx, authorized, args)
				require.NoError(t, err)
				require.NotEmpty(t, before)
				_, err = store.SetTeamMembership(ctx, f.teamUID, "member-actor", false, db.BootstrapActor)
				require.NoError(t, err)

				after, err := mcpevents.ResolveProjects(ctx, authorized, args)
				assert.NotErrorIs(t, err, mcpevents.ErrPrincipalRevoked)
				if selection == "removed" {
					require.ErrorIs(t, err, mcpevents.ErrScopeRemoved)
				} else {
					require.NoError(t, err)
					if selection == "remaining" {
						assert.Equal(t, []string{f.public.UID}, projectUIDs(after))
					} else {
						assert.Empty(t, after)
					}
				}
				// The same instance rechecks live access.
				assert.ErrorIs(t, authorized.AuthorizeOccurrence(ctx, mcpevents.Occurrence{ProjectUID: f.hidden.UID, IssueUID: f.hiddenIssue.UID}), mcpevents.ErrOccurrenceHidden)
			})
		})
	}
}

// Losing the only selected project ends the subscription with a terminated
// notice. A later move out of that project, which the owner can no longer
// see, sends no ping.
func TestMCPEventsLostSelectionEndsWithoutAPing(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		result := subscribeEvents(t, service, f.member, mcpevents.Arguments{For: "example-user", ProjectUIDs: []string{f.hidden.UID}})
		require.NoError(t, service.Drain(ctx))

		_, err := store.SetTeamMembership(ctx, f.teamUID, "member-actor", false, db.BootstrapActor)
		require.NoError(t, err)
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: f.hiddenIssue.ID, FromProjectID: f.hidden.ID, ToProjectID: f.public.ID, IfMatchRev: f.hiddenIssue.Revision, Actor: db.BootstrapActor})
		require.NoError(t, err)
		service.Recheck()
		require.NoError(t, service.Drain(ctx))

		row, err := store.GetMCPEventSubscription(ctx, result.ID)
		require.NoError(t, err)
		assert.Equal(t, db.MCPEventStateEnded, row.State)
		assert.Equal(t, "scope_removed", row.EndReason)
		assert.Zero(t, got.pings.Load())
		assert.Equal(t, int64(1), got.terminated.Load())
	})
}

// A token revoked between the scan's authorization steps ends the
// subscription with a terminated notice and sends no ping. Membership loss is
// not revocation; TestMCPEventsProjectAccessNarrowsWithoutRevokingPrincipal
// covers it, and the membership change pings on its own.
func TestMCPEventsTokenRevokedBetweenAuthorizationOperations(t *testing.T) {
	for _, phase := range []string{"before_occurrence", "before_delivery"} {
		t.Run(phase, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				f := newEventsFixture(t, store)
				ctx := t.Context()
				barrier := &authorizationBarrier{Authority: f.authority}
				f.authority = barrier
				var got pingRecorder
				service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
				result := subscribeEvents(t, service, f.member, mcpevents.Arguments{For: "example-user"})
				require.NoError(t, service.Drain(ctx))
				notifyIssue(t, store, f.hidden.ID, "restricted notification")
				revoke := func() {
					_, _, err := store.RevokeAPIToken(ctx, tokenID(t, f.member), db.BootstrapActor)
					require.NoError(t, err)
				}
				if phase == "before_occurrence" {
					barrier.beforeOccurrence = revoke
				} else {
					barrier.afterOccurrence = revoke
				}
				service.Recheck()
				require.NoError(t, service.Drain(ctx))
				assert.Nil(t, barrier.beforeOccurrence)
				assert.Nil(t, barrier.afterOccurrence)
				row, err := store.GetMCPEventSubscription(ctx, result.ID)
				require.NoError(t, err)
				assert.Equal(t, db.MCPEventStateEnded, row.State)
				assert.Equal(t, "principal_revoked", row.EndReason)
				assert.Equal(t, int64(1), got.terminated.Load(), "the termination control is delivered")
				assert.Zero(t, got.pings.Load(), "a revoked owner gets no ping")
			})
		})
	}
}

// A ping waiting for its retry rechecks the stored selection before it is
// sent again. The pending ping comes from a real failed delivery.
func TestMCPEventsPendingPingRechecksProjectSelection(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		t.Run(strconv.FormatBool(explicit), func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				f := newEventsFixture(t, store)
				ctx := t.Context()
				clock := &eventsClock{at: time.Now().UTC()}
				got := &pingRecorder{}
				got.setStatus(func(string) int { return http.StatusServiceUnavailable })
				service, _ := newDaemonServiceWithKeys(t, store, f, mcpevents.NewFileKeyProvider(filepath.Join(t.TempDir(), "mcp-events.key")),
					got.respond, func(cfg *mcpevents.ServiceConfig) { cfg.Now = clock.now })
				args := mcpevents.Arguments{For: "example-user"}
				if explicit {
					args.ProjectUIDs = []string{f.public.UID}
				}
				result := subscribeEvents(t, service, f.member, args)
				notifyIssue(t, store, f.public.ID, "fails once")
				require.NoError(t, service.Drain(ctx))
				failed := got.pingAttempts()
				require.Len(t, failed, 1, "the first attempt failed")
				row, err := store.GetMCPEventSubscription(ctx, result.ID)
				require.NoError(t, err)
				require.Equal(t, db.MCPEventPendingEvent, row.PendingKind)
				require.Equal(t, failed[0], row.PendingMessageID)

				got.setStatus(nil)
				_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: f.public.ID, Actor: db.BootstrapActor, Force: true})
				require.NoError(t, err)
				clock.advance(time.Minute)
				require.NoError(t, service.Drain(ctx))

				row, err = store.GetMCPEventSubscription(ctx, result.ID)
				require.NoError(t, err)
				attempts := got.pingAttempts()
				if explicit {
					assert.Equal(t, db.MCPEventStateEnded, row.State)
					assert.Equal(t, "scope_removed", row.EndReason)
					assert.Equal(t, failed, attempts, "the pending ping is never retried")
					assert.Equal(t, int64(1), got.terminated.Load())
				} else {
					assert.Equal(t, db.MCPEventStateActive, row.State)
					require.GreaterOrEqual(t, len(attempts), 2)
					assert.Equal(t, failed[0], attempts[1], "the dynamic selection keeps its grant and retries the same ping")
					assert.Zero(t, got.terminated.Load())
				}
			})
		})
	}
}

// Closing, deleting or restoring an issue changes a recipient's inbox only
// when the issue carries that recipient's request.
func TestMCPEventsIssueLifecycleWakesOnlyTheRequestedRecipient(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		subscribeEvents(t, service, f.member, mcpevents.Arguments{For: "example-user", ProjectUIDs: []string{f.public.UID}})
		require.NoError(t, service.Drain(ctx))

		// childIssue carries no request; publicIssue asks example-user for work.
		deleteAndRestore := func(issue db.Issue) {
			_, _, _, err := store.SoftDeleteIssue(ctx, issue.ID, "coordinator")
			require.NoError(t, err)
			_, _, _, err = store.RestoreIssue(ctx, issue.ID, "coordinator")
			require.NoError(t, err)
			require.NoError(t, service.Drain(ctx))
		}
		deleteAndRestore(f.childIssue)
		assert.Zero(t, got.pings.Load(), "an issue without the recipient's request")
		deleteAndRestore(f.publicIssue)
		assert.Equal(t, int64(1), got.pings.Load(), "the delete and the restore coalesce into one ping")
		assert.Equal(t, []string{"example-user"}, got.recipients())
	})
}

// countingEventsStore counts the writes the Events worker makes.
type countingEventsStore struct {
	db.Storage
	writes atomic.Int64
}

func (s *countingEventsStore) ClaimMCPEventSubscription(ctx context.Context, in db.MCPEventClaim) (bool, error) {
	s.writes.Add(1)
	return s.Storage.ClaimMCPEventSubscription(ctx, in)
}

func (s *countingEventsStore) UpdateMCPEventDelivery(ctx context.Context, in db.MCPEventDeliveryUpdate) (bool, error) {
	s.writes.Add(1)
	return s.Storage.UpdateMCPEventDelivery(ctx, in)
}

func (s *countingEventsStore) MarkMCPEventSubscriptionsDirty(ctx context.Context, ids []string) error {
	if len(ids) > 0 {
		s.writes.Add(1)
	}
	return s.Storage.MarkMCPEventSubscriptionsDirty(ctx, ids)
}

// Archive activity that cannot change a subscription's inbox costs the
// worker no subscription writes, while a real request is still delivered.
func TestMCPEventsUnrelatedArchiveActivityCostsNoWrites(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		counted := &countingEventsStore{Storage: store}
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, counted, f, got.receive)
		subscribeEvents(t, service, f.member, mcpevents.Arguments{For: "example-user", ProjectUIDs: []string{f.public.UID}})
		require.NoError(t, service.Drain(ctx))
		counted.writes.Store(0)

		// A comment cannot change an inbox, and the other project is not selected.
		_, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: f.publicIssue.ID, Author: "coordinator", Body: "progress note"})
		require.NoError(t, err)
		_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.hidden.ID, Title: "elsewhere", Author: "coordinator",
			Metadata: map[string]jsontext.Value{notification.MetadataKey("example-user"): jsontext.Value(`{"from":"coordinator","message":"other project"}`)}})
		require.NoError(t, err)
		require.NoError(t, service.Drain(ctx))
		assert.Zero(t, counted.writes.Load(), "no subscription write for activity that cannot match")
		assert.Zero(t, got.pings.Load())

		notifyIssue(t, store, f.public.ID, "review")
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, int64(1), got.pings.Load(), "a request in the selected project is delivered")
	})
}

// A purge reserves its reset position without an event row, and the events
// it deleted cannot be matched any more, so every subscriber reloads. An
// unrelated event committed after it must not hide it.
func TestMCPEventsPurgeBehindUnrelatedActivityStillPings(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newEventsFixture(t, store)
		ctx := t.Context()
		var got pingRecorder
		service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
		subscribeEvents(t, service, f.member, mcpevents.Arguments{For: "example-user", ProjectUIDs: []string{f.public.UID}})
		require.NoError(t, service.Drain(ctx))

		_, err := store.PurgeIssue(ctx, f.childIssue.ID, "admin", nil)
		require.NoError(t, err)
		_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: f.publicIssue.ID, Author: "coordinator", Body: "progress note"})
		require.NoError(t, err)
		require.NoError(t, service.Drain(ctx))
		assert.Equal(t, int64(1), got.pings.Load())
	})
}

// Restoring a deleted parent inside a scoped credential's subtree brings its
// descendants back into scope. Neither an unfiltered subscription nor one
// filtered to the grandchild sees an event on the grandchild itself, so the
// restore of the parent must wake both.
func TestMCPEventsScopedRestoreOfAnIntermediateParentWakes(t *testing.T) {
	for _, filtered := range []bool{false, true} {
		t.Run("issue filter "+strconv.FormatBool(filtered), func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				f := newEventsFixture(t, store)
				ctx := t.Context()
				grandchild, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "grandchild", Author: "coordinator",
					Links:    []db.InitialLink{{Type: "parent", ToNumber: f.childIssue.ID}},
					Metadata: map[string]jsontext.Value{notification.MetadataKey("example-user"): jsontext.Value(`{"from":"coordinator","message":"please review"}`)}})
				require.NoError(t, err)
				var got pingRecorder
				service, _ := newDaemonServiceReceiving(t, store, f, got.receive)
				args := mcpevents.Arguments{For: "example-user"}
				if filtered {
					args.IssueUID = grandchild.UID
				}
				subscribeEvents(t, service, f.scoped, args)
				_, _, _, err = store.SoftDeleteIssue(ctx, f.childIssue.ID, "coordinator")
				require.NoError(t, err)
				require.NoError(t, service.Drain(ctx))
				got.pings.Store(0)

				_, _, _, err = store.RestoreIssue(ctx, f.childIssue.ID, "coordinator")
				require.NoError(t, err)
				require.NoError(t, service.Drain(ctx))
				assert.Equal(t, int64(1), got.pings.Load(), "the restore that exposes the grandchild's request wakes the recipient")
			})
		})
	}
}
