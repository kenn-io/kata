package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
)

type docbankBusySyncError struct{}

func (docbankBusySyncError) Error() string                 { return "busy" }
func (docbankBusySyncError) HTTPStatus() int               { return http.StatusTooManyRequests }
func (docbankBusySyncError) HTTPBody() string              { return `{"error":{"code":"admission_limited"}}` }
func (docbankBusySyncError) HTTPRetryAfter() time.Duration { return 5 * time.Second }

func TestDocbankPendingSyncBusy(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)
	binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator"}
	_, err := store.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"busy", docbankBusySyncError{}, 429, "server_busy"},
		{"offline", errors.New("unavailable"), 503, "federation_offline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ServerConfig{DB: store, FederationCredentials: uiCapabilityCredentials{credential: config.FederationCredential{HubURL: binding.HubURL, HubProjectID: 1, Token: "fixture"}}, FederationSyncOnce: func(context.Context, db.FederationBinding, config.FederationCredential) error { return tc.err }}
			err := ensureDocbankIssueUpstream(t.Context(), cfg, project, binding, issue, &claimRefreshResult{binding: binding, known: true, missing: true})
			var apiErr *api.APIError
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, tc.status, apiErr.Status)
			require.Equal(t, tc.code, apiErr.Code)
			if tc.name == "busy" {
				require.Contains(t, apiErr.Message, "hub.example")
				var headers huma.HeadersError
				require.ErrorAs(t, err, &headers)
				require.Equal(t, "5", headers.GetHeaders().Get("Retry-After"))
			}
		})
	}
}

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
	for _, tc := range []struct {
		suffix, method string
		status         int
		code           string
		wantStatus     int
		wantCode       string
	}{
		{"issues/a/uploads", http.MethodPost, http.StatusUnauthorized, "signature_invalid", http.StatusBadGateway, "hub_credential_rejected"},
		{"issues/a/links", http.MethodPost, http.StatusForbidden, "auth_invalid", http.StatusBadGateway, "hub_credential_rejected"},
		{"issues/a/links", http.MethodPost, http.StatusForbidden, "claim_denied", http.StatusForbidden, "claim_denied"},
		{"issues/a/links", http.MethodPost, http.StatusForbidden, "docbank_outside_scope", http.StatusForbidden, "docbank_outside_scope"},
	} {
		t.Run(tc.suffix+"/"+tc.code, func(t *testing.T) {
			status, code = tc.status, tc.code
			_, err := docbankForwardRequest(t.Context(), cfg, binding, tc.method, tc.suffix, nil, nil)
			var apiErr *api.APIError
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, tc.wantStatus, apiErr.Status)
			require.Equal(t, tc.wantCode, apiErr.Code)
			require.NotContains(t, apiErr.Error(), "private remote detail")
			if tc.code == "claim_denied" {
				require.Contains(t, apiErr.Hint, "run kata federation lease acquire")
			}
		})
	}
}

func TestForwardDocbankDescriptorRejectsReadOnlyBeforeProbe(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(upstream.Close)
	binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: upstream.URL, HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, Actor: "operator", AllowInsecure: true}
	_, err := store.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	cfg := ServerConfig{DB: store, FederationCredentials: uiCapabilityCredentials{credential: config.FederationCredential{HubURL: upstream.URL, HubProjectID: 1, Token: "fixture", AllowInsecure: true}}}
	_, err = forwardDocbankDescriptor(t.Context(), cfg, project, &binding, issue, nil, "", "file.pdf", nil, strings.NewReader("file"))
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusConflict, apiErr.Status)
	require.Equal(t, "federated_read_only", apiErr.Code)
	require.Zero(t, calls.Load())
}

func TestForwardDocbankDescriptorRejectsUnexpectedOrigin(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)
	origin := store.InstanceUID()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"project_uid":%q,"attachment":{"name":"file.pdf","media_type":"application/pdf","size":12,"docbank_version":"6f1c2e8a-2222-4222-8222-222222222222","origin":%q}}`, project.UID, origin)
	}))
	t.Cleanup(upstream.Close)
	binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: upstream.URL, HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator", AllowInsecure: true}
	_, err := store.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	cfg := ServerConfig{DB: store, FederationCredentials: uiCapabilityCredentials{credential: config.FederationCredential{HubURL: upstream.URL, HubProjectID: 1, Token: "fixture", AllowInsecure: true}}}
	result := &claimRefreshResult{known: true, binding: binding}
	_, err = forwardDocbankDescriptor(t.Context(), cfg, project, &binding, issue, result, store.InstanceUID(), "file.pdf", nil, strings.NewReader("data"))
	require.NoError(t, err)
	origin = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	_, err = forwardDocbankDescriptor(t.Context(), cfg, project, &binding, issue, result, store.InstanceUID(), "file.pdf", nil, strings.NewReader("data"))
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusBadGateway, apiErr.Status)
	require.Equal(t, "attachment_invalid", apiErr.Code)
}

func TestDocbankUploadDoesNotBlockRebindOrLaterForwards(t *testing.T) {
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)
	uploadStarted := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/uploads") {
			close(uploadStarted)
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = fmt.Fprintf(w, `{"project_uid":%q,"attachment":{"name":"file.pdf","media_type":"application/pdf","size":12,"docbank_version":"6f1c2e8a-2222-4222-8222-222222222222","origin":%q}}`, project.UID, store.InstanceUID())
			return
		}
		_, _ = fmt.Fprintf(w, `{"version":1,"project_uid":%q,"origin":%q}`, project.UID, store.InstanceUID())
	}))
	t.Cleanup(upstream.Close)
	binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: upstream.URL, HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "operator", AllowInsecure: true}
	_, err := store.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	cfg := ServerConfig{DB: store, FederationCredentials: uiCapabilityCredentials{credential: config.FederationCredential{HubURL: upstream.URL, HubProjectID: 1, Token: "fixture", AllowInsecure: true}}}
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close(); _ = reader.Close() })
	uploadDone := make(chan error, 1)
	go func() {
		_, err := forwardDocbankDescriptor(t.Context(), cfg, project, &binding, issue, &claimRefreshResult{known: true, binding: binding}, store.InstanceUID(), "file.pdf", nil, reader)
		uploadDone <- err
	}()
	<-uploadStarted
	rebound := make(chan error, 1)
	go func() {
		finish, err := federationcoord.BeginRebind(t.Context(), federationcoord.Key(store.InstanceUID(), project.ID), store, project.ID)
		if err == nil {
			current := binding
			current.HubProjectID = 2
			_, err = store.UpsertFederationBinding(t.Context(), current)
			finish()
		}
		rebound <- err
	}()
	select {
	case err := <-rebound:
		require.NoError(t, err)
	case <-time.After(time.Second): //nolint:kennlint // deadlinetest: a stalled upload must not hold the rebind gate
		t.Fatal("rebind stalled behind an upload")
	}
	current := binding
	current.HubProjectID = 2
	cfg.FederationCredentials = uiCapabilityCredentials{credential: config.FederationCredential{HubURL: upstream.URL, HubProjectID: 2, Token: "fixture", AllowInsecure: true}}
	_, err = docbankOriginCapabilities(t.Context(), cfg, project, &current, "")
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	err = <-uploadDone
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, "attachment_origin_mismatch", apiErr.Code)
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

	entered := make(chan struct{})
	cfg.DB = &rebindEntryStore{Storage: store, entered: entered}
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
	current.HubURL = replacement.URL
	current.HubProjectID = 2
	_, err = store.UpsertFederationBinding(t.Context(), current)
	require.NoError(t, err)
	release()
	require.Error(t, <-done)
	cfg.DB = store
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

type rebindEntryStore struct {
	db.Storage
	entered chan struct{}
	once    sync.Once
}

func (s *rebindEntryStore) InstanceUID() string {
	s.once.Do(func() { close(s.entered) })
	return s.Storage.InstanceUID()
}
