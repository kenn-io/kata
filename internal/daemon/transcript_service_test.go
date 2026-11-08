package daemon

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/transcriptservice"
)

// This exercises configured policy, not an AgentsView receiver or a real archive.
func TestTranscriptDiscoveryNeverContactsReceiver(t *testing.T) {
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
		Auth: config.AuthConfig{Token: "test-owner-token"}, TranscriptService: config.TranscriptServiceConfig{Project: map[string]config.TranscriptServiceTarget{
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
	discovery, err := transcriptservice.DecodeDiscovery(raw, transcriptPin(pin), []string{store.InstanceUID()}, time.Now())
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

func TestTranscriptDiscoveryRequiresExistingRootAndExplicitPolicy(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, _ := createClaimGateHelperIssue(t, store)
	_, err := discoverTranscriptService(t.Context(), ServerConfig{DB: store}, project.ID)
	require.Error(t, err)
	enableClaimGateHelperHub(t, store, project.ID)
	_, err = discoverTranscriptService(t.Context(), ServerConfig{DB: store}, project.ID)
	require.Error(t, err)
}

func TestTranscriptRouteGenerationIgnoresCursorButRejectsRebind(t *testing.T) {
	before := db.FederationBinding{Enabled: true, Role: db.FederationRoleSpoke, HubURL: "https://root.example", HubProjectID: 7, HubProjectUID: "00000000000000000000000001",
		RelayConfig: &db.RelayBindingConfig{BindingUID: "00000000000000000000000002", ResetEpoch: 3}}
	after := before
	after.PullCursorEventID = 42
	require.True(t, sameTranscriptRoute(before, after))
	copyRelay := *before.RelayConfig
	after.RelayConfig = &copyRelay
	after.RelayConfig.ResetEpoch++
	require.False(t, sameTranscriptRoute(before, after))
}

func TestTranscriptConfiguredPolicyDoesNotResolveCredential(t *testing.T) {
	target := config.TranscriptServiceConfig{Project: map[string]config.TranscriptServiceTarget{
		"00000000000000000000000001": {Enabled: true, ReceiverURL: "https://receiver.example", LocatorURL: "https://archive.example"},
	}}
	got, err := config.NormalizeTranscriptServiceConfig(target)
	require.NoError(t, err)
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "token")
}

func TestTranscriptForwardingUsesOnlySelectedEnrollmentAndRejectsRedirect(t *testing.T) {
	project := "00000000000000000000000001"
	root := "00000000000000000000000002"
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{13}, ed25519.SeedSize))
	now := time.Now()
	descriptor, err := transcriptservice.SignDescriptor(transcriptservice.Descriptor{Version: 1, ProjectUID: project, AuthorityUID: root,
		ServiceID: strings.Repeat("a", 64), LocatorBase: "https://archive.example", EnvelopeVersion: 1,
		MaxBytes: transcriptservice.MaxSnapshotBytes, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()}, key)
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
		require.Equal(t, "Bearer selected-enrollment", r.Header.Get("Authorization"))
		if redirect.Load() {
			http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
			return
		}
		require.NoError(t, json.MarshalWrite(w, transcriptservice.Discovery{Descriptor: descriptor, InstanceUID: root, Route: []transcriptservice.RouteObservation{{InstanceUID: root, ProjectUID: project}}}))
	}))
	t.Cleanup(upstream.Close)
	binding := db.FederationBinding{HubURL: upstream.URL, HubProjectID: 7}
	credential := config.FederationCredential{HubURL: upstream.URL, HubProjectID: 7, Token: "selected-enrollment"}
	got, err := fetchTranscriptService(t.Context(), ServerConfig{}, binding, credential, project, pin, []string{root})
	require.NoError(t, err)
	require.Equal(t, descriptor, got.Descriptor)
	redirect.Store(true)
	_, err = fetchTranscriptService(t.Context(), ServerConfig{}, binding, credential, project, pin, []string{root})
	require.Error(t, err)
	require.Zero(t, foreignCalls.Load())
}
