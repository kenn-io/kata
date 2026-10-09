package daemon_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationsigning"
)

type bridgeDisconnectPartialSetupStore struct {
	db.Storage
	failSpokeBinding bool
	failRelayConfig  bool
}

func (s *bridgeDisconnectPartialSetupStore) UpsertFederationBinding(ctx context.Context, binding db.FederationBinding) (db.FederationBinding, error) {
	if s.failSpokeBinding && binding.Role == db.FederationRoleSpoke {
		s.failSpokeBinding = false
		return db.FederationBinding{}, errors.New("injected interruption before spoke binding commit")
	}
	return s.Storage.UpsertFederationBinding(ctx, binding)
}

func (s *bridgeDisconnectPartialSetupStore) SetRelayBindingConfig(ctx context.Context, projectID int64, relay db.RelayBindingConfig, expected ...db.RelayBindingConfig) (db.FederationBinding, error) {
	if s.failRelayConfig {
		s.failRelayConfig = false
		return db.FederationBinding{}, errors.New("injected interruption before relay config commit")
	}
	return s.Storage.SetRelayBindingConfig(ctx, projectID, relay, expected...)
}

func (s *bridgeDisconnectPartialSetupStore) ValidateRelayLifecycle(ctx context.Context, projectID int64) error {
	lifecycle, ok := s.Storage.(db.RelayLifecycleStore)
	if !ok {
		return db.ErrTransactionFinalizationFailed
	}
	return lifecycle.ValidateRelayLifecycle(ctx, projectID)
}

type bridgeDisconnectObservedCredentials struct {
	config.FederationCredentialStore
	config.FederationCredentialReplacer
	config.FederationCredentialRemover
	config.FederationRelayPendingMetadataReader
	pendingLeave chan struct{}
	once         sync.Once
}

func (s *bridgeDisconnectObservedCredentials) ReplaceFederationCredential(ctx context.Context, replacement config.FederationCredentialReplacement) error {
	err := s.FederationCredentialReplacer.ReplaceFederationCredential(ctx, replacement)
	if err == nil && replacement.Replacement.LeavePending && !replacement.Expected.LeavePending {
		s.once.Do(func() { close(s.pendingLeave) })
	}
	return err
}

type bridgeDisconnectResponse struct {
	status int
	body   []byte
}

// R9: disconnect previews without mutation, revokes exactly the retained narrow
// grant, and safely resumes after a lost upstream response without losing data.
func TestFederationBridgeDisconnectLifecycle(t *testing.T) {
	for _, mode := range []string{"normal", "lost_response", "disconnect_write_race", "pending_enrollment", "precommit_enrollment_failure", "partial_project_only", "partial_binding_without_relay", "partial_project_archived", "inflight_enrollment", "parent_revoked", "credential_changed", "archived", "archived_pending", "signed_disconnect"} {
		t.Run(mode, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				t.Setenv("KATA_HOME", t.TempDir())
				if mode == "signed_disconnect" {
					t.Setenv("KATA_BRIDGE_SIGNING_KEY", strings.Repeat("k", 64))
				}
				rootStore := openReplicaServiceStore(t)
				root := newProjectAccessFixture(t, rootStore)
				_, err := rootStore.EnableProjectFederation(t.Context(), root.private.ID, "admin")
				require.NoError(t, err)
				public, _, err := ed25519.GenerateKey(nil)
				require.NoError(t, err)
				require.NoError(t, rootStore.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: root.private.UID, AuthorityUID: rootStore.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				var disconnectCalls atomic.Int32
				var sawSignedDisconnect atomic.Bool
				enrollmentStarted := make(chan struct{}, 1)
				releaseEnrollment := make(chan struct{})
				var releaseEnrollmentOnce sync.Once
				releaseEnrollmentRequest := func() { releaseEnrollmentOnce.Do(func() { close(releaseEnrollment) }) }
				if mode == "inflight_enrollment" {
					defer releaseEnrollmentRequest()
				}
				disconnectRequestStarted := make(chan struct{}, 1)
				revokeResponseBlocked := make(chan struct{}, 1)
				releaseRevokeResponse := make(chan struct{})
				var releaseRevokeResponseOnce sync.Once
				releaseRevokeResponseRequest := func() { releaseRevokeResponseOnce.Do(func() { close(releaseRevokeResponse) }) }
				defer releaseRevokeResponseRequest()
				upstreamURL, err := url.Parse(root.server.URL)
				require.NoError(t, err)
				remoteHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/federation/enrollments" {
						require.Equal(t, "Bearer member-test-token", r.Header.Get("Authorization"))
						if mode == "inflight_enrollment" {
							select {
							case enrollmentStarted <- struct{}{}:
							default:
							}
							<-releaseEnrollment
						}
						if mode == "precommit_enrollment_failure" {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
					}
					if r.URL.Path == fmt.Sprintf("/api/v1/projects/%d/federation/relay:disconnect", root.private.ID) {
						credential, found, readErr := config.DefaultFederationCredentialStore().FederationCredential(r.Context(), root.private.UID)
						require.NoError(t, readErr)
						require.True(t, found)
						require.Equal(t, "Bearer "+credential.Token, r.Header.Get("Authorization"))
						disconnectCalls.Add(1)
						if mode == "signed_disconnect" && r.Header.Get("Signature") != "" {
							sawSignedDisconnect.Store(true)
						}
						select {
						case disconnectRequestStarted <- struct{}{}:
						default:
						}
					}
					req := r.Clone(r.Context())
					req.RequestURI = ""
					if mode == "signed_disconnect" {
						req.Header.Del("Signature")
						req.Header.Del("Signature-Input")
						req.Header.Del("Content-Digest")
					}
					req.URL.Scheme = upstreamURL.Scheme
					req.URL.Host = upstreamURL.Host
					req.Host = upstreamURL.Host
					//nolint:gosec // The destination is this test’s httptest loopback listener.
					response, callErr := root.server.Client().Do(req)
					require.NoError(t, callErr)
					defer func() { _ = response.Body.Close() }()
					raw, readErr := io.ReadAll(response.Body)
					require.NoError(t, readErr)
					if mode == "disconnect_write_race" && response.StatusCode == http.StatusOK && r.URL.Path == fmt.Sprintf("/api/v1/projects/%d/federation/relay:disconnect", root.private.ID) {
						select {
						case revokeResponseBlocked <- struct{}{}:
						default:
						}
						<-releaseRevokeResponse
					}
					if response.StatusCode == 200 && ((mode == "lost_response" && disconnectCalls.Load() == 1 && r.URL.Path == fmt.Sprintf("/api/v1/projects/%d/federation/relay:disconnect", root.private.ID)) || (mode == "pending_enrollment" && r.URL.Path == "/api/v1/federation/enrollments")) {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if mode == "credential_changed" && disconnectCalls.Load() == 1 && r.URL.Path == fmt.Sprintf("/api/v1/projects/%d/federation/relay:disconnect", root.private.ID) {
						credential, found, err := config.DefaultFederationCredentialStore().FederationCredential(r.Context(), root.private.UID)
						require.NoError(t, err)
						require.True(t, found)
						credential.Token = "concurrent-replacement-test-token"
						require.NoError(t, config.DefaultFederationCredentialStore().StoreFederationCredential(r.Context(), root.private.UID, credential))
					}
					for k, v := range response.Header {
						for _, item := range v {
							w.Header().Add(k, item)
						}
					}
					w.WriteHeader(response.StatusCode)
					_, _ = w.Write(raw)
				})
				var remote *httptest.Server
				if mode == "signed_disconnect" {
					remote = httptest.NewTLSServer(remoteHandler)
					previousTransport := http.DefaultTransport
					http.DefaultTransport = remote.Client().Transport
					t.Cleanup(func() { http.DefaultTransport = previousTransport })
				} else {
					remote = httptest.NewServer(remoteHandler)
				}
				t.Cleanup(remote.Close)
				localStore := store
				switch mode {
				case "partial_project_only", "partial_project_archived":
					localStore = &bridgeDisconnectPartialSetupStore{Storage: store, failSpokeBinding: true}
				case "partial_binding_without_relay":
					localStore = &bridgeDisconnectPartialSetupStore{Storage: store, failRelayConfig: true}
				}
				var credentialStore config.FederationCredentialStore = config.DefaultFederationCredentialStore()
				var leavePendingMarked <-chan struct{}
				if mode == "inflight_enrollment" {
					base := config.DefaultFederationCredentialStore()
					observed := &bridgeDisconnectObservedCredentials{
						FederationCredentialStore:            base,
						FederationCredentialReplacer:         base.(config.FederationCredentialReplacer),
						FederationCredentialRemover:          base.(config.FederationCredentialRemover),
						FederationRelayPendingMetadataReader: base.(config.FederationRelayPendingMetadataReader),
						pendingLeave:                         make(chan struct{}),
					}
					credentialStore = observed
					leavePendingMarked = observed.pendingLeave
				}
				local := daemon.NewServer(daemon.ServerConfig{DB: localStore, Auth: config.AuthConfig{Token: "local-owner-test-token"}, FederationCredentials: credentialStore, FederationCatalog: []config.CatalogDaemonConfig{{Name: "selected-hub", URL: remote.URL, InstanceUID: rootStore.InstanceUID(), Token: "member-test-token", AllowInsecure: true}}})
				t.Cleanup(func() { require.NoError(t, local.Close()) })
				endpoint := httptest.NewServer(local.Handler())
				t.Cleanup(endpoint.Close)
				request := projectAccessFixture{store: store, server: endpoint}
				headers := map[string]string{"Authorization": "Bearer local-owner-test-token"}
				if mode == "inflight_enrollment" {
					connectDone := make(chan bridgeDisconnectResponse, 1)
					go func() {
						code, _, raw := request.request(t, http.MethodPost, "/api/v1/federation/bridges", "", map[string]any{"hub_catalog": "selected-hub", "hub_project": root.private.Name, "project_name": "shared-replica", "actor": "local-member", "serve_downstream": false}, headers)
						connectDone <- bridgeDisconnectResponse{status: code, body: raw}
					}()
					select {
					case <-enrollmentStarted:
					case <-time.After(5 * time.Second):
						t.Fatal("connect did not reach relay enrollment")
					}
					disconnectDone := make(chan bridgeDisconnectResponse, 1)
					go func() {
						code, _, raw := request.request(t, http.MethodPost, "/api/v1/federation/bridges/shared-replica/disconnect", "", map[string]any{}, headers)
						disconnectDone <- bridgeDisconnectResponse{status: code, body: raw}
					}()
					select {
					case <-leavePendingMarked:
					case <-time.After(5 * time.Second):
						t.Fatal("disconnect did not mark the reserved candidate for leave")
					}
					select {
					case <-disconnectRequestStarted:
						t.Error("disconnect reached the hub before the in-flight enrollment drained")
					case <-time.After(100 * time.Millisecond):
					}
					releaseEnrollmentRequest()
					var connected, disconnected bridgeDisconnectResponse
					select {
					case connected = <-connectDone:
					case <-time.After(5 * time.Second):
						t.Fatal("connect did not finish after relay enrollment resumed")
					}
					select {
					case disconnected = <-disconnectDone:
					case <-time.After(5 * time.Second):
						t.Fatal("disconnect did not finish after relay enrollment drained")
					}
					require.Equal(t, http.StatusConflict, connected.status, string(connected.body))
					require.Equal(t, http.StatusOK, disconnected.status, string(disconnected.body))
					_, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), root.private.UID)
					require.NoError(t, err)
					require.False(t, found)
					_, err = store.ProjectByUID(t.Context(), root.private.UID)
					require.ErrorIs(t, err, db.ErrNotFound)
					grants, err := rootStore.ListFederationEnrollments(t.Context())
					require.NoError(t, err)
					require.Len(t, grants, 1)
					require.NotNil(t, grants[0].RevokedAt)
					require.EqualValues(t, 1, disconnectCalls.Load())
					return
				}
				code, _, raw := request.request(t, http.MethodPost, "/api/v1/federation/bridges", "", map[string]any{"hub_catalog": "selected-hub", "hub_project": root.private.Name, "project_name": "shared-replica", "actor": "local-member", "serve_downstream": false}, headers)
				expected := http.StatusOK
				switch mode {
				case "pending_enrollment", "precommit_enrollment_failure":
					expected = http.StatusServiceUnavailable
				case "partial_project_only", "partial_binding_without_relay", "partial_project_archived":
					expected = http.StatusInternalServerError
				}
				require.Equal(t, expected, code, string(raw))
				credential, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), root.private.UID)
				require.NoError(t, err)
				require.True(t, found)
				if mode == "signed_disconnect" {
					credential.Signing = &federationsigning.Source{
						KeyID: "bridge-signing-key", KeyEnv: "KATA_BRIDGE_SIGNING_KEY", HubURL: remote.URL,
					}
					require.NoError(t, config.DefaultFederationCredentialStore().StoreFederationCredential(t.Context(), root.private.UID, credential))
				}
				if mode == "archived" || mode == "archived_pending" || mode == "partial_project_archived" {
					project, err := store.ProjectByUID(t.Context(), root.private.UID)
					require.NoError(t, err)
					archiveContext := t.Context()
					if mode == "archived" {
						// Owner replay restores an already committed archive without
						// creating new ordinary deliveries. Keep binding + credential.
						archiveContext = db.WithRelayStateCapture(archiveContext)
					}
					_, _, err = store.RemoveProject(archiveContext, db.RemoveProjectParams{ProjectID: project.ID, Actor: "local-member", Force: true})
					require.NoError(t, err)
				}
				if mode == "parent_revoked" {
					parent, err := rootStore.ResolveAPIToken(t.Context(), "member-test-token")
					require.NoError(t, err)
					_, _, err = rootStore.RevokeAPIToken(t.Context(), parent.ID, "admin")
					require.NoError(t, err)
				}
				path := "/api/v1/federation/bridges/shared-replica/disconnect"
				code, _, raw = request.request(t, http.MethodPost, path, "", map[string]any{"preflight": true}, headers)
				if mode == "archived_pending" || mode == "partial_project_archived" {
					for _, preflight := range []bool{true, false} {
						if !preflight {
							code, _, raw = request.request(t, http.MethodPost, path, "", map[string]any{}, headers)
						}
						require.Equal(t, http.StatusConflict, code, string(raw))
						require.Contains(t, string(raw), "federation_lifecycle_blocked")
					}
					require.Zero(t, disconnectCalls.Load())
					retained, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), root.private.UID)
					require.NoError(t, err)
					require.True(t, found)
					require.True(t, credential.Equal(retained))
					project, err := store.ProjectByUID(t.Context(), root.private.UID)
					require.NoError(t, err)
					require.NotNil(t, project.DeletedAt)
					binding, bindingErr := store.FederationBindingByProject(t.Context(), project.ID)
					if mode == "partial_project_archived" {
						require.ErrorIs(t, bindingErr, db.ErrNotFound)
					} else {
						require.NoError(t, bindingErr)
						require.True(t, binding.Enabled)
					}
					grants, err := rootStore.ListFederationEnrollments(t.Context())
					require.NoError(t, err)
					require.Len(t, grants, 1)
					require.Nil(t, grants[0].RevokedAt)
					return
				}
				require.Equal(t, http.StatusOK, code, string(raw))
				require.Zero(t, disconnectCalls.Load())
				current, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), root.private.UID)
				require.NoError(t, err)
				require.True(t, found)
				require.True(t, credential.Equal(current))
				if mode == "disconnect_write_race" {
					disconnectDone := make(chan bridgeDisconnectResponse, 1)
					go func() {
						status, _, body := request.request(t, http.MethodPost, path, "", map[string]any{}, headers)
						disconnectDone <- bridgeDisconnectResponse{status: status, body: body}
					}()
					select {
					case <-revokeResponseBlocked:
					case <-time.After(5 * time.Second):
						t.Fatal("hub did not revoke the grant before the local write")
					}
					_, _, writeErr := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: root.private.ID, Author: "local-member", Title: "Write during disconnect"})
					releaseRevokeResponseRequest()
					select {
					case response := <-disconnectDone:
						code, raw = response.status, response.body
					case <-time.After(5 * time.Second):
						t.Fatal("disconnect did not finish after the hub response resumed")
					}
					require.ErrorIs(t, writeErr, db.ErrFederatedReadOnly, "disconnect must fence local writes before the hub grant is revoked")
				} else {
					code, _, raw = request.request(t, http.MethodPost, path, "", map[string]any{}, headers)
				}
				if mode == "credential_changed" {
					require.Equal(t, http.StatusConflict, code, string(raw))
					current, found, err = config.DefaultFederationCredentialStore().FederationCredential(t.Context(), root.private.UID)
					require.NoError(t, err)
					require.True(t, found)
					require.Equal(t, "concurrent-replacement-test-token", current.Token)
					project, err := store.ProjectByUID(t.Context(), root.private.UID)
					require.NoError(t, err)
					binding, err := store.FederationBindingByProject(t.Context(), project.ID)
					require.NoError(t, err)
					require.True(t, binding.Enabled)
					require.False(t, binding.PushEnabled)
					return
				}
				if mode == "lost_response" {
					require.Equal(t, http.StatusServiceUnavailable, code, string(raw))
					current, found, err = config.DefaultFederationCredentialStore().FederationCredential(t.Context(), root.private.UID)
					require.NoError(t, err)
					require.True(t, found)
					require.True(t, current.LeavePending)
					code, _, raw = request.request(t, http.MethodPost, path, "", map[string]any{}, headers)
				}
				require.Equal(t, http.StatusOK, code, string(raw))
				require.NotContains(t, string(raw), credential.Token)
				_, found, err = config.DefaultFederationCredentialStore().FederationCredential(t.Context(), root.private.UID)
				require.NoError(t, err)
				require.False(t, found)
				if mode != "pending_enrollment" && mode != "precommit_enrollment_failure" {
					project, err := store.ProjectByUID(t.Context(), root.private.UID)
					require.NoError(t, err)
					if mode == "archived" {
						require.NotNil(t, project.DeletedAt, "cleanup must preserve the restored archive")
					}
					_, err = store.FederationBindingByProject(t.Context(), project.ID)
					require.ErrorIs(t, err, db.ErrNotFound)
				}
				grants, err := rootStore.ListFederationEnrollments(t.Context())
				require.NoError(t, err)
				if mode == "precommit_enrollment_failure" {
					require.Empty(t, grants)
				} else {
					require.Len(t, grants, 1)
					require.NotNil(t, grants[0].RevokedAt)
				}
				beforeRetry := disconnectCalls.Load()
				code, _, raw = request.request(t, http.MethodPost, path, "", map[string]any{}, headers)
				require.Equal(t, http.StatusOK, code, string(raw))
				require.Equal(t, beforeRetry, disconnectCalls.Load(), "local response retry must not need another upstream call")
				if mode == "signed_disconnect" {
					require.True(t, sawSignedDisconnect.Load(), "disconnect must sign with the saved federation credential")
				}

			})
		})
	}
}
