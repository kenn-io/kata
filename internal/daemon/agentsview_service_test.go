package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentsviewservice"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
)

// This exercises configured policy, not an AgentsView receiver or a real archive.
func TestAgentsViewDiscoveryNeverContactsReceiver(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, _ := createClaimGateHelperIssue(t, store)
	enableClaimGateHelperHub(t, store, project.ID)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{11}, ed25519.SeedSize))
	public := key.Public().(ed25519.PublicKey)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(t.Context(), pin))
	var receiverCalls atomic.Int64
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		receiverCalls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(receiver.Close)
	server := NewServer(ServerConfig{DB: store, RootSigning: NewRootSigning(db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: key}),
		Auth: config.AuthConfig{Token: "test-owner-token"}, AgentsView: config.AgentsViewConfig{Project: map[string]config.AgentsViewTarget{
			project.UID: {Enabled: true, ReceiverURL: receiver.URL, LocatorURL: "https://archive.example/company"},
		}}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	endpoint := httptest.NewServer(server.Handler())
	t.Cleanup(endpoint.Close)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/transcript-service", endpoint.URL, project.ID), nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer test-owner-token")
	response, err := endpoint.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
	require.Equal(t, "private, no-store", response.Header.Get("Cache-Control"))
	discovery, err := agentsviewservice.DecodeDiscovery(raw, agentsViewPin(pin), []string{store.InstanceUID()}, time.Now())
	require.NoError(t, err)
	require.Equal(t, "https://archive.example/company", discovery.Descriptor.LocatorBase)
	require.NotContains(t, string(raw), receiver.URL)
	require.NotContains(t, string(raw), "test-owner-token")
	require.Zero(t, receiverCalls.Load())
	// An arbitrary enrolled identity is not accepted on the ordinary API.
	request.Header.Set("Authorization", "Bearer wrong-token")
	denied, err := endpoint.Client().Do(request)
	require.NoError(t, err)
	require.NoError(t, denied.Body.Close())
	require.NotEqual(t, http.StatusOK, denied.StatusCode)
}

type agentsViewDeadlineHost struct{ deadlines chan bool }

func (h agentsViewDeadlineHost) Authorize(ctx context.Context, _ HostAccessRequest) (HostAccessDecision, error) {
	_, bounded := ctx.Deadline()
	h.deadlines <- bounded
	return HostAccessDecision{}, nil
}

// Release authorization must share the discovery budget, including after I/O.
func TestAgentsViewDiscoveryBoundsFinalHostAuthorization(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, _ := createClaimGateHelperIssue(t, store)
	enableClaimGateHelperHub(t, store, project.ID)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{15}, ed25519.SeedSize))
	public := key.Public().(ed25519.PublicKey)
	require.NoError(t, store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	deadlines := make(chan bool, 10)
	server := NewServer(ServerConfig{DB: store, HostAccess: agentsViewDeadlineHost{deadlines}, RootSigning: NewRootSigning(db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: key}),
		AgentsView: config.AgentsViewConfig{Project: map[string]config.AgentsViewTarget{project.UID: {Enabled: true, ReceiverURL: "https://receiver.example", LocatorURL: "https://archive.example/team"}}}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/transcript-service", project.ID), nil)
	request = request.WithContext(WithPrincipal(request.Context(), Principal{Kind: PrincipalHost, Actor: "member", Subject: "host-member"}))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.GreaterOrEqual(t, len(deadlines), 2)
	for len(deadlines) > 0 {
		require.True(t, <-deadlines, "all host authorization calls must have a deadline")
	}
}

func TestAgentsViewDiscoveryBudgetCoversBothRoutes(t *testing.T) {
	for _, path := range []string{"/api/v1/projects/7/transcript-service", "/api/v1/projects/7/federation/transcript-service"} {
		t.Run(path, func(t *testing.T) {
			called := false
			handler := withAgentsViewDiscoveryBudget(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				called = true
				deadline, ok := r.Context().Deadline()
				require.True(t, ok)
				require.LessOrEqual(t, time.Until(deadline), agentsViewRequestTimeout)
			}))
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
			require.True(t, called)
		})
	}
}

func TestAgentsViewDiscoveryRequiresExistingRootAndExplicitPolicy(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, _ := createClaimGateHelperIssue(t, store)
	enableClaimGateHelperHub(t, store, project.ID)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{12}, ed25519.SeedSize))
	public := key.Public().(ed25519.PublicKey)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(t.Context(), pin))
	serverCfg := ServerConfig{
		DB:          store,
		RootSigning: NewRootSigning(db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: key}),
		AgentsView: config.AgentsViewConfig{Project: map[string]config.AgentsViewTarget{
			project.UID: {Enabled: false, ReceiverURL: "https://receiver.example", LocatorURL: "https://archive.example/team"},
		}},
	}
	_, err := discoverAgentsViewService(t.Context(), serverCfg, project.ID)
	require.Error(t, err)
	serverCfg.AgentsView.Project[project.UID] = config.AgentsViewTarget{Enabled: true, ReceiverURL: "https://receiver.example", LocatorURL: "https://archive.example/team"}
	_, err = discoverAgentsViewService(t.Context(), serverCfg, project.ID)
	require.NoError(t, err)

	withoutPin := openClaimGateHelperDB(t)
	other, _ := createClaimGateHelperIssue(t, withoutPin)
	enableClaimGateHelperHub(t, withoutPin, other.ID)
	_, err = discoverAgentsViewService(t.Context(), ServerConfig{
		DB:          withoutPin,
		RootSigning: NewRootSigning(db.RootAttributionSigner{AuthorityUID: withoutPin.InstanceUID(), PrivateKey: key}),
		AgentsView: config.AgentsViewConfig{Project: map[string]config.AgentsViewTarget{
			other.UID: {Enabled: true, ReceiverURL: "https://receiver.example", LocatorURL: "https://archive.example/team"},
		}},
	}, other.ID)
	require.Error(t, err)
}

func TestAgentsViewRouteGenerationIgnoresCursorButRejectsRebind(t *testing.T) {
	before := db.FederationBinding{Enabled: true, Role: db.FederationRoleSpoke, HubURL: "https://root.example", HubProjectID: 7, HubProjectUID: "00000000000000000000000001",
		RelayConfig: &db.RelayBindingConfig{BindingUID: "00000000000000000000000002", ResetEpoch: 3}}
	after := before
	after.PullCursorEventID = 42
	require.True(t, sameAgentsViewRoute(before, after))
	copyRelay := *before.RelayConfig
	after.RelayConfig = &copyRelay
	after.RelayConfig.ResetEpoch++
	require.False(t, sameAgentsViewRoute(before, after))
}

func TestAgentsViewForwardClientUsesOnlySelectedCredentialAndPolicy(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[auth]\ntoken = \"ambient-file-token\"\ntrust_private_network = true\n"), 0600))
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "ambient-env-token")

	_, err := agentsViewForwardHTTPClient(t.Context(), ServerConfig{}, config.FederationCredential{
		HubURL: "http://100.64.0.5:7373", HubProjectID: 7, Token: "selected-enrollment",
	})
	require.Error(t, err, "the selected daemon policy must not inherit trust from the invoking CLI home")

	var authorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	client, err := agentsViewForwardHTTPClient(t.Context(), ServerConfig{}, config.FederationCredential{
		HubURL: upstream.URL, HubProjectID: 7, Token: "selected-enrollment",
	})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, upstream.URL, nil)
	require.NoError(t, err)
	response, err := client.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, "Bearer selected-enrollment", authorization)
}

func TestAgentsViewForwardClientRejectsUnpinnedUnixTarget(t *testing.T) {
	var called bool
	_, err := agentsViewForwardHTTPClient(t.Context(), ServerConfig{AgentsViewHubClient: func(context.Context, config.FederationCredential) (*http.Client, error) {
		called = true
		return http.DefaultClient, nil
	}}, config.FederationCredential{HubURL: "http://kata.invalid", HubProjectID: 7, Token: "selected-enrollment"})
	require.Error(t, err)
	require.False(t, called, "an unpinned socket target must not select a transport")
}

func TestAgentsViewForwardingUsesOnlySelectedEnrollmentAndRejectsRedirect(t *testing.T) {
	project := "00000000000000000000000001"
	root := "00000000000000000000000002"
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{13}, ed25519.SeedSize))
	now := time.Now()
	descriptor, err := agentsviewservice.SignDescriptor(agentsviewservice.Descriptor{Version: 1, ProjectUID: project, AuthorityUID: root,
		ServiceID: strings.Repeat("a", 64), LocatorBase: "https://archive.example", EnvelopeVersion: 1,
		MaxBytes: agentsviewservice.MaxSnapshotBytes, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()}, key)
	require.NoError(t, err)
	public := key.Public().(ed25519.PublicKey)
	pin := db.RootKeyPin{ProjectUID: project, AuthorityUID: root, KeyID: descriptor.KeyID, PublicKey: public}
	var foreignCalls atomic.Int64
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { foreignCalls.Add(1); w.WriteHeader(http.StatusOK) }))
	t.Cleanup(foreign.Close)
	var redirect atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/projects/7/federation/transcript-service", r.URL.Path)
		require.Equal(t, project, r.URL.Query().Get("project_uid"))
		require.Equal(t, root, r.URL.Query().Get("authority_uid"))
		require.Equal(t, "00000000000000000000000004", r.URL.Query().Get("binding_uid"))
		require.Equal(t, "00000000000000000000000003", r.URL.Query().Get("peer_instance_uid"))
		require.Equal(t, fmt.Sprint(db.RelayProtocolVersion), r.URL.Query().Get("protocol_version"))
		require.Equal(t, "3", r.URL.Query().Get("reset_epoch"))
		require.Equal(t, "identity", r.Header.Get("Accept-Encoding"))
		require.Equal(t, "Bearer selected-enrollment", r.Header.Get("Authorization"))
		if redirect.Load() {
			http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
			return
		}
		require.NoError(t, json.MarshalWrite(w, agentsviewservice.Discovery{Descriptor: descriptor, InstanceUID: root, Route: []agentsviewservice.RouteObservation{{InstanceUID: root, ProjectUID: project}}}))
	}))
	t.Cleanup(upstream.Close)
	binding := db.FederationBinding{HubURL: upstream.URL, HubProjectID: 7}
	credential := config.FederationCredential{HubURL: upstream.URL, HubProjectID: 7, Token: "selected-enrollment"}
	relay := db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: "00000000000000000000000004", ResetEpoch: 3}
	got, err := fetchAgentsViewService(t.Context(), ServerConfig{}, binding, credential, project, pin, []string{root}, relay, "00000000000000000000000003")
	require.NoError(t, err)
	require.Equal(t, descriptor, got.Descriptor)
	redirect.Store(true)
	_, err = fetchAgentsViewService(t.Context(), ServerConfig{}, binding, credential, project, pin, []string{root}, relay, "00000000000000000000000003")
	require.Error(t, err)
	require.Zero(t, foreignCalls.Load())
}

func TestAgentsViewFederationRequestRequiresCurrentRelayGeneration(t *testing.T) {
	principal := federationPrincipal{
		EnrollmentID: 9, ScopedProjectID: 7, RelayBindingUID: "00000000000000000000000004",
		RelayProtocolVersion: db.RelayProtocolVersion, RelayResetEpoch: 3, SpokeInstanceUID: "00000000000000000000000003",
	}
	require.True(t, agentsViewRelayGenerationMatches(principal, 7, db.RelayProtocolVersion, principal.RelayBindingUID, principal.SpokeInstanceUID, 3))
	require.False(t, agentsViewRelayGenerationMatches(principal, 7, 0, principal.RelayBindingUID, principal.SpokeInstanceUID, 3))
	require.False(t, agentsViewRelayGenerationMatches(principal, 7, db.RelayProtocolVersion, "00000000000000000000000005", principal.SpokeInstanceUID, 3))
	require.False(t, agentsViewRelayGenerationMatches(principal, 7, db.RelayProtocolVersion, principal.RelayBindingUID, "00000000000000000000000006", 3))
	require.False(t, agentsViewRelayGenerationMatches(principal, 7, db.RelayProtocolVersion, principal.RelayBindingUID, principal.SpokeInstanceUID, 4))
	legacy := principal
	legacy.RelayProtocolVersion = 0
	require.False(t, agentsViewRelayGenerationMatches(legacy, 7, 0, "", "", 0))
	changed := principal
	changed.RelayResetEpoch++
	require.False(t, sameAgentsViewPrincipal(principal, changed))
}

func TestAgentsViewTransportFailsClosedWhileProjectIsRebinding(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, _ := createClaimGateHelperIssue(t, store)
	key := federationcoord.Key(store.InstanceUID(), project.ID)
	release, err := federationcoord.BeginRebind(t.Context(), key, store, project.ID)
	require.NoError(t, err)
	t.Cleanup(release)
	_, err = tryBeginAgentsViewTransport(t.Context(), ServerConfig{DB: store}, project.ID)
	require.Error(t, err)
	require.ErrorContains(t, err, "AgentsView service is unavailable")
}

func TestAgentsViewForwardingBoundsInjectedHeadersAndRejectsCompressedOrOversizedJSON(t *testing.T) {
	project := "00000000000000000000000001"
	root := "00000000000000000000000002"
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{14}, ed25519.SeedSize))
	now := time.Now()
	descriptor, err := agentsviewservice.SignDescriptor(agentsviewservice.Descriptor{
		Version: 1, ProjectUID: project, AuthorityUID: root, ServiceID: strings.Repeat("b", 64),
		LocatorBase: "https://archive.example/team", EnvelopeVersion: 1,
		MaxBytes: agentsviewservice.MaxSnapshotBytes, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}, key)
	require.NoError(t, err)
	discovery, err := json.Marshal(agentsviewservice.Discovery{
		Descriptor:  descriptor,
		InstanceUID: root,
		Route:       []agentsviewservice.RouteObservation{{InstanceUID: root, ProjectUID: project}},
	})
	require.NoError(t, err)
	oversized := append(bytes.Clone(discovery), bytes.Repeat([]byte{' '}, agentsviewservice.MaxDiscoveryBytes-len(discovery)+1)...)
	var responseKind atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch responseKind.Load() {
		case 0:
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(discovery)
		case 1:
			w.Header().Set("X-Oversized", strings.Repeat("x", agentsViewMaxHeaderBytes+1))
			_, _ = w.Write(discovery)
		case 2:
			_, _ = w.Write(oversized)
		default:
			_, _ = w.Write(discovery)
		}
	}))
	t.Cleanup(server.Close)
	binding := db.FederationBinding{HubURL: server.URL, HubProjectID: 7}
	credential := config.FederationCredential{HubURL: server.URL, HubProjectID: 7, Token: "selected-enrollment"}
	relay := db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: "00000000000000000000000004", ResetEpoch: 3}
	public := key.Public().(ed25519.PublicKey)
	pin := db.RootKeyPin{ProjectUID: project, AuthorityUID: root, KeyID: descriptor.KeyID, PublicKey: public}
	responseKind.Store(3)
	_, err = fetchAgentsViewService(t.Context(), ServerConfig{}, binding, credential, project, pin, []string{root}, relay, "00000000000000000000000003")
	require.NoError(t, err)
	for _, kind := range []int32{0, 2} {
		responseKind.Store(kind)
		_, err := fetchAgentsViewService(t.Context(), ServerConfig{}, binding, credential, project, pin, []string{root}, relay, "00000000000000000000000003")
		require.Error(t, err)
	}
	responseKind.Store(1)
	_, err = fetchAgentsViewService(t.Context(), ServerConfig{AgentsViewHubClient: func(context.Context, config.FederationCredential) (*http.Client, error) {
		return server.Client(), nil
	}}, binding, credential, project, pin, []string{root}, relay, "00000000000000000000000003")
	require.Error(t, err)
}
