package federation_test

import (
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
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
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

func relaySessionHistory(t *testing.T, root *relayMatrixNode, clearSummary, incomplete bool) {
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
	if clearSummary {
		in.ExpectedRevision = observed.Run.Revision
		in.Summary = cron.Summary{Version: 1}
		_, err = root.store.ObserveCronRun(t.Context(), in)
		require.NoError(t, err)
	}
}

func relaySessionPinRoot(t *testing.T, root *relayMatrixNode, project db.Project) {
	t.Helper()
	public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
	require.NoError(t, root.store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
}

// relaySessionPair serves a hub root and one enrolled, synced downstream peer.
func relaySessionPair(t *testing.T, backend string) (*relayMatrixNode, *relayMatrixNode) {
	t.Helper()
	root := newRelayMatrixNode(t, backend, "root-member")
	project, err := root.store.CreateProject(t.Context(), "shared-project")
	require.NoError(t, err)
	return root, relaySessionPeer(t, backend, root, project)
}

func relaySessionPeer(t *testing.T, backend string, root *relayMatrixNode, project db.Project) *relayMatrixNode {
	t.Helper()
	peer := newRelayMatrixNode(t, backend, "peer-member")
	root.project = project
	_, err := root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	relaySessionPinRoot(t, root, project)
	enrollRelayMatrixReplica(t, root, peer, "peer-project", true)
	syncRelayMatrixNode(t, peer)
	return peer
}

// relaySessionRestoredPair serves a hub whose current run row carries sessions
// but whose retained history has no observation event. That is the state the
// production ImportReplay route produces for historical rows that predate
// observation events (see PrepareCronAdoption), so it replaces row deletion.
func relaySessionRestoredPair(t *testing.T, backend string) (*relayMatrixNode, *relayMatrixNode) {
	t.Helper()
	source := openRelayMatrixStore(t, backend, false)
	sourceProject, err := source.CreateProject(t.Context(), "shared-project")
	require.NoError(t, err)
	relaySessionHistory(t, &relayMatrixNode{store: source, project: sourceProject, account: "root-member"}, false, false)
	records, err := dbtest.CollectImportRecords(t.Context(), source, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	var retained []db.ImportRecord
	for _, record := range records {
		if event, ok := record.(*db.EventExport); ok && event.Type == "cron.run.observed" {
			continue
		}
		retained = append(retained, record)
	}
	restored := newRelayBackupRestoreStore(t, backend)
	require.NoError(t, restored.ImportReplay(t.Context(), retained, db.ImportOptions{}))
	project, err := restored.ProjectByUID(t.Context(), sourceProject.UID)
	require.NoError(t, err)
	root := newRelayMatrixNodeFromStore(t, restored, "root-member")
	return root, relaySessionPeer(t, backend, root, project)
}

func relaySessionGET(t *testing.T, node *relayMatrixNode, token, path, features string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, node.http.URL+path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	// HTTP cannot distinguish an absent header from an empty one, so the legacy
	// request is the one without the advertisement.
	if features != "" {
		req.Header.Set(db.EventFeaturesHeader, features)
	}
	resp, err := node.http.Client().Do(req)
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode, raw
}

// relaySessionRefuseThenServe sends the legacy request, expects a refusal that
// leaves every committed row unchanged and discloses none of the forbidden
// markers, then returns the body served once the peer advertises the features.
func relaySessionRefuseThenServe(t *testing.T, node *relayMatrixNode, token, path string, forbidden ...string) []byte {
	t.Helper()
	before := relayRows(t, node.store)
	status, raw := relaySessionGET(t, node, token, path, "")
	require.Equal(t, http.StatusConflict, status, string(raw))
	require.Contains(t, string(raw), "unsupported_event_features")
	for _, marker := range forbidden {
		require.NotContains(t, string(raw), marker)
	}
	require.Equal(t, before, relayRows(t, node.store))
	status, raw = relaySessionGET(t, node, token, path, db.SupportedEventFeatures)
	require.Equal(t, http.StatusOK, status, string(raw))
	return raw
}

func relaySessionAcceptAndAck(t *testing.T, root, peer *relayMatrixNode, batch db.RelayBatch) {
	t.Helper()
	binding, err := peer.store.FederationBindingByProject(t.Context(), peer.project.ID)
	require.NoError(t, err)
	accepted, err := peer.store.AcceptRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, batch)
	require.NoError(t, err)
	require.NoError(t, root.store.AckRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, batch.Envelopes[0].Epoch, batch.Stream, accepted.Through, accepted.Digest))
}

func relayBodiesHaveSessions(t *testing.T, bodies [][]byte) bool {
	t.Helper()
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

func relayEnvelopesHaveSessions(t *testing.T, envelopes []db.RelayEnvelope) bool {
	t.Helper()
	bodies := make([][]byte, 0, len(envelopes))
	for _, envelope := range envelopes {
		bodies = append(bodies, envelope.Body)
	}
	return relayBodiesHaveSessions(t, bodies)
}

func relaySectionHasSessions(t *testing.T, section []byte) bool {
	t.Helper()
	var bodies [][]byte
	require.NoError(t, json.Unmarshal(section, &bodies))
	return relayBodiesHaveSessions(t, bodies)
}

func TestRelaySessionOfferRefusesBeforeEmissionAndRetries(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, tc := range []struct {
			name              string
			clear, incomplete bool
			limit             int
		}{{"retained-cleared", true, false, 100}, {"incomplete-only", false, true, 100}, {"page-boundary", true, false, 1}} {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				root, peer := relaySessionPair(t, backend)
				relaySessionHistory(t, root, tc.clear, tc.incomplete)
				path := fmt.Sprintf("/api/v1/projects/%d/federation/relay?stream=events&limit=%d", root.project.ID, tc.limit)
				if tc.limit == 1 {
					// A legacy peer still receives the session-free page before the retained session event.
					status, raw := relaySessionGET(t, root, peer.credential.Token, path, "")
					require.Equal(t, http.StatusOK, status, string(raw))
					var first db.RelayBatch
					require.NoError(t, json.Unmarshal(raw, &first))
					require.Len(t, first.Envelopes, 1)
					require.False(t, relayEnvelopesHaveSessions(t, first.Envelopes))
					relaySessionAcceptAndAck(t, root, peer, first)
				}
				var batch db.RelayBatch
				require.NoError(t, json.Unmarshal(relaySessionRefuseThenServe(t, root, peer.credential.Token, path, "envelopes"), &batch))
				require.NotEmpty(t, batch.Envelopes)
				if tc.limit == 1 {
					require.Len(t, batch.Envelopes, 1)
					require.True(t, relayEnvelopesHaveSessions(t, batch.Envelopes))
				}
				relaySessionAcceptAndAck(t, root, peer, batch)
			})
		}
	}
}

func TestRelaySessionResetRefusesWithoutWritesAndRetries(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, tc := range []struct {
			name                string
			cached, currentOnly bool
		}{{"fresh-history", false, false}, {"fresh-current-only", false, true}, {"cached-history", true, false}, {"cached-current-only", true, true}} {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				var root, peer *relayMatrixNode
				if tc.currentOnly {
					root, peer = relaySessionRestoredPair(t, backend)
				} else {
					root, peer = relaySessionPair(t, backend)
					relaySessionHistory(t, root, true, false)
				}
				path := fmt.Sprintf("/api/v1/projects/%d/federation/relay/reset", root.project.ID)
				var cached []byte
				if tc.cached {
					status, raw := relaySessionGET(t, root, peer.credential.Token, path, db.SupportedEventFeatures)
					require.Equal(t, http.StatusOK, status, string(raw))
					cached = raw
				}
				raw := relaySessionRefuseThenServe(t, root, peer.credential.Token, path, "snapshot_uid", "signature")
				var checkpoint db.RelayResetCheckpoint
				require.NoError(t, json.Unmarshal(raw, &checkpoint))
				pin, err := root.store.RootAuthority(t.Context(), root.project.UID)
				require.NoError(t, err)
				require.NoError(t, db.VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot))
				require.Equal(t, !tc.currentOnly, relaySectionHasSessions(t, checkpoint.Snapshot.Events), "signed history contains only retained evidence")
				require.Equal(t, tc.currentOnly, relaySectionHasSessions(t, checkpoint.Snapshot.Entities), "current signed state reflects summary replacement")
				if tc.cached {
					require.JSONEq(t, string(cached), string(raw), "retained signed checkpoint stays identical")
				}
			})
		}
	}
}

// readOnlyAdoptionStore reports the existing read-only adoption result so the
// reconciler's operator guidance can be asserted without a relay adoption flow.
type readOnlyAdoptionStore struct{ dbtest.TransactionalStore }

func (readOnlyAdoptionStore) AdoptProjectIntoFederation(context.Context, db.AdoptProjectIntoFederationParams) (db.AdoptProjectIntoFederationResult, error) {
	return db.AdoptProjectIntoFederationResult{}, db.ErrFederatedReadOnly
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
					hubBinding, err := root.store.EnableProjectFederation(t.Context(), project.ID, root.account)
					require.NoError(t, err)
					relaySessionPinRoot(t, root, project)
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
						// Before the upgrade the hub is a legacy one; afterwards its own
						// production advertisement passes through unchanged.
						if !upgraded {
							w.Header().Del(db.EventFeaturesHeader)
						}
						w.WriteHeader(recorder.Code)
						_, _ = w.Write(recorder.Body.Bytes())
					}))
					t.Cleanup(proxy.Close)
					root.http = proxy
					status, raw := relayMatrixRequest(t, root, http.MethodPost, "/api/v1/federation/enrollments", root.userToken, map[string]any{"project_id": project.ID, "spoke_instance_uid": peer.store.InstanceUID(), "capabilities": "claim,pull,push", "relay": api.RelayEnrollmentOptions{ProtocolVersion: 1, ServeDownstream: true}})
					require.Equal(t, http.StatusOK, status, string(raw))
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
						HubURL: root.http.URL, HubProjectID: project.ID, HubProjectUID: project.UID, ProjectName: peer.project.Name, ReplayHorizonEventID: hubBinding.ReplayHorizonEventID,
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
					// SyncFederationOnce records push start and errors in this operational table.
					delete(before, "federation_sync_status")
					outbox := collectRelayOutbox(t, sender)
					require.NotEmpty(t, outbox)
					err = federation.SyncFederationOnce(t.Context(), sender, binding, peer.credential)
					require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
					require.Zero(t, posts, "no incompatible accept is sent")
					afterRefusal := relayRows(t, sender)
					delete(afterRefusal, "federation_sync_status")
					require.Equal(t, before, afterRefusal, "refusal retains un-emitted source rows and cursors")
					upgraded = true
					require.NoError(t, federation.SyncFederationOnce(t.Context(), sender, binding, peer.credential))
					require.Positive(t, posts)
					current := make(map[int64]db.RelayOutboxExport)
					for _, exported := range collectRelayOutbox(t, sender) {
						current[exported.ID] = exported
					}
					for _, original := range outbox {
						retained, found := current[original.ID]
						require.True(t, found, "retry keeps each source identity")
						require.Equal(t, original.SourceUID, retained.SourceUID)
						require.Equal(t, original.SourceHash, retained.SourceHash)
						require.Equal(t, original.Envelope, retained.Envelope)
					}
				})
			}
		}
	}
}

// Each row is a distinct typed category of reconciliation failure.
func TestReconcileSessionProbeClassificationAndGuidance(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		features string
		want     error
		readOnly bool
	}{
		{"unsupported", http.StatusOK, db.CronEventFeature, federation.ErrHubUnavailable, false},
		{"unavailable", http.StatusServiceUnavailable, "", federation.ErrHubUnavailable, false},
		{"credential rejected", http.StatusUnauthorized, "", federation.ErrConfigurationConflict, false},
		{"read-only adoption", http.StatusOK, "", federation.ErrConfigurationConflict, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openReconcileStore(t)
			project, err := store.CreateProject(t.Context(), "spoke-project")
			require.NoError(t, err)
			if tc.readOnly {
				// Session-free content adopts without a hub probe and reaches the store's refusal.
				_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Example", Author: "worker"})
				require.NoError(t, err)
			} else {
				relaySessionHistory(t, &relayMatrixNode{store: store, project: project, account: "identity-user"}, false, true)
			}
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
			var reconciled db.Storage = store
			if tc.readOnly {
				reconciled = readOnlyAdoptionStore{dbtest.TransactionalStore{Storage: store}}
			}
			err = federation.ReconcileMapping(t.Context(), reconciled, newFakeCredentialStore(), newFakeHub(), catalog, testMapping(), nil)
			require.ErrorIs(t, err, tc.want)
			require.NotContains(t, err.Error(), "private-hub-body")
			if tc.name == "credential rejected" {
				require.NotContains(t, err.Error(), "credential changed during local convergence")
			}
			_, err = store.FederationBindingByProject(t.Context(), project.ID)
			require.ErrorIs(t, err, db.ErrNotFound)
		})
	}
}

// One fresh forwarded case: the relay's forwarded checkpoint reaches the same
// gate through its own entry, and retains the root's exact signed identity.
// Deleting the root events afterwards forces the relay to install that reset
// instead of pulling events, as the existing relay reset runtime test does.
func TestRelaySessionForwardedResetRefusesAndRetries(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root, relay := relaySessionPair(t, backend)
			relaySessionHistory(t, root, true, false)
			syncRelayMatrixNode(t, relay)
			binding, err := relay.store.FederationBindingByProject(t.Context(), relay.project.ID)
			require.NoError(t, err)
			original, err := root.store.(db.RelayResetStore).CreateRelayReset(t.Context(), binding.RelayConfig.BindingUID, root.signer)
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
			raw := relaySessionRefuseThenServe(t, relay, leaf.credential.Token, path, "snapshot_uid")
			var forwarded db.RelayResetCheckpoint
			require.NoError(t, json.Unmarshal(raw, &forwarded))
			require.Equal(t, original.Manifest, forwarded.Manifest)
			require.Equal(t, original.Snapshot, forwarded.Snapshot)
			require.True(t, relaySectionHasSessions(t, forwarded.Snapshot.Events))
			require.False(t, relaySectionHasSessions(t, forwarded.Snapshot.Entities))
			pin, err := root.store.RootAuthority(t.Context(), root.project.UID)
			require.NoError(t, err)
			require.NoError(t, db.VerifyRootResetManifest(pin, forwarded.Manifest, forwarded.Snapshot))
		})
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
			var batch db.RelayBatch
			require.NoError(t, json.Unmarshal(relaySessionRefuseThenServe(t, root, peer.credential.Token, path, "envelopes"), &batch))
			require.NotEmpty(t, batch.Envelopes)
			for _, envelope := range batch.Envelopes {
				require.Equal(t, checkpoint.Translation.Authority.Epoch, envelope.Epoch)
			}
			require.True(t, relayEnvelopesHaveSessions(t, batch.Envelopes))
			relaySessionAcceptAndAck(t, root, peer, batch)
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
