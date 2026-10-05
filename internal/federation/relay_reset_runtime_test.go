package federation_test

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// R4/R5: an authenticated epoch change retrieves and installs the exact root
// checkpoint through the existing outbound sync, including a forwarded hop.
func TestRelaySignedResetRuntime(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRelayMatrixNode(t, backend, "company-member")
			personal := newRelayMatrixNode(t, backend, "personal-member")
			leaf := newRelayMatrixNode(t, backend, "leaf-member")
			project, err := root.store.CreateProject(t.Context(), "shared-project")
			require.NoError(t, err)
			root.project = project
			_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
			require.NoError(t, err)
			public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
			pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
			require.NoError(t, root.store.PinRootAuthority(t.Context(), pin))
			issue, _, err := root.store.CreateIssue(db.WithRootAttribution(t.Context(), root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Author: "source-agent", Title: "Retained signed creator"})
			require.NoError(t, err)
			identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: root.store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
			artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
			require.NoError(t, err)
			_, err = root.store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(t.Context(), artifact)
			require.NoError(t, err)
			enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
			syncRelayMatrixNode(t, personal)
			binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
			require.NoError(t, err)
			creator := root.store.(interface {
				CreateRelayReset(context.Context, string, db.RootAttributionSigner) (db.RelayResetCheckpoint, error)
			})
			checkpoint, err := creator.CreateRelayReset(t.Context(), binding.RelayConfig.BindingUID, root.signer)
			require.NoError(t, err)
			executor := root.store.(interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
			})
			query := "DELETE FROM events WHERE project_id=?"
			if backend == "postgres" {
				query = "DELETE FROM events WHERE project_id=$1"
			}
			_, err = executor.ExecContext(t.Context(), query, project.ID)
			require.NoError(t, err)
			syncRelayMatrixNode(t, personal)
			installed, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
			require.NoError(t, err)
			require.Equal(t, checkpoint.Translation.Authority.Epoch, installed.RelayConfig.ResetEpoch)
			mirrored, err := personal.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "verified", mirrored.Verification)
			enrollRelayMatrixReplica(t, personal, leaf, "leaf-alias", false)
			syncRelayMatrixNode(t, leaf)
			leafBinding, err := leaf.store.FederationBindingByProject(t.Context(), leaf.project.ID)
			require.NoError(t, err)
			forwarder := personal.store.(interface {
				CreateRelayReset(context.Context, string, db.RootAttributionSigner) (db.RelayResetCheckpoint, error)
			})
			forwarded, err := forwarder.CreateRelayReset(t.Context(), leafBinding.RelayConfig.BindingUID, db.RootAttributionSigner{})
			require.NoError(t, err)
			require.Equal(t, checkpoint.Manifest, forwarded.Manifest)
			require.Equal(t, checkpoint.Snapshot, forwarded.Snapshot)
			pending, err := personal.store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(t.Context(), forwarded.Translation.Authority.Epoch), leafBinding.RelayConfig.BindingUID, db.RelayStreamArtifact, 32)
			require.NoError(t, err)
			require.Len(t, pending, 1, "forwarded reset must reoffer durable artifacts in the new child epoch")
			require.Equal(t, artifact.Digest, pending[0].SourceUID)
			require.Equal(t, forwarded.Translation.Authority.Epoch, pending[0].Epoch)
			syncRelayMatrixNode(t, leaf)
			proof, err := leaf.store.EntityAttribution(t.Context(), project.UID, "issue", issue.UID)
			require.NoError(t, err)
			require.NoError(t, db.VerifyRootReceipt(pin, proof))
			syncRelayMatrixNode(t, leaf)
			for _, node := range []*relayMatrixNode{personal, leaf} {
				retained, err := node.store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(t.Context(), project.UID, artifact.Digest)
				require.NoError(t, err)
				require.Equal(t, artifact, retained, "signed reset preserves canonical vectors for exact-digest reuse")
			}
			// No checkpoint bytes are exposed to missing, ordinary-user or wrong
			// project credentials. Cached downloads still enforce parent revocation.
			sibling, err := root.store.CreateProject(t.Context(), "private-project")
			require.NoError(t, err)
			checkDenied := func(token string, projectID int64) {
				t.Helper()
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/federation/relay/reset", root.http.URL, projectID), nil)
				require.NoError(t, err)
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				response, err := root.http.Client().Do(req)
				require.NoError(t, err)
				raw, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound}, response.StatusCode, string(raw))
				require.NotContains(t, string(raw), "snapshot_uid")
				require.NotContains(t, string(raw), "signature")
				require.NotContains(t, string(raw), issue.UID)
			}
			checkDenied("", project.ID)
			checkDenied(root.userToken, project.ID)
			checkDenied(personal.credential.Token, sibling.ID)
			tokens, err := root.store.ListAPITokens(t.Context())
			require.NoError(t, err)
			require.Len(t, tokens, 1)
			_, _, err = root.store.RevokeAPIToken(t.Context(), tokens[0].ID, "admin")
			require.NoError(t, err)
			checkDenied(personal.credential.Token, project.ID)
		})
	}
}
