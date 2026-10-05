package federation_test

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

func newRecoveryRoot(t *testing.T, backend string) *relayMatrixNode {
	t.Helper()
	root := newRelayMatrixNode(t, backend, "root-member")
	project, err := root.store.CreateProject(t.Context(), "shared-project")
	require.NoError(t, err)
	root.project = project
	_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
	require.NoError(t, root.store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	return root
}

func TestRelayMembershipRegrantCatchup(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRecoveryRoot(t, backend)
			personal := newRelayMatrixNode(t, backend, "personal-member")
			leaf := newRelayMatrixNode(t, backend, "leaf-member")
			team, _, err := root.store.CreateTeam(t.Context(), "shared-team", "admin")
			require.NoError(t, err)
			_, err = root.store.SetTeamMembership(t.Context(), team.UID, root.account, true, "admin")
			require.NoError(t, err)
			_, _, err = root.store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: root.project.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
			require.NoError(t, err)
			enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
			enrollRelayMatrixReplica(t, personal, leaf, "leaf-alias", false)
			syncRelayMatrixNode(t, personal)
			syncRelayMatrixNode(t, leaf)
			_, err = root.store.SetTeamMembership(t.Context(), team.UID, root.account, false, "admin")
			require.NoError(t, err)
			binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
			require.NoError(t, err)
			require.Error(t, federation.SyncFederationOnce(t.Context(), personal.store, binding, personal.credential))
			binding, err = personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
			require.NoError(t, err)
			require.True(t, binding.RelayConfig.UpstreamRevoked)
			leafBinding, err := leaf.store.FederationBindingByProject(t.Context(), leaf.project.ID)
			require.NoError(t, err)
			require.Error(t, federation.SyncFederationOnce(t.Context(), leaf.store, leafBinding, leaf.credential))
			leafBinding, err = leaf.store.FederationBindingByProject(t.Context(), leaf.project.ID)
			require.NoError(t, err)
			require.True(t, leafBinding.RelayConfig.UpstreamRevoked)
			_, err = root.store.SetTeamMembership(t.Context(), team.UID, root.account, true, "admin")
			require.NoError(t, err)
			_, err = root.store.AuthorizeFederationToken(t.Context(), personal.credential.Token, root.project.ID, "pull")
			require.NoError(t, err, "the original credential is live again on the root")
			personalIssue, _, err := personal.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: personal.project.ID, Title: "Offline task", Author: personal.account})
			require.NoError(t, err)
			err = federation.SyncFederationOnce(t.Context(), personal.store, binding, personal.credential)
			require.NoError(t, err, "A4: restored membership must permit ordinary catchup with the current credential")
			leafIssue, _, err := leaf.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: leaf.project.ID, Title: "Queued leaf task", Author: leaf.account})
			require.NoError(t, err)
			syncRelayMatrixNode(t, leaf)
			syncRelayMatrixNode(t, personal)
			syncRelayMatrixNode(t, leaf)
			for _, node := range []*relayMatrixNode{root, personal, leaf} {
				for _, uid := range []string{personalIssue.UID, leafIssue.UID} {
					restored, err := node.store.IssueByUID(t.Context(), uid, db.IncludeDeletedNo)
					require.NoError(t, err)
					require.Equal(t, root.account, restored.AccountableActor)
				}
				current, err := node.store.FederationBindingByProject(t.Context(), node.project.ID)
				require.NoError(t, err)
				if node != root {
					require.False(t, current.RelayConfig.UpstreamRevoked)
				}
			}
		})
	}
}

// A4: a successful handshake cannot overwrite a concurrent operator decision.
type relayRecoveryChangedStore struct {
	db.Storage
	beforeRecovery func()
}

func (s relayRecoveryChangedStore) SetRelayBindingConfig(ctx context.Context, id int64, config db.RelayBindingConfig, expected ...db.RelayBindingConfig) (db.FederationBinding, error) {
	if len(expected) == 1 {
		s.beforeRecovery()
	}
	return s.Storage.SetRelayBindingConfig(ctx, id, config, expected...)
}
func TestRelayRegrantPreservesConcurrentConfiguration(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRecoveryRoot(t, backend)
			personal := newRelayMatrixNode(t, backend, "personal-member")
			enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
			binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
			require.NoError(t, err)
			revoked := *binding.RelayConfig
			revoked.UpstreamRevoked = true
			binding, err = personal.store.SetRelayBindingConfig(t.Context(), personal.project.ID, revoked)
			require.NoError(t, err)
			changed := revoked
			changed.ServeDownstream = false
			wrapped := relayRecoveryChangedStore{Storage: personal.store, beforeRecovery: func() {
				_, err := personal.store.SetRelayBindingConfig(t.Context(), personal.project.ID, changed)
				require.NoError(t, err)
			}}
			err = federation.SyncFederationOnce(t.Context(), wrapped, binding, personal.credential)
			require.ErrorIs(t, err, db.ErrRemoteEventConflict)
			current, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
			require.NoError(t, err)
			require.Equal(t, changed, *current.RelayConfig)
		})
	}
}

// A4/A6: regranted authority must compose with old-epoch drain, lost
// activation and the deliberate no-reset-with-active-descendants boundary.
func TestRelayRegrantResetRecovery(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, boundary := range []string{"drain", "lost_activation", "activation_unreached", "pending_descendant"} {
			t.Run(backend+"/"+boundary, func(t *testing.T) {
				ctx := t.Context()
				root := newRecoveryRoot(t, backend)
				personal := newRelayMatrixNode(t, backend, "personal-member")
				team, _, err := root.store.CreateTeam(ctx, "shared-team", "admin")
				require.NoError(t, err)
				_, err = root.store.SetTeamMembership(ctx, team.UID, root.account, true, "admin")
				require.NoError(t, err)
				_, _, err = root.store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: root.project.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
				require.NoError(t, err)
				original, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Title: "Compacted original", Author: "source-agent"})
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
				activated := false
				if boundary == "lost_activation" || boundary == "activation_unreached" {
					upstream := root.http
					proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if strings.HasSuffix(r.URL.Path, "/federation/relay") && r.URL.Query().Get("epoch") == "2" && !activated {
							activated = true
							if boundary == "lost_activation" {
								upstream.Config.Handler.ServeHTTP(httptest.NewRecorder(), r)
							}
							w.WriteHeader(http.StatusServiceUnavailable) // Activation committed; its response is lost.
							return
						}
						upstream.Config.Handler.ServeHTTP(w, r)
					}))
					t.Cleanup(proxy.Close)
					root.http = proxy
				}
				enrollRelayMatrixReplica(t, root, personal, "personal-alias", boundary == "pending_descendant")
				var leaf *relayMatrixNode
				if boundary == "pending_descendant" {
					leaf = newRelayMatrixNode(t, backend, "leaf-member")
					enrollRelayMatrixReplica(t, personal, leaf, "leaf-alias", false)
				}
				_, err = root.store.SetTeamMembership(ctx, team.UID, root.account, false, "admin")
				require.NoError(t, err)
				binding, err := personal.store.FederationBindingByProject(ctx, personal.project.ID)
				require.NoError(t, err)
				require.Error(t, federation.SyncFederationOnce(ctx, personal.store, binding, personal.credential))
				binding, err = personal.store.FederationBindingByProject(ctx, personal.project.ID)
				require.NoError(t, err)
				require.True(t, binding.RelayConfig.UpstreamRevoked)
				if leaf != nil {
					_, err = personal.store.AuthorizeFederationToken(ctx, leaf.credential.Token, personal.project.ID, "pull")
					require.Error(t, err, "observed upstream revocation denies descendants")
				}
				_, err = root.store.SetTeamMembership(ctx, team.UID, root.account, true, "admin")
				require.NoError(t, err)
				local, source, err := personal.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: personal.project.ID, Title: "Queued while revoked", Author: personal.account})
				require.NoError(t, err)
				err = federation.SyncFederationOnce(ctx, personal.store, binding, personal.credential)
				if boundary == "drain" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				current, readErr := personal.store.FederationBindingByProject(ctx, personal.project.ID)
				require.NoError(t, readErr)
				require.False(t, current.RelayConfig.UpstreamRevoked, "only the validated live regrant clears admission")
				got, readErr := root.store.IssueByUID(ctx, local.UID, db.IncludeDeletedNo)
				require.NoError(t, readErr, "old-epoch intent drains before reset")
				require.Equal(t, root.account, got.AccountableActor)
				proof, readErr := root.store.EntityAttribution(ctx, root.project.UID, "issue", local.UID)
				require.NoError(t, readErr)
				require.Equal(t, source.UID, proof.EventUID)
				require.Equal(t, source.ContentHash, proof.ContentHash)
				if leaf != nil {
					require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush, "delivery owed to the descendant blocks installation before the enrollment guard")
					require.Equal(t, binding.RelayConfig.ResetEpoch, current.RelayConfig.ResetEpoch, "failed installation keeps the consistent old namespace")
					_, readErr = personal.store.AuthorizeFederationToken(ctx, leaf.credential.Token, personal.project.ID, "pull")
					require.NoError(t, readErr, "current authenticated root authority admits existing descendants even when safe reset is blocked")
					retained, readErr := personal.store.IssueByUID(ctx, local.UID, db.IncludeDeletedNo)
					require.NoError(t, readErr)
					require.Equal(t, local.UID, retained.UID)
					return
				}
				if boundary == "lost_activation" || boundary == "activation_unreached" {
					require.True(t, activated)
					require.Equal(t, int64(2), current.RelayConfig.ResetEpoch)
					grant, readErr := root.store.AuthorizeFederationToken(ctx, personal.credential.Token, root.project.ID, "pull")
					require.NoError(t, readErr)
					if boundary == "lost_activation" {
						require.Equal(t, current.RelayConfig.ResetEpoch, grant.RelayResetEpoch)
					} else {
						require.Equal(t, binding.RelayConfig.ResetEpoch, grant.RelayResetEpoch, "upstream remains in the old namespace until activation arrives")
					}
					syncRelayMatrixNode(t, personal)
				}
				_, err = personal.store.IssueByUID(ctx, original.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				retained, err := personal.store.IssueByUID(ctx, local.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, source.UID, proof.EventUID)
				require.Equal(t, root.account, retained.AccountableActor)
			})
		}
	}
}
