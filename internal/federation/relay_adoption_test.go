package federation_test

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/uid"
)

// Successful timestamps at the batch boundary do not prove the baseline has
// drained. Only the later turn that observes all streams empty may finalize.
func TestAdoptionFinalizerRequiresDrainedTurn(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			root := newRecoveryRoot(t, backend)
			replica := newRelayMatrixNode(t, backend, "replica-member")
			enrollRelayMatrixReplica(t, root, replica, "replica-alias", false)
			syncRelayMatrixNode(t, replica)
			t.Cleanup(federation.SetRelaySyncTurnBudgetForTest(8, 1))
			for i := range 8 {
				_, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Author: "source-agent", Title: fmt.Sprintf("Baseline item %d", i)})
				require.NoError(t, err)
			}
			calls := 0
			runner := &federation.Runner{DB: replica.store, Credentials: replica.credentials, OnRelayDrained: func(_ context.Context, current db.FederationBinding) error {
				calls++
				require.Equal(t, replica.project.ID, current.ProjectID)
				return nil
			}}
			require.NoError(t, runner.RunOnce(ctx))
			status, err := replica.store.FederationSyncStatusByProject(ctx, replica.project.ID)
			require.NoError(t, err)
			require.NotNil(t, status.LastPullSuccessAt)
			require.NotNil(t, status.LastPushSuccessAt)
			require.Zero(t, calls, "eight nonempty batches exhaust the turn even with successful timestamps")
			require.NoError(t, runner.RunOnce(ctx))
			require.Equal(t, 1, calls, "a following turn observes every stream empty")
		})
	}
}

func TestAdoptionFinalizerCallbackRetry(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			root := newRecoveryRoot(t, backend)
			replica := newRelayMatrixNode(t, backend, "replica-member")
			enrollRelayMatrixReplica(t, root, replica, "replica-alias", false)
			binding, err := replica.store.FederationBindingByProject(ctx, replica.project.ID)
			require.NoError(t, err)
			failure := fmt.Errorf("durable credential unavailable")
			calls := 0
			finalize := func(context.Context, db.FederationBinding) error {
				calls++
				if calls == 1 {
					return failure
				}
				return nil
			}
			err = federation.SyncFederationOnceWithCallbacks(ctx, replica.store, binding, replica.credential, clientpkg.Opts{}, nil, finalize)
			require.ErrorIs(t, err, failure)
			require.NoError(t, federation.SyncFederationOnceWithCallbacks(ctx, replica.store, binding, replica.credential, clientpkg.Opts{}, nil, finalize))
			require.Equal(t, 2, calls)
		})
	}
}

// Admission success cannot override an error, a poisoned batch, a replaced
// binding, or leadership lost immediately after stream completion.
func TestAdoptionFinalizerRejectsUnsafeTurn(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, mode := range []string{"http_error", "quarantine", "binding_replaced", "lease_lost", "claim_error"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				ctx := t.Context()
				root := newRecoveryRoot(t, backend)
				replica := newRelayMatrixNode(t, backend, "replica-member")
				enrollRelayMatrixReplica(t, root, replica, "replica-alias", false)
				binding, err := replica.store.FederationBindingByProject(ctx, replica.project.ID)
				require.NoError(t, err)
				failure := errors.New("runner lease lost")
				var late atomic.Bool
				store := &adoptionCompletionStore{Storage: replica.store, RelayArtifactStorage: replica.store.(db.RelayArtifactStorage), RelayResetStore: replica.store.(db.RelayResetStore)}
				store.afterSuccess = func() error {
					late.Store(true)
					if mode == "binding_replaced" {
						changed := *binding.RelayConfig
						changed.LocalActor = "replacement-member"
						_, err := replica.store.SetRelayBindingConfig(ctx, binding.ProjectID, changed, *binding.RelayConfig)
						return err
					}
					return nil
				}
				store.validateLease = func() error {
					if mode == "lease_lost" && late.Load() {
						return failure
					}
					return nil
				}
				if mode == "quarantine" {
					_, err := replica.store.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{ProjectID: binding.ProjectID, Direction: db.FederationQuarantineDirectionPull, FirstEventID: 1, LastEventID: 1, EventUIDs: []string{replica.project.UID}, Error: "poisoned batch", RelayBindingUID: binding.RelayConfig.BindingUID, RelayResetEpoch: binding.RelayConfig.ResetEpoch})
					require.NoError(t, err)
				}
				if mode == "claim_error" {
					issue, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Author: "source-agent", Title: "Pending claim"})
					require.NoError(t, err)
					syncRelayMatrixNode(t, replica)
					_, err = replica.store.EnqueuePendingClaim(ctx, db.PendingClaimParams{ProjectID: replica.project.ID, IssueRef: issue.UID, Principal: db.ClaimPrincipal{HolderInstanceUID: replica.store.InstanceUID(), Holder: replica.account, ClientKind: "cli"}, ClaimKind: "hard", Now: time.Now().UTC()})
					require.NoError(t, err)
					original := *root.handler.Load()
					var rejectClaims http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if strings.Contains(r.URL.Path, "acquire") {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						original.ServeHTTP(w, r)
					})
					root.handler.Store(&rejectClaims)
				}
				if mode == "http_error" {
					replica.credential.Token = "refused-test-token"
					require.NoError(t, replica.credentials.StoreFederationCredential(ctx, replica.project.UID, replica.credential))
				}
				calls := 0
				runner := &federation.Runner{DB: store, Credentials: replica.credentials, Interval: time.Hour, OnRelayDrained: func(context.Context, db.FederationBinding) error { calls++; return nil }}
				if mode == "lease_lost" {
					runCtx, cancel := context.WithCancel(ctx)
					defer cancel()
					failures := make(chan error, 1)
					runner.OnError = func(err error) {
						select {
						case failures <- err:
						default:
						}
					}
					done := make(chan error, 1)
					go func() { done <- runner.Run(runCtx) }()
					select {
					case err := <-failures:
						require.ErrorIs(t, err, failure)
					case <-time.After(10 * time.Second):
						t.Fatal("lease loss was not reported")
					}
					cancel()
					require.ErrorIs(t, <-done, context.Canceled)
				} else {
					require.Error(t, runner.RunOnce(ctx))
				}
				require.Zero(t, calls)
			})
		}
	}
}

type adoptionCompletionStore struct {
	db.Storage
	db.RelayArtifactStorage
	db.RelayResetStore
	afterSuccess  func() error
	validateLease func() error
}

func (s *adoptionCompletionStore) RecordFederationSyncPullSuccess(ctx context.Context, id int64, at time.Time) error {
	if err := s.Storage.RecordFederationSyncPullSuccess(ctx, id, at); err != nil {
		return err
	}
	return s.afterSuccess()
}
func (s *adoptionCompletionStore) AcquireFederationRunnerLease(context.Context) (func() error, error) {
	return func() error { return nil }, nil
}
func (s *adoptionCompletionStore) ValidateFederationRunnerLease(context.Context) error {
	return s.validateLease()
}

// A checkpoint installed locally is not complete until the upstream accepts the
// new epoch. On retry the callback must receive that new epoch, with its HLC
// already observed by the native writer.
func TestAdoptionFinalizerResetActivation(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			root := newRecoveryRoot(t, backend)
			issue, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Author: "source-agent", Title: "Checkpoint issue"})
			require.NoError(t, err)
			// A native future clock makes the post-baseline ordering assertion stronger
			// than wall-clock passage during the test.
			clockProject, err := root.store.CreateProject(ctx, "clock-project")
			require.NoError(t, err)
			eventUID, err := uid.New()
			require.NoError(t, err)
			future := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: root.store.InstanceUID(), ProjectUID: clockProject.UID, ProjectName: clockProject.Name, Type: "project.metadata_updated", Actor: root.account, HLCPhysicalMS: time.Now().Add(time.Hour).UnixMilli(), Payload: jsontext.Value(`{"diff":{"example":{"from":null,"to":"baseline"}}}`), CreatedAt: time.Now().UTC()}
			future.ContentHash, err = db.RemoteEventContentHash(future)
			require.NoError(t, err)
			_, err = root.store.InsertRemoteEvent(ctx, clockProject.ID, future)
			require.NoError(t, err)
			query := "DELETE FROM events WHERE project_id=?"
			if backend == "postgres" {
				query = "DELETE FROM events WHERE project_id=$1"
			}
			_, err = root.store.(interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
			}).ExecContext(ctx, query, root.project.ID)
			require.NoError(t, err)
			originalHandler := root.http.Config.Handler
			var blocked atomic.Bool
			blocked.Store(true)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if blocked.Load() && strings.HasSuffix(r.URL.Path, "/federation/relay") && r.URL.Query().Get("epoch") == "2" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				originalHandler.ServeHTTP(w, r)
			}))
			t.Cleanup(proxy.Close)
			root.http = proxy
			replica := newRelayMatrixNode(t, backend, "replica-member")
			enrollRelayMatrixReplica(t, root, replica, "replica-alias", false)
			binding, err := replica.store.FederationBindingByProject(ctx, replica.project.ID)
			require.NoError(t, err)
			calls := 0
			finalize := func(callbackCtx context.Context, current db.FederationBinding) error {
				calls++
				require.Equal(t, int64(2), current.RelayConfig.ResetEpoch)
				_, err := replica.store.IssueByUID(callbackCtx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				_, event, err := replica.store.CreateIssue(callbackCtx, db.CreateIssueParams{ProjectID: current.ProjectID, Author: "replica-member", Title: "After baseline"})
				require.NoError(t, err)
				require.GreaterOrEqual(t, event.HLCPhysicalMS, future.HLCPhysicalMS)
				if event.HLCPhysicalMS == future.HLCPhysicalMS {
					require.Greater(t, event.HLCCounter, future.HLCCounter)
				}
				return nil
			}
			require.Error(t, federation.SyncFederationOnceWithCallbacks(ctx, replica.store, binding, replica.credential, clientpkg.Opts{}, nil, finalize))
			require.Zero(t, calls)
			current, err := replica.store.FederationBindingByProject(ctx, replica.project.ID)
			require.NoError(t, err)
			require.Equal(t, int64(2), current.RelayConfig.ResetEpoch)
			blocked.Store(false)
			require.NoError(t, federation.SyncFederationOnceWithCallbacks(ctx, replica.store, current, replica.credential, clientpkg.Opts{}, nil, finalize))
			require.Equal(t, 1, calls)
		})
	}
}

// Independent artifact delivery can reach the receiver before its issue
// creation event. A staged manifest is a successful exchange but is still pending.
func TestAdoptionFinalizerWaitsForStagedArtifacts(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, direction := range []string{"push", "pull"} {
			t.Run(backend+"/"+direction, func(t *testing.T) {
				ctx := t.Context()
				root := newRecoveryRoot(t, backend)
				replica := newRelayMatrixNode(t, backend, "replica-member")
				var hidden atomic.Bool
				handler := root.http.Config.Handler
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if hidden.Load() && direction == "pull" && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/federation/relay") && r.URL.Query().Get("stream") == db.RelayStreamEvent {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"stream":"events","envelopes":[]}`))
						return
					}
					handler.ServeHTTP(w, r)
				}))
				t.Cleanup(proxy.Close)
				root.http = proxy
				enrollRelayMatrixReplica(t, root, replica, "replica-alias", false)
				syncRelayMatrixNode(t, replica)
				producer := root
				createCtx := db.WithRootAttribution(ctx, root.signer, root.account)
				if direction == "push" {
					producer = replica
					createCtx = ctx
				}
				title := "Issue not delivered yet"
				issue, _, err := producer.store.CreateIssue(createCtx, db.CreateIssueParams{ProjectID: producer.project.ID, Author: producer.account, Title: title})
				require.NoError(t, err)
				identity := embedding.ArtifactIdentity{ProjectUID: root.project.UID, IssueUID: issue.UID, ProducerInstanceUID: producer.store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
				artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(title, ""), [][]float32{{1, 0}})
				require.NoError(t, err)
				_, err = producer.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
				require.NoError(t, err)
				binding, err := replica.store.FederationBindingByProject(ctx, replica.project.ID)
				require.NoError(t, err)
				hidden.Store(true)
				store := &adoptionArtifactStore{Storage: replica.store, RelayArtifactStorage: replica.store.(db.RelayArtifactStorage), RelayResetStore: replica.store.(db.RelayResetStore), hideEvents: func() bool { return hidden.Load() && direction == "push" }}
				calls := 0
				finalize := func(context.Context, db.FederationBinding) error { calls++; return nil }
				require.NoError(t, federation.SyncFederationOnceWithCallbacks(ctx, store, binding, replica.credential, clientpkg.Opts{}, nil, nil), "ordinary nil-callback sync retains staging behavior")
				pending, err := producer.store.PendingRelayDeliveries(ctx, binding.RelayConfig.BindingUID, db.RelayStreamArtifact, 32)
				require.NoError(t, err)
				require.Len(t, pending, 1, "the native receiver staged bytes without acknowledging the offer")
				require.NoError(t, federation.SyncFederationOnceWithCallbacks(ctx, store, binding, replica.credential, clientpkg.Opts{}, nil, finalize))
				require.Zero(t, calls, "staged artifacts do not prove empty")
				hidden.Store(false)
				require.NoError(t, federation.SyncFederationOnceWithCallbacks(ctx, store, binding, replica.credential, clientpkg.Opts{}, nil, finalize))
				require.Equal(t, 1, calls)
			})
		}
	}
}

type adoptionArtifactStore struct {
	db.Storage
	db.RelayArtifactStorage
	db.RelayResetStore
	hideEvents func() bool
}

func (s *adoptionArtifactStore) PendingRelayDeliveries(ctx context.Context, bindingUID, stream string, limit int) ([]db.RelayEnvelope, error) {
	if stream == db.RelayStreamEvent && s.hideEvents() {
		return nil, nil
	}
	return s.Storage.PendingRelayDeliveries(ctx, bindingUID, stream, limit)
}

// Native fuzzing varies the batch budget and backlog together. Materialization
// is bounded because every case exercises real stores and authenticated HTTP.
func FuzzAdoptionFinalizerTurnBudget(f *testing.F) {
	for _, seed := range []struct{ budget, count uint8 }{{7, 0}, {7, 7}, {7, 8}, {7, 9}, {0, 2}} {
		f.Add(seed.budget, seed.count)
	}
	f.Fuzz(func(t *testing.T, rawBudget, rawCount uint8) {
		budget, count := 1+int(rawBudget%8), int(rawCount%18)
		ctx := t.Context()
		root := newRecoveryRoot(t, "sqlite")
		replica := newRelayMatrixNode(t, "sqlite", "replica-member")
		enrollRelayMatrixReplica(t, root, replica, "replica-alias", false)
		syncRelayMatrixNode(t, replica)
		t.Cleanup(federation.SetRelaySyncTurnBudgetForTest(budget, 1))
		for i := range count {
			_, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Author: "source-agent", Title: fmt.Sprintf("Budget item %d", i)})
			require.NoError(t, err)
		}
		binding, err := replica.store.FederationBindingByProject(ctx, replica.project.ID)
		require.NoError(t, err)
		calls := 0
		finalize := func(context.Context, db.FederationBinding) error { calls++; return nil }
		require.NoError(t, federation.SyncFederationOnceWithCallbacks(ctx, replica.store, binding, replica.credential, clientpkg.Opts{}, nil, finalize))
		want := 0
		if count < budget {
			want = 1
		}
		require.Equal(t, want, calls, "only an empty observation, including at exact budget boundaries, permits finalization")
		issues, err := replica.store.ListIssues(ctx, db.ListIssuesParams{ProjectID: replica.project.ID})
		require.NoError(t, err)
		require.Len(t, issues, min(count, budget), "callback API returns after one bounded turn")
	})
}
