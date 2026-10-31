package federation_test

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

// A direct spoke's creation must traverse a root and a serving relay before a
// later root notification, without turning legacy source labels into proof.
func TestRelayLegacySpokeDelivery(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			root := newRelayMatrixNode(t, backend, "root-member")
			project, err := root.store.CreateProject(ctx, "shared-project")
			require.NoError(t, err)
			root.project = project
			_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{
				ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID,
				HubProjectUID: project.UID, Enabled: true,
			})
			require.NoError(t, err)
			public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
			require.NoError(t, root.store.PinRootAuthority(ctx, db.RootKeyPin{
				ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(),
				KeyID: db.RootPublicKeyID(public), PublicKey: public,
			}))
			direct := newRelayMatrixNode(t, backend, "direct-member")
			enrollLegacyRelaySpoke(t, root, direct)
			syncRelayMatrixNode(t, direct)
			relay := newRelayMatrixNode(t, backend, "relay-member")
			leaf := newRelayMatrixNode(t, backend, "leaf-member")
			enrollRelayMatrixReplica(t, root, relay, "relay-project", true)
			syncRelayMatrixNode(t, relay)
			enrollRelayMatrixReplica(t, relay, leaf, "leaf-project", false)
			syncRelayMatrixNode(t, leaf)

			issue, source, err := direct.store.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: direct.project.ID, Title: "Direct spoke task", Author: "source-member",
			})
			require.NoError(t, err)
			syncRelayMatrixNode(t, direct)
			rootIssue, err := root.store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			_, err = root.store.PatchIssueMetadata(db.WithRootAttribution(ctx, root.signer, root.account), db.PatchIssueMetadataIn{
				IssueID: rootIssue.ID, Actor: "system", Patch: map[string]jsontext.Value{"notify.member": jsontext.Value(`"Scheduled date reached"`)},
			})
			require.NoError(t, err)
			syncRelayMatrixNode(t, relay)
			syncRelayMatrixNode(t, leaf)
			for _, node := range []*relayMatrixNode{root, relay, leaf} {
				mirrored, err := node.store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, issue.Title, mirrored.Title)
				require.Equal(t, issue.Author, mirrored.Author)
				require.JSONEq(t, `{"notify.member":"Scheduled date reached"}`, string(mirrored.Metadata))
				require.Equal(t, "legacy", mirrored.Verification)
				require.Empty(t, mirrored.AccountableActor)
				_, err = node.store.EntityAttribution(ctx, project.UID, "issue", issue.UID)
				require.ErrorIs(t, err, db.ErrNotFound)
				events, err := node.store.EventsByUIDs(ctx, node.project.ID, []string{source.UID})
				require.NoError(t, err)
				require.Len(t, events, 1)
				for _, event := range events {
					require.Equal(t, source.ContentHash, event.ContentHash)
					require.Equal(t, direct.store.InstanceUID(), event.OriginInstanceUID)
				}
			}
			later, _, err := direct.store.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: direct.project.ID, Title: "Later direct spoke task", Author: "source-member",
			})
			require.NoError(t, err)
			syncRelayMatrixNode(t, direct)
			syncRelayMatrixNode(t, relay)
			syncRelayMatrixNode(t, leaf)
			_, err = leaf.store.IssueByUID(ctx, later.UID, db.IncludeDeletedYes)
			require.NoError(t, err, "subsequent direct-spoke creations must continue through both hops")
		})
	}
}

func enrollLegacyRelaySpoke(t *testing.T, root, direct *relayMatrixNode) {
	t.Helper()
	status, raw := relayMatrixRequest(t, root, http.MethodPost, "/api/v1/federation/enrollments", "bootstrap-test-token", map[string]any{
		"project_id": root.project.ID, "spoke_instance_uid": direct.store.InstanceUID(), "capabilities": "claim,pull,push", "actor": root.account,
	})
	require.Equal(t, http.StatusOK, status, string(raw))
	var grant api.FederationEnrollmentOut
	require.NoError(t, json.Unmarshal(raw, &grant))
	require.Nil(t, grant.Relay)
	project, err := direct.store.CreateProject(t.Context(), "direct-project")
	require.NoError(t, err)
	adopted, err := direct.store.AdoptProjectIntoFederation(t.Context(), db.AdoptProjectIntoFederationParams{
		ProjectID: project.ID, HubURL: root.http.URL, HubProjectID: root.project.ID,
		HubProjectUID: root.project.UID, Actor: grant.Actor, EmptyOnly: true,
	})
	require.NoError(t, err)
	direct.project = adopted.Project
	binding := adopted.Binding
	binding.Enabled, binding.PushEnabled = true, true
	_, err = direct.store.UpsertFederationBinding(t.Context(), binding)
	require.NoError(t, err)
	direct.credential = config.FederationCredential{
		HubURL: root.http.URL, HubProjectID: root.project.ID, Token: grant.Token,
		Actor: grant.Actor, Capabilities: grant.Capabilities,
	}
	require.NoError(t, direct.credentials.StoreFederationCredential(t.Context(), direct.project.UID, direct.credential))
}
