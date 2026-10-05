package daemon_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
)

// R3/R9/R10: preview is read-only, resolves only the selected catalog user
// credential and project, and rejects an unsupported peer before enrollment.
func TestFederationBridgeCatalogPreflight(t *testing.T) {
	for _, mode := range []string{"normal", "lost_response", "revoked_retry", "credential_changed", "leave_overlap", "parent_rebind"} {
		t.Run(mode, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				credentials := newReplicaCredentialStore()
				rootStore := openReplicaServiceStore(t)
				root := newProjectAccessFixture(t, rootStore)
				_, err := rootStore.EnableProjectFederation(t.Context(), root.private.ID, "admin")
				require.NoError(t, err)
				public, _, err := ed25519.GenerateKey(nil)
				require.NoError(t, err)
				pin := db.RootKeyPin{ProjectUID: root.private.UID, AuthorityUID: rootStore.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
				require.NoError(t, rootStore.PinRootAuthority(t.Context(), pin))
				var enrollmentCalls, wrongCredentials atomic.Int32
				var unsupported atomic.Bool
				remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer member-test-token" && (mode != "parent_rebind" || r.Header.Get("Authorization") != "Bearer replacement-member-test-token") {
						wrongCredentials.Add(1)
					}
					if r.URL.Path == "/api/v1/federation/enrollments" {
						enrollmentCalls.Add(1)
					}
					if r.URL.Path == "/api/v1/instance" && unsupported.Load() {
						w.Header().Set("Content-Type", "application/json")
						_, _ = fmt.Fprintf(w, `{"instance_uid":%q,"version":"old","schema_version":34,"auth":{"kind":"api_token","actor":"member"}}`, rootStore.InstanceUID())
						return
					}
					// The fixture's existing server preserves the full authentication and
					// project middleware. A reverse proxy forwards only this selected request.
					req := r.Clone(r.Context())
					req.RequestURI = ""
					req.URL.Scheme = "http"
					req.URL.Host = root.server.Listener.Addr().String()
					//nolint:gosec // The destination is this test’s httptest loopback listener.
					response, callErr := root.server.Client().Do(req)
					if callErr != nil {
						http.Error(w, "fixture forwarding failed", 500)
						return
					}
					defer func() { _ = response.Body.Close() }()
					if r.URL.Path == "/api/v1/federation/enrollments" && enrollmentCalls.Load() == 1 {
						switch mode {
						case "lost_response", "revoked_retry":
							_, _ = io.Copy(io.Discard, response.Body)
							if mode == "revoked_retry" {
								_, revokeErr := rootStore.SetTeamMembership(r.Context(), root.team.UID, "member", false, "admin")
								if revokeErr != nil {
									http.Error(w, "fixture revoke failed", 500)
									return
								}
							}
							http.Error(w, "lost enrollment response", http.StatusServiceUnavailable)
							return
						case "credential_changed":
							candidate, _, _ := credentials.FederationCredential(r.Context(), root.private.UID)
							candidate.Token = "replacement-narrow-test-token"
							_ = credentials.StoreFederationCredential(r.Context(), root.private.UID, candidate)
						}
					}
					for key, values := range response.Header {
						for _, value := range values {
							w.Header().Add(key, value)
						}
					}
					w.WriteHeader(response.StatusCode)
					_, _ = io.Copy(w, response.Body)
				}))
				t.Cleanup(remote.Close)
				smoke, err := http.NewRequestWithContext(t.Context(), http.MethodGet, remote.URL+"/api/v1/instance", nil)
				require.NoError(t, err)
				smoke.Header.Set("Authorization", "Bearer member-test-token")
				response, err := remote.Client().Do(smoke)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, response.StatusCode)
				var current struct {
					Auth struct {
						Actor string `json:"actor"`
					} `json:"auth"`
				}
				require.NoError(t, json.UnmarshalRead(response.Body, &current))
				require.NoError(t, response.Body.Close())
				require.Equal(t, "member", current.Auth.Actor)
				localStore := store
				var started *relayReplicaBindingStartedStore
				if mode == "leave_overlap" {
					started = &relayReplicaBindingStartedStore{relayReplicaStartedStore: &relayReplicaStartedStore{Storage: store, started: make(chan struct{})}, bindingWritten: make(chan struct{})}
					localStore = started
				}
				localServer := daemon.NewServer(daemon.ServerConfig{DB: localStore, Auth: config.AuthConfig{Token: "local-owner-test-token"}, FederationCredentials: credentials, FederationCatalog: []config.CatalogDaemonConfig{
					{Name: "selected-hub", URL: remote.URL, InstanceUID: rootStore.InstanceUID(), Token: "member-test-token", AllowInsecure: true},
					{Name: "unrelated-hub", URL: "https://unrelated.example", Token: "unrelated-test-token"},
				}})
				t.Cleanup(func() { require.NoError(t, localServer.Close()) })
				localHTTP := httptest.NewServer(localServer.Handler())
				t.Cleanup(localHTTP.Close)
				request := projectAccessFixture{store: store, server: localHTTP}
				body := map[string]any{"hub_catalog": "selected-hub", "hub_project": "restricted-project", "project_name": "shared-replica", "actor": "local-member", "preflight": true}
				code, _, raw := request.request(t, http.MethodPost, "/api/v1/federation/bridges", "", body, map[string]string{"Authorization": "Bearer local-owner-test-token"})
				require.Equal(t, http.StatusOK, code, string(raw))
				var preview map[string]any
				require.NoError(t, json.Unmarshal(raw, &preview))
				require.Equal(t, "bidirectional", preview["direction"])
				require.Equal(t, rootStore.InstanceUID(), preview["hub_instance_uid"])
				require.Equal(t, root.private.UID, preview["hub_project_uid"])
				require.Equal(t, "member", preview["upstream_account"])
				require.Zero(t, enrollmentCalls.Load())
				require.Zero(t, wrongCredentials.Load())
				require.NotContains(t, string(raw), "member-test-token")
				require.NotContains(t, string(raw), "unrelated-test-token")
				_, err = store.ProjectByName(t.Context(), "shared-replica")
				require.ErrorIs(t, err, db.ErrNotFound)
				body["preflight"] = false
				code, _, raw = request.request(t, http.MethodPost, "/api/v1/federation/bridges", "", body, map[string]string{"Authorization": "Bearer local-owner-test-token"})
				if mode == "credential_changed" {
					require.Equal(t, http.StatusConflict, code, string(raw))
					candidate, found, err := credentials.FederationCredential(t.Context(), root.private.UID)
					require.NoError(t, err)
					require.True(t, found)
					require.Equal(t, "replacement-narrow-test-token", candidate.Token)
					_, err = store.ProjectByUID(t.Context(), root.private.UID)
					require.ErrorIs(t, err, db.ErrNotFound)
					return
				}
				if mode == "lost_response" || mode == "revoked_retry" {
					require.Equal(t, http.StatusServiceUnavailable, code, string(raw))
					candidate, found, err := credentials.FederationCredential(t.Context(), root.private.UID)
					require.NoError(t, err)
					require.True(t, found)
					require.True(t, candidate.RelayEnrollmentPending)
					_, err = store.ProjectByUID(t.Context(), root.private.UID)
					require.ErrorIs(t, err, db.ErrNotFound)
					code, _, raw = request.request(t, http.MethodPost, "/api/v1/federation/bridges", "", body, map[string]string{"Authorization": "Bearer local-owner-test-token"})
					if mode == "revoked_retry" {
						require.Equal(t, http.StatusNotFound, code, string(raw))
						require.EqualValues(t, 1, enrollmentCalls.Load())
						after, found, err := credentials.FederationCredential(t.Context(), root.private.UID)
						require.NoError(t, err)
						require.True(t, found)
						require.True(t, after.Equal(candidate))
						_, err = store.ProjectByUID(t.Context(), root.private.UID)
						require.ErrorIs(t, err, db.ErrNotFound)
						return
					}
					after, found, err := credentials.FederationCredential(t.Context(), root.private.UID)
					require.NoError(t, err)
					require.True(t, found)
					require.Equal(t, candidate.Token, after.Token)
				}
				require.Equal(t, http.StatusOK, code, string(raw))
				project, err := store.ProjectByUID(t.Context(), root.private.UID)
				require.NoError(t, err)
				require.Equal(t, "shared-replica", project.Name)
				binding, err := store.FederationBindingByProject(t.Context(), project.ID)
				require.NoError(t, err)
				require.True(t, binding.PushEnabled)
				require.NotNil(t, binding.RelayConfig)
				require.Equal(t, "local-member", binding.RelayConfig.LocalActor)
				require.Equal(t, "member", binding.Actor)
				credential, found, err := credentials.FederationCredential(t.Context(), project.UID)
				require.NoError(t, err)
				require.True(t, found)
				require.False(t, credential.RelayEnrollmentPending)
				require.NotEmpty(t, credential.Token)
				require.NotContains(t, string(raw), credential.Token)
				enrollments, err := rootStore.ListFederationEnrollments(t.Context())
				require.NoError(t, err)
				require.Len(t, enrollments, 1)
				require.Equal(t, "member", enrollments[0].Actor)
				require.Equal(t, "claim,pull,push", enrollments[0].Capabilities)
				if mode == "parent_rebind" {
					before := enrollments[0]
					_, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Work retained through credential replacement", Author: "local-member"})
					require.NoError(t, err)
					pending, err := store.PendingRelayDeliveries(t.Context(), before.RelayBindingUID, db.RelayStreamEvent, 32)
					require.NoError(t, err)
					require.NotEmpty(t, pending)
					require.NotNil(t, before.ParentTokenID)
					_, _, err = rootStore.CreateAPIToken(t.Context(), db.CreateAPITokenParams{Actor: "member", AdminActor: "admin", PlaintextToken: "replacement-member-test-token"})
					require.NoError(t, err)
					_, _, err = rootStore.RevokeAPIToken(t.Context(), *before.ParentTokenID, "admin")
					require.NoError(t, err)
					replacementServer := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "local-owner-test-token"}, FederationCredentials: credentials, FederationCatalog: []config.CatalogDaemonConfig{{Name: "selected-hub", URL: remote.URL, InstanceUID: rootStore.InstanceUID(), Token: "replacement-member-test-token", AllowInsecure: true}}})
					t.Cleanup(func() { require.NoError(t, replacementServer.Close()) })
					replacementHTTP := httptest.NewServer(replacementServer.Handler())
					t.Cleanup(replacementHTTP.Close)
					replacementRequest := projectAccessFixture{store: store, server: replacementHTTP}
					code, _, raw = replacementRequest.request(t, http.MethodPost, "/api/v1/federation/bridges", "", body, map[string]string{"Authorization": "Bearer local-owner-test-token"})
					require.Equal(t, http.StatusOK, code, string(raw))
					after, found, err := credentials.FederationCredential(t.Context(), project.UID)
					require.NoError(t, err)
					require.True(t, found)
					require.Equal(t, credential.Token, after.Token)
					grants, err := rootStore.ListFederationEnrollments(t.Context())
					require.NoError(t, err)
					require.Len(t, grants, 1)
					require.Equal(t, before.ID, grants[0].ID)
					require.Equal(t, before.RelayBindingUID, grants[0].RelayBindingUID)
					require.Equal(t, before.RelayResetEpoch, grants[0].RelayResetEpoch)
					require.NotEqual(t, before.ParentTokenID, grants[0].ParentTokenID)
					pendingAfter, err := store.PendingRelayDeliveries(t.Context(), before.RelayBindingUID, db.RelayStreamEvent, 32)
					require.NoError(t, err)
					require.Equal(t, pending, pendingAfter)
					require.Zero(t, wrongCredentials.Load())
					return
				}
				if mode == "leave_overlap" {
					finish, err := federationcoord.BeginSync(t.Context(), federationcoord.Key(store.InstanceUID(), project.ID), store, project.ID)
					require.NoError(t, err)
					var once sync.Once
					release := func() { once.Do(finish) }
					defer release()
					started.bindingOnce = sync.Once{}
					started.bindingWritten = make(chan struct{})
					data, err := json.Marshal(body)
					require.NoError(t, err)
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					req, err := http.NewRequestWithContext(ctx, http.MethodPost, localHTTP.URL+"/api/v1/federation/bridges", bytes.NewReader(data))
					require.NoError(t, err)
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer local-owner-test-token")
					type outcome struct {
						code int
						body []byte
						err  error
					}
					done := make(chan outcome, 1)
					go func() {
						response, err := localHTTP.Client().Do(req)
						if err != nil {
							done <- outcome{err: err}
							return
						}
						raw, err := io.ReadAll(response.Body)
						_ = response.Body.Close()
						done <- outcome{code: response.StatusCode, body: raw, err: err}
					}()
					select {
					case <-started.bindingWritten:
					case <-time.After(5 * time.Second):
						t.Fatal("connect did not reach transport drain")
					}
					// A distinct project completes only after connect released the
					// global mutex for its drain, so leave begins in that window.
					other := replicaServiceParams()
					other.HubProjectUID = "00000000000000000000000009"
					other.ProjectName = "unrelated-replica"
					_, err = daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, other)
					require.NoError(t, err)
					_, err = daemon.PrepareFederationReplicaLeave(t.Context(), store, credentials, project.ID)
					require.NoError(t, err)
					release()
					select {
					case result := <-done:
						require.NoError(t, result.err)
						require.Equal(t, http.StatusConflict, result.code, string(result.body))
						require.Contains(t, string(result.body), "federation_credential_conflict")
					case <-time.After(5 * time.Second):
						t.Fatal("connect did not finish after transport drain")
					}
					retained, found, err := credentials.FederationCredential(t.Context(), project.UID)
					require.NoError(t, err)
					require.True(t, found)
					require.True(t, retained.Equal(credential))
					return
				}
				// Repeating connect retains the same narrow token and enrollment.
				code, _, raw = request.request(t, http.MethodPost, "/api/v1/federation/bridges", "", body, map[string]string{"Authorization": "Bearer local-owner-test-token"})
				require.Equal(t, http.StatusOK, code, string(raw))
				after, found, err := credentials.FederationCredential(t.Context(), project.UID)
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, credential.Token, after.Token)
				enrollments, err = rootStore.ListFederationEnrollments(t.Context())
				require.NoError(t, err)
				require.Len(t, enrollments, 1)
				callsBeforeUnsupported := enrollmentCalls.Load()
				unsupported.Store(true)
				body["preflight"] = false
				code, _, raw = request.request(t, http.MethodPost, "/api/v1/federation/bridges", "", body, map[string]string{"Authorization": "Bearer local-owner-test-token"})
				require.Equal(t, http.StatusConflict, code, string(raw))
				require.Contains(t, string(raw), "unsupported_relay_protocol")
				require.Equal(t, callsBeforeUnsupported, enrollmentCalls.Load(), "unsupported peer must not receive enrollment")
			})
		})
	}
}
