package federation

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// The sender can still see the old owner when another joined spoke has already
// moved the issue on the hub. Supplemental proofs cannot rely on local ownership.
func TestFederationOrdinarySourceProofIncludesJoinedCurrentProject(t *testing.T) {
	for _, kind := range []string{"same-origin", "other-binding-origin", "other-credential-origin", "other-actor", "pull-only", "relay"} {
		t.Run(kind, func(t *testing.T) {
			hub, projects := newMoveDeliveryHub(t, "proof-source", "proof-target")
			spoke := newMoveDeliverySpoke(t, hub, projects)
			issue, _, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Stale local owner", Author: "worker"})
			require.NoError(t, err)
			_, event, err := spoke.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Delayed source edit"})
			require.NoError(t, err)
			other := testenv.New(t)
			target, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[1].ID)
			require.NoError(t, err)
			switch kind {
			case "other-binding-origin":
				target.HubURL = other.URL
			case "other-credential-origin":
				credential := spoke.credentials[projects[1].UID]
				credential.HubURL = other.URL
				spoke.credentials[projects[1].UID] = credential
			case "other-actor":
				target.Actor = "other-worker"
			case "pull-only":
				target.PushEnabled = false
			}
			_, err = spoke.env.DB.UpsertFederationBinding(t.Context(), target)
			require.NoError(t, err)
			if kind == "relay" {
				bindingUID, err := uid.New()
				require.NoError(t, err)
				public, _, err := ed25519.GenerateKey(rand.Reader)
				require.NoError(t, err)
				require.NoError(t, spoke.env.DB.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: projects[1].UID, AuthorityUID: hub.DB.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				_, err = spoke.env.DB.SetRelayBindingConfig(t.Context(), target.ProjectID, db.RelayBindingConfig{
					ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID,
					UpstreamInstanceUID: hub.DB.InstanceUID(), AuthorityUID: hub.DB.InstanceUID(),
					HubPath:    []string{hub.DB.InstanceUID(), spoke.env.DB.InstanceUID()},
					LocalActor: "worker", ResetEpoch: 1,
				})
				require.NoError(t, err)
			}
			binding, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[0].ID)
			require.NoError(t, err)
			tokens, err := federationMoveSourceTokens(t.Context(), spoke.env.DB, spoke.credentials, binding, hub.URL, []db.Event{event}, clientpkg.Opts{})
			if kind == "other-credential-origin" {
				require.Error(t, err, "credentials are never sent to a different configured origin")
				require.Empty(t, tokens)
				return
			}
			require.NoError(t, err)
			if kind == "same-origin" {
				require.Equal(t, spoke.credentials[projects[1].UID].Token, tokens[projects[1].UID])
			} else {
				require.NotContains(t, tokens, projects[1].UID)
			}
		})
	}
}
