package federation_test

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

func relayResetRecoveryRoot(t *testing.T, backend string) (*relayMatrixNode, *relayMatrixNode) {
	t.Helper()
	root := newRelayMatrixNode(t, backend, "root-member")
	replica := newRelayMatrixNode(t, backend, "replica-member")
	project, err := root.store.CreateProject(t.Context(), "shared-project")
	require.NoError(t, err)
	root.project = project
	_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
	require.NoError(t, root.store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	return root, replica
}

// R4/A6: work arriving after the drain must block checkpoint installation.
// A retry delivers that same source event before replacing the projection.
func TestRelayResetPreservesIntentArrivingAfterDrain(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, boundary := range []string{"before_capture", "after_capture", "lost_reply", "lost_activation"} {
			t.Run(backend+"/"+boundary, func(t *testing.T) {
				root, replica := relayResetRecoveryRoot(t, backend)
				ctx := t.Context()
				_, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Author: "source-agent", Title: "Compacted original"})
				require.NoError(t, err)
				executor := root.store.(interface {
					ExecContext(context.Context, string, ...any) (sql.Result, error)
				})
				query := "DELETE FROM events WHERE project_id=?"
				if backend == "postgres" {
					query = "DELETE FROM events WHERE project_id=$1"
				}
				_, err = executor.ExecContext(ctx, query, root.project.ID)
				require.NoError(t, err)
				upstream := root.http
				var local db.Issue
				var source db.Event
				var lateRoot db.Issue
				var lateRootSource db.Event
				var lateRootErr error
				rootArrived := false
				var creationErr error
				injected := false
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/federation/relay") && r.URL.Query().Get("epoch") == "2" && !rootArrived {
						rootArrived = true
						lateRoot, lateRootSource, lateRootErr = root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Author: "source-agent", Title: "Root write after checkpoint capture"})
						if boundary == "lost_activation" {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
					}
					if !strings.HasSuffix(r.URL.Path, "/federation/relay/reset") || injected {
						upstream.Config.Handler.ServeHTTP(w, r)
						return
					}
					captured := httptest.NewRecorder()
					if boundary != "before_capture" {
						upstream.Config.Handler.ServeHTTP(captured, r)
					}
					injected = true
					local, source, creationErr = replica.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: replica.project.ID, Author: "offline-agent", Title: "Intent arriving during checkpoint fetch"})
					if boundary == "before_capture" {
						upstream.Config.Handler.ServeHTTP(captured, r)
					}
					if boundary == "lost_reply" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					maps.Copy(w.Header(), captured.Header())
					w.WriteHeader(captured.Code)
					_, _ = w.Write(captured.Body.Bytes())
				}))
				t.Cleanup(proxy.Close)
				root.http = proxy
				enrollRelayMatrixReplica(t, root, replica, "replica-project", false)
				binding, err := replica.store.FederationBindingByProject(ctx, replica.project.ID)
				require.NoError(t, err)
				err = federation.SyncFederationOnce(ctx, replica.store, binding, replica.credential)
				if boundary == "lost_reply" {
					require.Error(t, err)
				} else {
					require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
				}
				require.True(t, injected)
				require.NoError(t, creationErr)
				retained, err := replica.store.IssueByUID(ctx, local.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, local.Title, retained.Title)
				unchanged, err := replica.store.FederationBindingByProject(ctx, replica.project.ID)
				require.NoError(t, err)
				require.Equal(t, binding.RelayConfig.ResetEpoch, unchanged.RelayConfig.ResetEpoch)
				if boundary == "lost_activation" {
					current, err := replica.store.FederationBindingByProject(ctx, replica.project.ID)
					require.NoError(t, err)
					require.Error(t, federation.SyncFederationOnce(ctx, replica.store, current, replica.credential))
					installed, err := replica.store.FederationBindingByProject(ctx, replica.project.ID)
					require.NoError(t, err)
					require.Equal(t, int64(2), installed.RelayConfig.ResetEpoch, "projection committed before the activation request failed")
				}
				syncRelayMatrixNode(t, replica)
				require.True(t, rootArrived)
				require.NoError(t, lateRootErr)
				for _, node := range []*relayMatrixNode{root, replica} {
					proof, err := node.store.EntityAttribution(ctx, root.project.UID, "issue", local.UID)
					require.NoError(t, err)
					require.Equal(t, source.UID, proof.EventUID)
					require.Equal(t, source.ContentHash, proof.ContentHash)
					newProof, err := node.store.EntityAttribution(ctx, root.project.UID, "issue", lateRoot.UID)
					require.NoError(t, err)
					require.Equal(t, lateRootSource.UID, newProof.EventUID)
					require.Equal(t, lateRootSource.ContentHash, newProof.ContentHash)

				}
			})
		}
	}
}

// R4/A6: signaled recovery drains existing old-epoch work before switching the
// namespace, including fresh offline intent and deliveries owed by the root.
func TestRelayResetDrainsPending(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root, replica := relayResetRecoveryRoot(t, backend)
			ctx := t.Context()
			original, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Author: "source-agent", Title: "Compacted original"})
			require.NoError(t, err)
			executor := root.store.(interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
			})
			query := "DELETE FROM events WHERE project_id=?"
			if backend == "postgres" {
				query = "DELETE FROM events WHERE project_id=$1"
			}
			_, err = executor.ExecContext(ctx, query, root.project.ID)
			require.NoError(t, err)
			enrollRelayMatrixReplica(t, root, replica, "replica-project", false)
			local, _, err := replica.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: replica.project.ID, Author: "offline-agent", Title: "Offline work before first sync"})
			require.NoError(t, err)
			inbound, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Author: "root-agent", Title: "Delivery owed before reset"})
			require.NoError(t, err)
			syncRelayMatrixNode(t, replica)
			for _, uid := range []string{original.UID, local.UID, inbound.UID} {
				_, err = root.store.IssueByUID(ctx, uid, db.IncludeDeletedYes)
				require.NoError(t, err)
				got, err := replica.store.IssueByUID(ctx, uid, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, "verified", got.Verification)
			}
			binding, err := replica.store.FederationBindingByProject(ctx, replica.project.ID)
			require.NoError(t, err)
			require.Greater(t, binding.RelayConfig.ResetEpoch, int64(1))
			syncRelayMatrixNode(t, replica)
		})
	}
}

// R4: a second purge after an installed checkpoint must advertise and install
// another signed current state rather than silently keeping the removed row.
func TestRelayRepeatedPurgeReset(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root, replica := relayResetRecoveryRoot(t, backend)
			ctx := t.Context()
			var issues []db.Issue
			for _, title := range []string{"First purge", "Second purge", "Retained content"} {
				issue, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Author: "source-agent", Title: title})
				require.NoError(t, err)
				issues = append(issues, issue)
			}
			enrollRelayMatrixReplica(t, root, replica, "replica-project", false)
			syncRelayMatrixNode(t, replica)
			epoch := int64(1)
			for _, issue := range issues[:2] {
				_, err := root.store.PurgeIssue(ctx, issue.ID, "admin", nil)
				require.NoError(t, err)
				syncRelayMatrixNode(t, replica)
				_, err = replica.store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
				require.ErrorIs(t, err, db.ErrNotFound)
				binding, err := replica.store.FederationBindingByProject(ctx, replica.project.ID)
				require.NoError(t, err)
				require.Greater(t, binding.RelayConfig.ResetEpoch, epoch)
				epoch = binding.RelayConfig.ResetEpoch
				retained, err := replica.store.IssueByUID(ctx, issues[2].UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, "verified", retained.Verification)
				syncRelayMatrixNode(t, replica)
			}
		})
	}
}

// A6: restoring an archived replica preserves and drains its retained local
// intent. Archive/restore must not require deleting an outbox or credential.
func TestRelayArchivedPendingRecovery(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root, replica := relayResetRecoveryRoot(t, backend)
			ctx := t.Context()
			enrollRelayMatrixReplica(t, root, replica, "replica-project", false)
			syncRelayMatrixNode(t, replica)
			issue, source, err := replica.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: replica.project.ID, Author: "offline-agent", Title: "Retained before archive"})
			require.NoError(t, err)
			binding, err := replica.store.FederationBindingByProject(ctx, replica.project.ID)
			require.NoError(t, err)
			before, err := replica.store.PendingRelayDeliveries(ctx, binding.RelayConfig.BindingUID, db.RelayStreamEvent, 32)
			require.NoError(t, err)
			require.Len(t, before, 1)
			require.Equal(t, source.UID, before[0].SourceUID)
			_, _, err = replica.store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: replica.project.ID, Actor: replica.account, Force: true})
			require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
			blocked, err := replica.store.PendingRelayDeliveries(ctx, binding.RelayConfig.BindingUID, db.RelayStreamEvent, 32)
			require.NoError(t, err)
			require.Equal(t, before, blocked, "blocked archive preserves the emitted local intent")
			syncRelayMatrixNode(t, replica)
			_, archived, err := replica.store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: replica.project.ID, Actor: replica.account, Force: true})
			require.NoError(t, err)
			require.NotNil(t, archived)
			_, _, changed, err := replica.store.RestoreProject(ctx, replica.project.ID, replica.account)
			require.NoError(t, err)
			require.True(t, changed)
			after, err := replica.store.PendingRelayDeliveries(ctx, binding.RelayConfig.BindingUID, db.RelayStreamEvent, 32)
			require.NoError(t, err)
			require.NotEmpty(t, after)
			require.Equal(t, archived.UID, after[0].SourceUID, "restore retains the archive delivery rather than discarding it")
			require.Equal(t, archived.ContentHash, after[0].SourceHash)
			syncRelayMatrixNode(t, replica)
			for _, node := range []*relayMatrixNode{root, replica} {
				project, err := node.store.ProjectByUID(ctx, issue.ProjectUID)
				require.NoError(t, err)
				require.Nil(t, project.DeletedAt, "another instance's lifecycle audit does not archive this catalog")
				got, err := node.store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, "verified", got.Verification)
				require.Equal(t, root.account, got.AccountableActor)
				require.Equal(t, "offline-agent", got.SourceActor)
			}
			pending, err := replica.store.PendingRelayDeliveries(ctx, binding.RelayConfig.BindingUID, db.RelayStreamEvent, 32)
			require.NoError(t, err)
			require.Empty(t, pending)
			syncRelayMatrixNode(t, replica)
			retained, err := root.store.EventsByUIDs(ctx, root.project.ID, []string{source.UID})
			require.NoError(t, err)
			require.Len(t, retained, 1)
			require.Equal(t, source.ContentHash, retained[0].ContentHash)
		})
	}
}
