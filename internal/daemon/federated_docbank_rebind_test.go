package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
)

func TestDocbankForwardRejectsFormerRouteAfterRebind(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)
	var received []string
	var replacementReceived []string
	former := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = append(received, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "project_uid": project.UID, "origin": store.InstanceUID()})
	}))
	t.Cleanup(former.Close)
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		replacementReceived = append(replacementReceived, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(replacement.Close)
	old := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: former.URL, HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator", AllowInsecure: true}
	_, err := store.UpsertFederationBinding(t.Context(), old)
	require.NoError(t, err)
	cfg := ServerConfig{DB: store, FederationCredentials: uiCapabilityCredentials{credential: config.FederationCredential{HubURL: former.URL, HubProjectID: 1, Token: "old-token", AllowInsecure: true}}}
	_, err = docbankOriginCapabilities(t.Context(), cfg, project, &old, "")
	require.NoError(t, err)
	require.Equal(t, []string{"Bearer old-token"}, received)

	current := old
	current.HubURL = replacement.URL
	current.HubProjectID = 2
	_, err = store.UpsertFederationBinding(t.Context(), current)
	require.NoError(t, err)
	cfg.FederationCredentials = uiCapabilityCredentials{credential: config.FederationCredential{HubURL: replacement.URL, HubProjectID: 2, Token: "new-token", AllowInsecure: true}}
	resp, err := docbankForwardRequest(t.Context(), cfg, old, http.MethodPost, "issues/test/uploads", nil, strings.NewReader("file"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	assert.Equal(t, []string{"Bearer old-token"}, received, "the former relay must not receive the replacement relay's credential")
	assert.Error(t, err)
	resp, err = docbankForwardRequest(t.Context(), cfg, old, http.MethodPost, "issues/test/links", nil, strings.NewReader(`{}`))
	if resp != nil {
		_ = resp.Body.Close()
	}
	assert.Equal(t, []string{"Bearer old-token"}, received, "a link must not send the replacement credential to the former relay")
	assert.Error(t, err)
	err = ensureDocbankIssueUpstream(t.Context(), cfg, project, old, issue, nil)
	assert.Equal(t, []string{"Bearer old-token"}, received, "the claim lookup must not send the replacement credential to the former relay")
	assert.Error(t, err)
	resp, err = docbankForwardRequest(t.Context(), cfg, current, http.MethodPost, "issues/test/uploads", nil, strings.NewReader("file"))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{"Bearer new-token"}, replacementReceived)
}

func TestDocbankForwardWaitsForRebind(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, _ := createClaimGateHelperIssue(t, store)
	var calls atomic.Int32
	former := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(former.Close)
	old := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: former.URL, HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator", AllowInsecure: true}
	_, err := store.UpsertFederationBinding(t.Context(), old)
	require.NoError(t, err)
	credentials := &uiCapabilityCredentials{credential: config.FederationCredential{HubURL: former.URL, HubProjectID: 1, Token: "old-token", AllowInsecure: true}}
	cfg := ServerConfig{DB: store, FederationCredentials: credentials}
	finish, err := federationcoord.BeginRebind(t.Context(), federationcoord.Key(store.InstanceUID(), project.ID), store, project.ID)
	require.NoError(t, err)
	release := sync.OnceFunc(finish)
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() {
		resp, err := docbankForwardRequest(t.Context(), cfg, old, http.MethodPost, "issues/test/uploads", nil, strings.NewReader("file"))
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	current := old
	current.HubURL = "http://replacement.example"
	current.HubProjectID = 2
	_, err = store.UpsertFederationBinding(t.Context(), current)
	require.NoError(t, err)
	credentials.credential = config.FederationCredential{HubURL: current.HubURL, HubProjectID: current.HubProjectID, Token: "new-token", AllowInsecure: true}
	release()
	require.Error(t, <-done)
	require.Zero(t, calls.Load())
}
