package federation_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/uid"
)

type countedRelayQuarantineStore struct {
	db.Storage
	pendingCalls, acceptCalls int
}

func (s *countedRelayQuarantineStore) PendingRelayDeliveries(ctx context.Context, binding, stream string, limit int) ([]db.RelayEnvelope, error) {
	s.pendingCalls++
	return s.Storage.PendingRelayDeliveries(ctx, binding, stream, limit)
}
func (s *countedRelayQuarantineStore) AcceptRelayDeliveries(ctx context.Context, binding string, batch db.RelayBatch) (db.RelayAcceptance, error) {
	s.acceptCalls++
	return s.Storage.AcceptRelayDeliveries(ctx, binding, batch)
}

// R4: rejected canonical unknown data retains its exact batch and stops retries
// until an explicit operator retry, without advancing any emitted-prefix ACK.
func TestRelayUnknownEventQuarantine(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, direction := range []db.FederationQuarantineDirection{db.FederationQuarantineDirectionPush, db.FederationQuarantineDirectionPull} {
			t.Run(backend+"/"+string(direction), func(t *testing.T) {
				root := newRecoveryRoot(t, backend)
				personal := newRelayMatrixNode(t, backend, "personal-member")
				enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
				sourceNode := personal
				if direction == db.FederationQuarantineDirectionPull {
					sourceNode = root
				}
				_, created, err := sourceNode.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sourceNode.project.ID, Title: "Future data", Author: sourceNode.account})
				require.NoError(t, err)
				syncRelayMatrixNode(t, personal)
				source := db.RemoteEventFromStored(created)
				source.EventUID, err = uid.New()
				require.NoError(t, err)
				source.Type = "issue.future_operation"
				source.HLCCounter++
				source.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: source.EventUID, OriginInstanceUID: source.OriginInstanceUID, ProjectUID: source.ProjectUID, ProjectName: source.ProjectName, IssueUID: source.IssueUID, RelatedIssueUID: source.RelatedIssueUID, Type: source.Type, Actor: source.Actor, HLCPhysicalMS: source.HLCPhysicalMS, HLCCounter: source.HLCCounter, CreatedAt: source.CreatedAt.UTC().Format(db.EventTimestampFormat), Payload: source.Payload})
				require.NoError(t, err)
				inserted, err := sourceNode.store.InsertRemoteEvent(t.Context(), sourceNode.project.ID, source)
				require.NoError(t, err)
				require.True(t, inserted)
				binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
				require.NoError(t, err)
				pending, err := sourceNode.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamEvent, 100)
				require.NoError(t, err)
				require.Len(t, pending, 1)
				require.Equal(t, source.EventUID, pending[0].SourceUID)
				counted := &countedRelayQuarantineStore{Storage: personal.store}
				require.Error(t, federation.SyncFederationOnce(t.Context(), counted, binding, personal.credential))
				quarantines, err := personal.store.ActiveFederationQuarantinesByProject(t.Context(), personal.project.ID)
				require.NoError(t, err)
				require.Len(t, quarantines, 1, "unknown data needs durable operator recovery state")
				quarantine := quarantines[0]
				require.Equal(t, direction, quarantine.Direction)
				require.Equal(t, []string{source.EventUID}, quarantine.EventUIDs)
				require.Equal(t, pending[0].Sequence, quarantine.FirstEventID)
				require.Equal(t, pending[0].Sequence, quarantine.LastEventID)
				pendingCalls, acceptCalls := counted.pendingCalls, counted.acceptCalls
				err = federation.SyncFederationOnce(t.Context(), counted, binding, personal.credential)
				require.ErrorContains(t, err, "quarantined")
				require.Equal(t, pendingCalls, counted.pendingCalls, "quarantine prevents another offered push")
				require.Equal(t, acceptCalls, counted.acceptCalls, "quarantine prevents another ingest attempt")
				_, err = personal.store.SkipFederationQuarantine(t.Context(), db.SkipFederationQuarantineParams{ID: quarantine.ID, ProjectID: personal.project.ID, Actor: "local-owner", Reason: "explicit disposition"})
				require.ErrorContains(t, err, "relay quarantine", "legacy skip cannot falsely advance a relay ACK")
				retained, err := sourceNode.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamEvent, 100)
				require.NoError(t, err)
				require.Equal(t, pending, retained)
				resolved, err := personal.store.RetryFederationQuarantine(t.Context(), db.RetryFederationQuarantineParams{ID: quarantine.ID, ProjectID: personal.project.ID, Actor: "local-owner", Reason: "compatible peer deployed"})
				require.NoError(t, err)
				require.NotNil(t, resolved.SkippedAt)
				require.Error(t, federation.SyncFederationOnce(t.Context(), counted, binding, personal.credential), "unchanged incompatible recipient still rejects")
				again, err := personal.store.ActiveFederationQuarantinesByProject(t.Context(), personal.project.ID)
				require.NoError(t, err)
				require.Len(t, again, 1)
				require.NotEqual(t, quarantine.ID, again[0].ID)
				require.Equal(t, quarantine.EventUIDs, again[0].EventUIDs)
				retained, err = sourceNode.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamEvent, 100)
				require.NoError(t, err)
				require.Equal(t, pending, retained, "operator retry preserves immutable source/hop identities and epoch")
			})
		}
	}
}

// A delayed rejection cannot create current quarantine under a stale epoch or
// revoked local project grant; existing native admission fences own this check.
func TestRelayQuarantineAdmission(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, scenario := range []string{"wrong-epoch", "revoked"} {
			t.Run(backend+"/"+scenario, func(t *testing.T) {
				root := newRecoveryRoot(t, backend)
				personal := newRelayMatrixNode(t, backend, "personal-member")
				enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
				binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
				require.NoError(t, err)
				config := *binding.RelayConfig
				epoch := config.ResetEpoch
				if scenario == "wrong-epoch" {
					epoch++
				} else {
					revoked := config
					revoked.UpstreamRevoked = true
					_, err = personal.store.SetRelayBindingConfig(t.Context(), personal.project.ID, revoked, config)
					require.NoError(t, err)
				}
				_, err = personal.store.RecordFederationQuarantine(t.Context(), db.RecordFederationQuarantineParams{ProjectID: personal.project.ID, Direction: db.FederationQuarantineDirectionPush, FirstEventID: 1, LastEventID: 1, EventUIDs: []string{personal.project.UID}, Error: "delayed relay rejection", RelayBindingUID: config.BindingUID, RelayResetEpoch: epoch})
				require.Error(t, err)
				active, err := personal.store.ActiveFederationQuarantinesByProject(t.Context(), personal.project.ID)
				require.NoError(t, err)
				require.Empty(t, active)
			})
		}
	}
}

func TestRelayQuarantineOperatorHTTP(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, direction := range []db.FederationQuarantineDirection{db.FederationQuarantineDirectionPush, db.FederationQuarantineDirectionPull} {
			t.Run(backend+"/"+string(direction), func(t *testing.T) {
				root := newRecoveryRoot(t, backend)
				personal := newRelayMatrixNode(t, backend, "personal-member")
				enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
				binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
				require.NoError(t, err)
				q, err := personal.store.RecordFederationQuarantine(t.Context(), db.RecordFederationQuarantineParams{ProjectID: personal.project.ID, Direction: direction, FirstEventID: 1, LastEventID: 1, EventUIDs: []string{personal.project.UID}, Error: "incompatible peer", RelayBindingUID: binding.RelayConfig.BindingUID, RelayResetEpoch: binding.RelayConfig.ResetEpoch})
				require.NoError(t, err)
				server := daemon.NewServer(daemon.ServerConfig{DB: personal.store, Auth: config.AuthConfig{Token: "owner-quarantine-test-token"}})
				t.Cleanup(func() { require.NoError(t, server.Close()) })
				endpoint := httptest.NewServer(server.Handler())
				t.Cleanup(endpoint.Close)
				for _, action := range []string{"skip", "retry"} {
					request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/federation/quarantine/%d/%s", endpoint.URL, personal.project.ID, q.ID, action), bytes.NewBufferString(`{"actor":"local-owner","reason":"compatible peer deployed"}`))
					require.NoError(t, err)
					request.Header.Set("Authorization", "Bearer owner-quarantine-test-token")
					request.Header.Set("Content-Type", "application/json")
					verb := "SKIP"
					if action == "retry" {
						verb = "RETRY"
					}
					request.Header.Set("X-Kata-Confirm", fmt.Sprintf("%s FEDERATION BATCH %d", verb, q.ID))
					response, err := http.DefaultClient.Do(request)
					require.NoError(t, err)
					var body map[string]any
					require.NoError(t, json.UnmarshalRead(response.Body, &body))
					require.NoError(t, response.Body.Close())
					if action == "skip" {
						require.Equal(t, http.StatusConflict, response.StatusCode, body)
						require.Equal(t, "federation_quarantine_skip_unsupported", body["error"].(map[string]any)["code"])
						active, err := personal.store.ActiveFederationQuarantinesByProject(t.Context(), personal.project.ID)
						require.NoError(t, err)
						require.Len(t, active, 1)
					} else {
						require.Equal(t, http.StatusOK, response.StatusCode, body)
					}
				}
			})
		}
	}
}

type rejectFirstRelayEventStore struct {
	db.Storage
	rejected bool
}

func (s *rejectFirstRelayEventStore) AcceptRelayDeliveries(ctx context.Context, binding string, batch db.RelayBatch) (db.RelayAcceptance, error) {
	if batch.Stream == db.RelayStreamEvent && !s.rejected {
		s.rejected = true
		return db.RelayAcceptance{}, fmt.Errorf("%w: recipient upgrade pending", db.ErrFederationIngestValidation)
	}
	return s.Storage.AcceptRelayDeliveries(ctx, binding, batch)
}

// After compatible recovery, explicit retry commits the retained original
// event, then resumes normal content delivery in either direction.
func TestRelayQuarantineRetryConverges(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, direction := range []db.FederationQuarantineDirection{db.FederationQuarantineDirectionPush, db.FederationQuarantineDirectionPull} {
			t.Run(backend+"/"+string(direction), func(t *testing.T) {
				root := newRecoveryRoot(t, backend)
				personal := newRelayMatrixNode(t, backend, "personal-member")
				enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
				source, recipient := personal, root
				credential := personal.credential
				local := personal.store
				if direction == db.FederationQuarantineDirectionPull {
					source, recipient = root, personal
					local = &rejectFirstRelayEventStore{Storage: personal.store}
				} else {
					var rejected atomic.Bool
					proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if strings.HasSuffix(r.URL.Path, "/relay:accept") && rejected.CompareAndSwap(false, true) {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							_, _ = w.Write([]byte(`{"status":400,"error":{"code":"relay_invalid","message":"recipient upgrade pending"}}`))
							return
						}
						root.http.Config.Handler.ServeHTTP(w, r)
					}))
					t.Cleanup(proxy.Close)
					credential.HubURL = proxy.URL
				}
				issue, _, err := source.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.project.ID, Title: "Retained recovery work", Author: source.account})
				require.NoError(t, err)
				binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
				require.NoError(t, err)
				original, err := source.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamEvent, 100)
				require.NoError(t, err)
				require.Error(t, federation.SyncFederationOnce(t.Context(), local, binding, credential))
				active, err := personal.store.ActiveFederationQuarantinesByProject(t.Context(), personal.project.ID)
				require.NoError(t, err)
				require.Len(t, active, 1)
				retained, err := source.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamEvent, 100)
				require.NoError(t, err)
				require.Equal(t, original, retained)
				_, err = personal.store.RetryFederationQuarantine(t.Context(), db.RetryFederationQuarantineParams{ID: active[0].ID, ProjectID: personal.project.ID, Actor: "local-owner", Reason: "recipient upgraded"})
				require.NoError(t, err)
				// Replace the incompatible recipient fixture with the complete native
				// store, including its existing optional artifact-store capability.
				local = personal.store
				require.NoError(t, federation.SyncFederationOnce(t.Context(), local, binding, credential))
				mirrored, err := recipient.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, issue.Title, mirrored.Title)
				remaining, err := source.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamEvent, 100)
				require.NoError(t, err)
				require.Empty(t, remaining)
				active, err = personal.store.ActiveFederationQuarantinesByProject(t.Context(), personal.project.ID)
				require.NoError(t, err)
				require.Empty(t, active)
				followup, _, err := source.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.project.ID, Title: "Content after recovery", Author: source.account})
				require.NoError(t, err)
				require.NoError(t, federation.SyncFederationOnce(t.Context(), local, binding, credential))
				mirrored, err = recipient.store.IssueByUID(t.Context(), followup.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, followup.Title, mirrored.Title)
			})
		}
	}
}
