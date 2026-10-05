package federation_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

// R4/A7: local audit records cannot block shared content or replace root claims.
func TestRelaySyncAfterOrdinaryOperation(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, direction := range []string{"root", "relay"} {
			for _, action := range []string{"claim", "close-throttle", "rename", "detach-alias"} {
				t.Run(backend+"/"+direction+"/"+action, func(t *testing.T) {
					root := newRecoveryRoot(t, backend)
					personal := newRelayMatrixNode(t, backend, "personal-member")
					enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
					issue, _, err := root.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: root.project.ID, Title: "Ordinary work", Author: root.account})
					require.NoError(t, err)
					syncRelayMatrixNode(t, personal)
					source, recipient := root, personal
					if direction == "relay" {
						source, recipient = personal, root
					}
					var claimUID string
					switch action {
					case "claim":
						// Claim requests always arbitrate at the root, including from a relay.
						status, claim := relayClaimAction(t, personal, issue.UID, "acquire", "cli")
						require.Equal(t, 200, status)
						require.True(t, claim.Granted)
						require.NotNil(t, claim.Lease)
						claimUID = claim.Lease.ClaimUID
					case "close-throttle":
						_, err = source.store.InsertCloseThrottledEvent(t.Context(), issueIDForRelayMatrix(t, source, issue.UID), source.account, db.CloseThrottledPayload{})
						require.NoError(t, err)
					case "detach-alias":
						const localPath = "local:///example-workspace/private-alias-canary"
						alias, err := source.store.AttachAlias(t.Context(), source.project.ID, localPath, "local")
						require.NoError(t, err)
						_, audit, err := source.store.DetachProjectAlias(t.Context(), db.DetachAliasParams{ProjectID: source.project.ID, AliasID: alias.ID, Actor: source.account, Force: true})
						require.NoError(t, err)
						require.NotNil(t, audit)
						binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
						require.NoError(t, err)
						offered, err := source.store.PendingRelayDeliveries(t.Context(), binding.RelayConfig.BindingUID, db.RelayStreamEvent, 32)
						require.NoError(t, err)
						for _, envelope := range offered {
							require.NotEqual(t, audit.UID, envelope.SourceUID, "local workspace alias must not be offered")
							require.NotContains(t, string(envelope.Body), localPath)
						}
					case "rename":
						_, _, _, err = source.store.RenameProjectAndEvent(t.Context(), source.project.ID, "renamed-shared-project", source.account)
						require.NoError(t, err)
					}
					followup, _, err := source.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.project.ID, Title: "Content after local audit", Author: source.account})
					require.NoError(t, err)
					binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
					require.NoError(t, err)
					require.NoError(t, federation.SyncFederationOnce(t.Context(), personal.store, binding, personal.credential), "ordinary supported operation must not poison the event stream")
					mirrored, err := recipient.store.IssueByUID(t.Context(), followup.UID, db.IncludeDeletedYes)
					require.NoError(t, err)
					require.Equal(t, followup.Title, mirrored.Title)
					unchanged, err := recipient.store.ProjectByID(t.Context(), recipient.project.ID)
					require.NoError(t, err)
					require.Equal(t, recipient.project.Name, unchanged.Name, "local aliases remain local")
					if claimUID != "" {
						state, err := root.store.ClaimStatusReadOnly(t.Context(), root.project.ID, issue.UID, time.Now().UTC())
						require.NoError(t, err)
						require.True(t, state.Held)
						require.Equal(t, claimUID, state.Claim.ClaimUID)
					}
				})
			}
		}
	}
}
