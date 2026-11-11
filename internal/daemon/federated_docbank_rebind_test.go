package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
)

func TestDocbankForwardMapsHubCredentialRejection(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, _ := createClaimGateHelperIssue(t, store)
	var status int
	var code string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"private remote detail"}}`, code)
	}))
	t.Cleanup(upstream.Close)
	binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: upstream.URL, HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator", AllowInsecure: true}
	_, err := store.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	cfg := ServerConfig{DB: store, FederationCredentials: uiCapabilityCredentials{credential: config.FederationCredential{HubURL: upstream.URL, HubProjectID: 1, Token: "fixture", AllowInsecure: true}}}
	for _, suffix := range []string{"issues/a/uploads", "issues/a/links", "versions/a/content"} {
		for _, tc := range []struct {
			status     int
			code       string
			wantStatus int
			wantCode   string
		}{
			{http.StatusUnauthorized, "signature_invalid", http.StatusBadGateway, "hub_credential_rejected"},
			{http.StatusForbidden, "auth_invalid", http.StatusBadGateway, "hub_credential_rejected"},
			{http.StatusForbidden, "claim_denied", http.StatusForbidden, "claim_denied"},
			{http.StatusForbidden, "docbank_outside_scope", http.StatusForbidden, "docbank_outside_scope"},
		} {
			t.Run(fmt.Sprintf("%s/%s", suffix, tc.code), func(t *testing.T) {
				status, code = tc.status, tc.code
				method := http.MethodPost
				if strings.HasSuffix(suffix, "/content") {
					method = http.MethodGet
				}
				_, err := docbankForwardRequest(t.Context(), cfg, binding, method, suffix, nil, nil)
				var apiErr *api.APIError
				require.ErrorAs(t, err, &apiErr)
				require.Equal(t, tc.wantStatus, apiErr.Status)
				require.Equal(t, tc.wantCode, apiErr.Code)
				require.NotContains(t, apiErr.Error(), "private remote detail")
			})
		}
	}
}

func TestForwardDocbankDescriptorRejectsUnknownAttachmentFields(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)
	var extra string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"project_uid":%q,"attachment":{"name":"file.pdf","media_type":"application/pdf","size":12,"docbank_version":"6f1c2e8a-2222-4222-8222-222222222222","origin":%q%s}}`, project.UID, store.InstanceUID(), extra)
	}))
	t.Cleanup(upstream.Close)
	binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: upstream.URL, HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator", AllowInsecure: true}
	_, err := store.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	cfg := ServerConfig{DB: store, FederationCredentials: uiCapabilityCredentials{credential: config.FederationCredential{HubURL: upstream.URL, HubProjectID: 1, Token: "fixture", AllowInsecure: true}}}
	result := &claimRefreshResult{known: true, binding: binding}
	_, err = forwardDocbankDescriptor(t.Context(), cfg, project, &binding, issue, result, store.InstanceUID(), "file.pdf", nil, strings.NewReader("data"))
	require.NoError(t, err)
	extra = `,"new_required_field":"meaning"`
	_, err = forwardDocbankDescriptor(t.Context(), cfg, project, &binding, issue, result, store.InstanceUID(), "file.pdf", nil, strings.NewReader("data"))
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusBadGateway, apiErr.Status)
	require.Equal(t, "attachment_invalid", apiErr.Code)
}

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
	entered := make(chan struct{})
	cfg := ServerConfig{DB: &rebindEntryStore{Storage: store, entered: entered}, FederationCredentials: credentials}
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
	<-entered
	select {
	case err := <-done:
		t.Fatalf("forward returned during rebind: %v", err)
	case <-time.After(100 * time.Millisecond): //nolint:kennlint // deadlinetest: the rebind gate must hold the entered forward
	}
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

type rebindEntryStore struct {
	db.Storage
	entered chan struct{}
}

func (s *rebindEntryStore) InstanceUID() string {
	close(s.entered)
	return s.Storage.InstanceUID()
}
