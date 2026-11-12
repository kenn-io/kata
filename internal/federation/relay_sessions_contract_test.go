package federation_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/uid"
)

// relayRows compares committed rows, including delivery bookkeeping, without
// assuming PostgreSQL's nontransactional sequence allocations are gapless.
func relayRows(t *testing.T, store db.Storage) map[string][]string {
	t.Helper()
	reader := store.(interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	})
	result := make(map[string][]string)
	for _, table := range []string{"projects", "events", "cron_jobs", "cron_workflows", "cron_runs", "meta", "federation_bindings", "federation_sync_status", "federation_quarantine", "federation_enrollments", "federation_root_keys", "federation_event_provenance", "federation_entity_provenance", "federation_relay_outbox", "federation_relay_inbox", "federation_relay_cursors"} {
		rows, err := reader.QueryContext(t.Context(), "SELECT * FROM "+table)
		require.NoError(t, err)
		columns, err := rows.Columns()
		require.NoError(t, err)
		for rows.Next() {
			values := make([]any, len(columns))
			destinations := make([]any, len(columns))
			for i := range values {
				destinations[i] = &values[i]
			}
			require.NoError(t, rows.Scan(destinations...))
			for i, value := range values {
				if b, ok := value.([]byte); ok {
					values[i] = string(b)
				}
			}
			result[table] = append(result[table], fmt.Sprintf("%#v", values))
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		sort.Strings(result[table])
	}
	return result
}

func relaySessionHistory(t *testing.T, root *relayMatrixNode, clear, incomplete bool) {
	t.Helper()
	def := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"}
	job, _, err := root.store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: root.project.ID, Name: "Review", Actor: root.account, Definition: def})
	require.NoError(t, err)
	runUID, err := uid.New()
	require.NoError(t, err)
	summary := cron.Summary{Version: 1}
	if incomplete {
		yes := true
		summary.SessionsIncomplete = &yes
	} else {
		sessions := []cron.Session{{Agent: "claude", SessionID: "00000000-0000-4000-8000-000000000001"}}
		summary.Sessions = &sessions
	}
	in := db.ObserveCronRun{ProjectID: root.project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: root.account, Status: "succeeded", Summary: summary}
	observed, err := root.store.ObserveCronRun(t.Context(), in)
	require.NoError(t, err)
	if clear {
		in.ExpectedRevision = observed.Run.Revision
		in.Summary = cron.Summary{Version: 1}
		_, err = root.store.ObserveCronRun(t.Context(), in)
		require.NoError(t, err)
	}
}

func relaySessionPair(t *testing.T, backend string) (*relayMatrixNode, *relayMatrixNode) {
	t.Helper()
	root := newRelayMatrixNode(t, backend, "root-member")
	peer := newRelayMatrixNode(t, backend, "peer-member")
	project, err := root.store.CreateProject(t.Context(), "shared-project")
	require.NoError(t, err)
	root.project = project
	_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
	require.NoError(t, root.store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	enrollRelayMatrixReplica(t, root, peer, "peer-project", true)
	syncRelayMatrixNode(t, peer)
	return root, peer
}

func relaySessionGET(t *testing.T, node *relayMatrixNode, token, path, features string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, node.http.URL+path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	if features == "explicit-empty" {
		req.Header.Set(db.EventFeaturesHeader, "")
	} else if features != "" {
		req.Header.Set(db.EventFeaturesHeader, features)
	}
	resp, err := node.http.Client().Do(req)
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode, raw
}

func TestRelaySessionOfferRefusesBeforeEmissionAndRetries(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, tc := range []struct {
			name              string
			clear, incomplete bool
		}{{"retained-cleared", true, false}, {"incomplete-only", false, true}} {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				root, peer := relaySessionPair(t, backend)
				relaySessionHistory(t, root, tc.clear, tc.incomplete)
				path := fmt.Sprintf("/api/v1/projects/%d/federation/relay?stream=events&limit=100", root.project.ID)
				before := relayRows(t, root.store)
				for _, legacy := range []string{"", "explicit-empty"} {
					status, raw := relaySessionGET(t, root, peer.credential.Token, path, legacy)
					require.Equal(t, http.StatusConflict, status, string(raw))
					require.Contains(t, string(raw), "unsupported_event_features")
					require.NotContains(t, string(raw), "envelopes")
					require.Equal(t, before, relayRows(t, root.store))
				}
				status, raw := relaySessionGET(t, root, peer.credential.Token, path, db.SupportedEventFeatures)
				require.Equal(t, http.StatusOK, status, string(raw))
				var batch db.RelayBatch
				require.NoError(t, json.Unmarshal(raw, &batch))
				require.NotEmpty(t, batch.Envelopes)
				binding, err := peer.store.FederationBindingByProject(t.Context(), peer.project.ID)
				require.NoError(t, err)
				accepted, err := peer.store.AcceptRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, batch)
				require.NoError(t, err)
				require.NoError(t, root.store.AckRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, batch.Envelopes[0].Epoch, batch.Stream, accepted.Through, accepted.Digest))
			})
		}
	}
}

func relaySectionHasSessions(t *testing.T, section []byte) bool {
	t.Helper()
	var bodies [][]byte
	require.NoError(t, json.Unmarshal(section, &bodies))
	found := false
	for _, body := range bodies {
		event, err := db.DecodeRelaySourceEvent(body)
		require.NoError(t, err)
		features, err := db.PayloadRequiredFeatures(event.Type, string(event.Payload))
		require.NoError(t, err)
		if strings.Contains(features, db.CronRunSessionsEventFeature) {
			found = true
		}
	}
	return found
}

func TestRelaySessionResetRefusesWithoutWritesAndRetries(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, tc := range []struct {
			name                       string
			clear, cached, currentOnly bool
		}{{"fresh-history", true, false, false}, {"fresh-current-only", false, false, true}, {"cached-history", true, true, false}, {"cached-current-only", false, true, true}} {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				root, peer := relaySessionPair(t, backend)
				relaySessionHistory(t, root, tc.clear, false)
				if tc.currentOnly {
					query := "DELETE FROM events WHERE project_id=? AND type='cron.run.observed'"
					if backend == "postgres" {
						query = "DELETE FROM events WHERE project_id=$1 AND type='cron.run.observed'"
					}
					_, err := root.store.(interface {
						ExecContext(context.Context, string, ...any) (sql.Result, error)
					}).ExecContext(t.Context(), query, root.project.ID)
					require.NoError(t, err)
				}
				path := fmt.Sprintf("/api/v1/projects/%d/federation/relay/reset", root.project.ID)
				var cached []byte
				if tc.cached {
					status, raw := relaySessionGET(t, root, peer.credential.Token, path, db.SupportedEventFeatures)
					require.Equal(t, http.StatusOK, status, string(raw))
					cached = raw
				}
				before := relayRows(t, root.store)
				for _, legacy := range []string{"", "explicit-empty"} {
					status, raw := relaySessionGET(t, root, peer.credential.Token, path, legacy)
					require.Equal(t, http.StatusConflict, status, string(raw))
					require.Contains(t, string(raw), "unsupported_event_features")
					require.NotContains(t, string(raw), "snapshot_uid")
					require.NotContains(t, string(raw), "signature")
					require.Equal(t, before, relayRows(t, root.store))
				}
				status, raw := relaySessionGET(t, root, peer.credential.Token, path, db.SupportedEventFeatures)
				require.Equal(t, http.StatusOK, status, string(raw))
				var checkpoint db.RelayResetCheckpoint
				require.NoError(t, json.Unmarshal(raw, &checkpoint))
				pin, err := root.store.RootAuthority(t.Context(), root.project.UID)
				require.NoError(t, err)
				require.NoError(t, db.VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot))
				require.Equal(t, !tc.currentOnly, relaySectionHasSessions(t, checkpoint.Snapshot.Events), "signed history contains only retained evidence")
				require.Equal(t, !tc.clear, relaySectionHasSessions(t, checkpoint.Snapshot.Entities), "current signed state reflects summary replacement")
				if tc.cached {
					require.JSONEq(t, string(cached), string(raw), "retained signed checkpoint stays identical")
				}
			})
		}
	}
}

// Direct callers must receive the same pre-publication gate as the runner.
func TestRelaySessionDirectAcceptProbesBeforePost(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root, peer := relaySessionPair(t, backend)
			relaySessionHistory(t, root, true, false)
			binding, err := peer.store.FederationBindingByProject(t.Context(), peer.project.ID)
			require.NoError(t, err)
			envelopes, err := root.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamEvent, 100)
			require.NoError(t, err)
			require.NotEmpty(t, envelopes)
			batch := db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: envelopes}
			features, gets, posts := "", 0, 0
			var posted db.RelayBatch
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/federation/metadata") {
					gets++
					w.Header().Set(db.EventFeaturesHeader, features)
					_, _ = fmt.Fprintf(w, `{"project_id":42,"project_uid":%q}`, root.project.UID)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/relay:accept") {
					posts++
					require.NoError(t, json.NewDecoder(r.Body).Decode(&posted))
					_ = json.NewEncoder(w).Encode(db.RelayAcceptance{Through: envelopes[len(envelopes)-1].Sequence, Digest: envelopes[len(envelopes)-1].Digest})
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(hub.Close)
			client, err := federation.NewClient(t.Context(), hub.URL, "enrollment-token", clientpkg.Opts{})
			require.NoError(t, err)
			_, err = client.AcceptRelayDeliveries(t.Context(), 42, batch)
			require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
			require.Equal(t, 1, gets)
			require.Zero(t, posts)
			features = db.SupportedEventFeatures
			_, err = client.AcceptRelayDeliveries(t.Context(), 42, batch)
			require.NoError(t, err)
			require.Equal(t, 2, gets)
			require.Equal(t, 1, posts)
			require.Equal(t, batch, posted)
		})
	}
}

// Conversion seeds already legitimate direct-spoke history into the relay
// outbox. An old upstream must be rejected before that outbox is emitted.
func TestRelaySessionDirectConversionPublicationRetry(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, restore := range []bool{false, true} {
			for _, mode := range []string{"conversion", "rejoin"} {
				t.Run(fmt.Sprintf("%s/%s/restore=%t", backend, mode, restore), func(t *testing.T) {
					root := newRelayMatrixNode(t, backend, "root-member")
					peer := newRelayMatrixNode(t, backend, "peer-member")
					project, err := root.store.CreateProject(t.Context(), "shared-project")
					require.NoError(t, err)
					root.project = project
					_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
					require.NoError(t, err)
					public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
					require.NoError(t, root.store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
					handler := root.http.Config.Handler
					upgraded, posts := false, 0
					proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if strings.HasSuffix(r.URL.Path, "/relay:accept") {
							posts++
						}
						if !strings.HasSuffix(r.URL.Path, "/federation/metadata") {
							handler.ServeHTTP(w, r)
							return
						}
						recorder := httptest.NewRecorder()
						handler.ServeHTTP(recorder, r)
						for key, values := range recorder.Header() {
							for _, value := range values {
								w.Header().Add(key, value)
							}
						}
						w.Header().Del(db.EventFeaturesHeader)
						if upgraded {
							w.Header().Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
						}
						w.WriteHeader(recorder.Code)
						_, _ = w.Write(recorder.Body.Bytes())
					}))
					t.Cleanup(proxy.Close)
					root.http = proxy
					body, err := json.Marshal(map[string]any{"project_id": project.ID, "spoke_instance_uid": peer.store.InstanceUID(), "capabilities": "claim,pull,push", "relay": api.RelayEnrollmentOptions{ProtocolVersion: 1, ServeDownstream: true}})
					require.NoError(t, err)
					req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, root.http.URL+"/api/v1/federation/enrollments", bytes.NewReader(body))
					require.NoError(t, err)
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer "+root.userToken)
					response, err := root.http.Client().Do(req)
					require.NoError(t, err)
					raw, err := io.ReadAll(response.Body)
					require.NoError(t, err)
					require.NoError(t, response.Body.Close())
					require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
					var grant api.FederationEnrollmentOut
					require.NoError(t, json.Unmarshal(raw, &grant))
					require.NotNil(t, grant.Relay)
					if mode == "conversion" {
						adoptRelayMatrixReplica(t, root, peer, "peer-project", grant)
					} else {
						peer.project, err = peer.store.CreateProjectWithUID(t.Context(), "peer-project", project.UID)
						require.NoError(t, err)
						peer.credential = config.FederationCredential{HubURL: root.http.URL, HubProjectID: project.ID, Token: grant.Token, Actor: grant.Actor, Capabilities: grant.Capabilities}
					}
					relaySessionHistory(t, peer, true, false)
					joined, err := daemon.EnsureFederationReplica(t.Context(), peer.store, peer.credentials, nil, daemon.EnsureFederationReplicaParams{
						HubURL: root.http.URL, HubProjectID: project.ID, HubProjectUID: project.UID, ProjectName: peer.project.Name,
						Credential: peer.credential, PushEnabled: true, Relay: grant.Relay, RelayLocalActor: peer.account, RelayServeDownstream: true,
					})
					require.NoError(t, err)
					binding := joined.Binding
					sender := peer.store
					if restore {
						records := collectPreparedRelayBackupRecords(t, peer.store)
						sender = newRelayBackupRestoreStore(t, backend)
						require.NoError(t, sender.ImportReplay(t.Context(), records, db.ImportOptions{}))
						require.Equal(t, peer.store.InstanceUID(), sender.InstanceUID())
						binding, err = sender.FederationBindingByProject(t.Context(), peer.project.ID)
						require.NoError(t, err)
						require.Equal(t, collectRelayOutbox(t, peer.store), collectRelayOutbox(t, sender), "full replay retains pending relay rows")
					}
					before := relayRows(t, sender)
					outbox := collectRelayOutbox(t, sender)
					require.NotEmpty(t, outbox)
					err = federation.SyncFederationOnce(t.Context(), sender, binding, peer.credential)
					require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
					require.Zero(t, posts, "no incompatible accept is sent")
					require.Equal(t, before, relayRows(t, sender), "refusal retains un-emitted source rows and cursors")
					upgraded = true
					require.NoError(t, federation.SyncFederationOnce(t.Context(), sender, binding, peer.credential))
					require.Positive(t, posts)
					after := collectRelayOutbox(t, sender)
					for _, original := range outbox {
						found := false
						for _, current := range after {
							if current.ID == original.ID {
								require.Equal(t, original.SourceUID, current.SourceUID)
								require.Equal(t, original.SourceHash, current.SourceHash)
								require.Equal(t, original.Envelope, current.Envelope)
								found = true
								break
							}
						}
						require.True(t, found, "retry keeps each source identity")
					}
				})
			}
		}
	}
}

func TestReconcileSessionProbeClassificationAndGuidance(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		features string
		want     error
		hint     string
	}{
		{"unsupported", http.StatusOK, db.CronEventFeature, federation.ErrHubUnavailable, "hub"},
		{"unavailable", http.StatusServiceUnavailable, "", federation.ErrHubUnavailable, "hub"},
		{"credential rejected", http.StatusUnauthorized, "", federation.ErrConfigurationConflict, "credential"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openReconcileStore(t)
			project, err := store.CreateProject(t.Context(), "spoke-project")
			require.NoError(t, err)
			relaySessionHistory(t, &relayMatrixNode{store: store, project: project, account: "identity-user"}, false, true)
			hubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.status != http.StatusOK {
					http.Error(w, "private-hub-body", tc.status)
					return
				}
				w.Header().Set(db.EventFeaturesHeader, tc.features)
				_, _ = fmt.Fprintf(w, `{"project_id":42,"project_uid":%q}`, hubProjectUID)
			}))
			t.Cleanup(hubServer.Close)
			catalog := testCatalog()
			catalog.URL = hubServer.URL
			err = federation.ReconcileMapping(t.Context(), store, newFakeCredentialStore(), newFakeHub(), catalog, testMapping(), nil)
			require.ErrorIs(t, err, tc.want)
			require.Contains(t, strings.ToLower(err.Error()), tc.hint)
			require.NotContains(t, err.Error(), "private-hub-body")
			if tc.name == "credential rejected" {
				require.NotContains(t, err.Error(), "credential changed during local convergence")
			}
			_, err = store.FederationBindingByProject(t.Context(), project.ID)
			require.ErrorIs(t, err, db.ErrNotFound)
		})
	}
}

func TestRelaySessionOfferPageBoundary(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root, peer := relaySessionPair(t, backend)
			relaySessionHistory(t, root, true, false)
			binding, err := peer.store.FederationBindingByProject(t.Context(), peer.project.ID)
			require.NoError(t, err)
			path := fmt.Sprintf("/api/v1/projects/%d/federation/relay?stream=events&limit=1", root.project.ID)
			status, raw := relaySessionGET(t, root, peer.credential.Token, path, "")
			require.Equal(t, http.StatusOK, status, string(raw))
			var first db.RelayBatch
			require.NoError(t, json.Unmarshal(raw, &first))
			require.Len(t, first.Envelopes, 1)
			source, err := db.DecodeRelaySourceEvent(first.Envelopes[0].Body)
			require.NoError(t, err)
			features, err := db.PayloadRequiredFeatures(source.Type, string(source.Payload))
			require.NoError(t, err)
			require.NotContains(t, features, db.CronRunSessionsEventFeature)
			accepted, err := peer.store.AcceptRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, first)
			require.NoError(t, err)
			require.NoError(t, root.store.AckRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, first.Envelopes[0].Epoch, first.Stream, accepted.Through, accepted.Digest))
			before := relayRows(t, root.store)
			status, raw = relaySessionGET(t, root, peer.credential.Token, path, "")
			require.Equal(t, http.StatusConflict, status, string(raw))
			require.Contains(t, string(raw), "unsupported_event_features")
			require.Equal(t, before, relayRows(t, root.store))
			status, raw = relaySessionGET(t, root, peer.credential.Token, path, db.SupportedEventFeatures)
			require.Equal(t, http.StatusOK, status, string(raw))
			var second db.RelayBatch
			require.NoError(t, json.Unmarshal(raw, &second))
			require.Len(t, second.Envelopes, 1)
			source, err = db.DecodeRelaySourceEvent(second.Envelopes[0].Body)
			require.NoError(t, err)
			features, err = db.PayloadRequiredFeatures(source.Type, string(source.Payload))
			require.NoError(t, err)
			require.Contains(t, features, db.CronRunSessionsEventFeature)
		})
	}
}

func TestRelaySessionForwardedResetRefusesAndRetries(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, cached := range []bool{false, true} {
			for _, currentOnly := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/cached=%t/current-only=%t", backend, cached, currentOnly), func(t *testing.T) {
					root, relay := relaySessionPair(t, backend)
					relaySessionHistory(t, root, !currentOnly, false)
					syncRelayMatrixNode(t, relay)
					binding, err := relay.store.FederationBindingByProject(t.Context(), relay.project.ID)
					require.NoError(t, err)
					creator := root.store.(db.RelayResetStore)
					if currentOnly {
						query := "DELETE FROM events WHERE project_id=? AND type='cron.run.observed'"
						if backend == "postgres" {
							query = "DELETE FROM events WHERE project_id=$1 AND type='cron.run.observed'"
						}
						_, err = root.store.(interface {
							ExecContext(context.Context, string, ...any) (sql.Result, error)
						}).ExecContext(t.Context(), query, root.project.ID)
						require.NoError(t, err)
					}
					original, err := creator.CreateRelayReset(t.Context(), binding.RelayConfig.BindingUID, root.signer)
					require.NoError(t, err)
					query := "DELETE FROM events WHERE project_id=?"
					if backend == "postgres" {
						query = "DELETE FROM events WHERE project_id=$1"
					}
					_, err = root.store.(interface {
						ExecContext(context.Context, string, ...any) (sql.Result, error)
					}).ExecContext(t.Context(), query, root.project.ID)
					require.NoError(t, err)
					syncRelayMatrixNode(t, relay)
					leaf := newRelayMatrixNode(t, backend, "leaf-member")
					enrollRelayMatrixReplica(t, relay, leaf, "leaf-project", false)
					syncRelayMatrixNode(t, leaf)
					path := fmt.Sprintf("/api/v1/projects/%d/federation/relay/reset", relay.project.ID)
					var retained []byte
					if cached {
						status, raw := relaySessionGET(t, relay, leaf.credential.Token, path, db.SupportedEventFeatures)
						require.Equal(t, http.StatusOK, status, string(raw))
						retained = raw
					}
					before := relayRows(t, relay.store)
					status, raw := relaySessionGET(t, relay, leaf.credential.Token, path, "")
					require.Equal(t, http.StatusConflict, status, string(raw))
					require.Contains(t, string(raw), "unsupported_event_features")
					require.NotContains(t, string(raw), "snapshot_uid")
					require.Equal(t, before, relayRows(t, relay.store))
					status, raw = relaySessionGET(t, relay, leaf.credential.Token, path, db.SupportedEventFeatures)
					require.Equal(t, http.StatusOK, status, string(raw))
					var forwarded db.RelayResetCheckpoint
					require.NoError(t, json.Unmarshal(raw, &forwarded))
					require.Equal(t, original.Manifest, forwarded.Manifest)
					require.Equal(t, original.Snapshot, forwarded.Snapshot)
					require.Equal(t, !currentOnly, relaySectionHasSessions(t, forwarded.Snapshot.Events))
					require.Equal(t, currentOnly, relaySectionHasSessions(t, forwarded.Snapshot.Entities))
					pin, err := root.store.RootAuthority(t.Context(), root.project.UID)
					require.NoError(t, err)
					require.NoError(t, db.VerifyRootResetManifest(pin, forwarded.Manifest, forwarded.Snapshot))
					if cached {
						require.JSONEq(t, string(retained), string(raw))
					}
				})
			}
		}
	}
}

// A post-checkpoint observation must be gated in the newly requested epoch;
// refusal also rolls back the grant activation that the request would perform.
func TestRelaySessionNewEpochOfferRefusal(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root, peer := relaySessionPair(t, backend)
			binding, err := peer.store.FederationBindingByProject(t.Context(), peer.project.ID)
			require.NoError(t, err)
			checkpoint, err := root.store.(db.RelayResetStore).CreateRelayReset(t.Context(), binding.RelayConfig.BindingUID, root.signer)
			require.NoError(t, err)
			require.NoError(t, peer.store.(db.RelayResetStore).InstallRelayReset(t.Context(), binding.RelayConfig.BindingUID, checkpoint.Manifest, checkpoint.Snapshot, checkpoint.Translation))
			relaySessionHistory(t, root, false, false)
			path := fmt.Sprintf("/api/v1/projects/%d/federation/relay?stream=events&limit=100&epoch=%d", root.project.ID, checkpoint.Translation.Authority.Epoch)
			before := relayRows(t, root.store)
			status, raw := relaySessionGET(t, root, peer.credential.Token, path, "")
			require.Equal(t, http.StatusConflict, status, string(raw))
			require.Contains(t, string(raw), "unsupported_event_features")
			require.NotContains(t, string(raw), "envelopes")
			require.Equal(t, before, relayRows(t, root.store))
			status, raw = relaySessionGET(t, root, peer.credential.Token, path, db.SupportedEventFeatures)
			require.Equal(t, http.StatusOK, status, string(raw))
			var batch db.RelayBatch
			require.NoError(t, json.Unmarshal(raw, &batch))
			require.NotEmpty(t, batch.Envelopes)
			found := false
			for _, envelope := range batch.Envelopes {
				require.Equal(t, checkpoint.Translation.Authority.Epoch, envelope.Epoch)
				event, err := db.DecodeRelaySourceEvent(envelope.Body)
				require.NoError(t, err)
				features, err := db.PayloadRequiredFeatures(event.Type, string(event.Payload))
				require.NoError(t, err)
				found = found || strings.Contains(features, db.CronRunSessionsEventFeature)
			}
			require.True(t, found)
			accepted, err := peer.store.AcceptRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, batch)
			require.NoError(t, err)
			require.NoError(t, root.store.AckRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, checkpoint.Translation.Authority.Epoch, batch.Stream, accepted.Through, accepted.Digest))
		})
	}
}

// Session-free event batches must not add a metadata request to direct accept.
func TestRelaySessionFreeDirectAcceptDoesNotProbe(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root, peer := relaySessionPair(t, backend)
			_, _, err := root.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: root.project.ID, Author: root.account, Title: "Session-free event"})
			require.NoError(t, err)
			binding, err := peer.store.FederationBindingByProject(t.Context(), peer.project.ID)
			require.NoError(t, err)
			envelopes, err := root.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamEvent, 100)
			require.NoError(t, err)
			require.NotEmpty(t, envelopes)
			gets, posts := 0, 0
			var posted db.RelayBatch
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					gets++
					http.Error(w, "unexpected metadata probe", http.StatusInternalServerError)
					return
				}
				posts++
				require.NoError(t, json.NewDecoder(r.Body).Decode(&posted))
				_ = json.NewEncoder(w).Encode(db.RelayAcceptance{Through: envelopes[len(envelopes)-1].Sequence, Digest: envelopes[len(envelopes)-1].Digest})
			}))
			t.Cleanup(hub.Close)
			client, err := federation.NewClient(t.Context(), hub.URL, "enrollment-token", clientpkg.Opts{})
			require.NoError(t, err)
			batch := db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: envelopes}
			_, err = client.AcceptRelayDeliveries(t.Context(), 42, batch)
			require.NoError(t, err)
			require.Zero(t, gets)
			require.Equal(t, 1, posts)
			require.Equal(t, batch, posted)
		})
	}
}

// Independent receipt and artifact streams remain usable by old peers even
// while a session-bearing event is pending on the event stream.
func TestRelaySessionLegacyReceiptAndArtifactOffers(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root, peer := relaySessionPair(t, backend)
			issue, _, err := root.store.CreateIssue(db.WithRootAttribution(t.Context(), root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Author: root.account, Title: "Artifact source"})
			require.NoError(t, err)
			artifact := relayBackupArtifact(t, root, issue)
			_, err = root.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(t.Context(), artifact)
			require.NoError(t, err)
			relaySessionHistory(t, root, false, false)
			for _, stream := range []string{db.RelayStreamReceipt, db.RelayStreamArtifact} {
				path := fmt.Sprintf("/api/v1/projects/%d/federation/relay?stream=%s&limit=100", root.project.ID, stream)
				status, raw := relaySessionGET(t, root, peer.credential.Token, path, "")
				require.Equal(t, http.StatusOK, status, string(raw))
				var batch db.RelayBatch
				require.NoError(t, json.Unmarshal(raw, &batch))
				require.Equal(t, stream, batch.Stream)
				require.NotEmpty(t, batch.Envelopes)
			}
		})
	}
}
