package dbtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/uid"
)

// RunArtifactRelayOutbox exercises artifact relay outbox on the supplied native store.
// R7: durable artifact retention atomically queues metadata-only deliveries,
// independently of issue events, and exact retries keep the same identities.
func RunArtifactRelayOutbox(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "artifact-relay-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "artifact-parent-test-token", Actor: "member", AdminActor: "admin"})
	require.NoError(t, err)
	//nolint:gosec // This token is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "artifact-peer-test-token"})
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Shared vectors", Body: "Original content", Author: "assistant"})
	require.NoError(t, err)
	identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
	require.NoError(t, err)
	artifacts := store.(db.EmbeddingArtifactStorage)
	durable, err := artifacts.RetainEmbeddingArtifact(ctx, artifact)
	require.NoError(t, err)
	require.True(t, durable)
	pending, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamArtifact, 32)
	require.NoError(t, err)
	require.Len(t, pending, 1, "artifact delivery is queued with the retained bytes")
	first := pending[0]
	require.Equal(t, artifact.Digest, first.SourceUID)
	require.Equal(t, artifact.Digest, first.SourceHash)
	var manifest embedding.ArtifactManifest
	require.NoError(t, json.Unmarshal(first.Body, &manifest, json.RejectUnknownMembers(true)))
	require.Equal(t, artifact.Manifest(), manifest)
	require.NotContains(t, string(first.Body), "vector_bytes\"")
	durable, err = artifacts.RetainEmbeddingArtifact(ctx, artifact)
	require.NoError(t, err)
	require.True(t, durable)
	retry, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamArtifact, 32)
	require.NoError(t, err)
	require.Equal(t, pending, retry)
	require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, first.Epoch, db.RelayStreamArtifact, first.Sequence, first.Digest))
	require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, first.Epoch, db.RelayStreamArtifact, first.Sequence, first.Digest))
	_, _, err = store.RevokeAPIToken(ctx, parent.ID, "admin")
	require.NoError(t, err)
	_, err = store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamArtifact, 32)
	require.Error(t, err, "revoked grants cannot inventory even an acknowledged artifact")
}

// RunArtifactManifestAcknowledgedPrefix exercises artifact manifest acknowledged prefix on the supplied native store.
// R7/A6: exact durable hits advance only a contiguous emitted offer prefix;
// missing metadata is retained for an identical retry without vector bytes.
func RunArtifactManifestAcknowledgedPrefix(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "artifact-relay-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "artifact-parent-test-token", Actor: "member", AdminActor: "admin"})
	require.NoError(t, err)
	//nolint:gosec // This token is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "artifact-peer-test-token"})
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Shared vectors", Body: "Original content", Author: "assistant"})
	require.NoError(t, err)
	identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
	require.NoError(t, err)

	artifacts := store.(db.EmbeddingArtifactStorage)
	sets := []embedding.EmbeddingArtifact{artifact}
	for _, vector := range [][]float32{{0, 1}, {-1, 0}} {
		next, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{vector})
		require.NoError(t, err)
		sets = append(sets, next)
	}
	for _, i := range []int{0, 2} {
		durable, err := artifacts.RetainEmbeddingArtifact(ctx, sets[i])
		require.NoError(t, err)
		require.True(t, durable)
	}
	batch := db.RelayBatch{Stream: db.RelayStreamArtifact}
	for i, sequence := range []int64{7, 13, 21} {
		body, err := json.Marshal(sets[i].Manifest())
		require.NoError(t, err)
		envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID, ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), SenderInstanceUID: grant.Enrollment.SpokeInstanceUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: grant.Enrollment.RelayResetEpoch, Sequence: sequence, Stream: db.RelayStreamArtifact, Path: []string{grant.Enrollment.SpokeInstanceUID}, SourceUID: sets[i].Digest, SourceHash: sets[i].Digest, Body: body})
		require.NoError(t, err)
		batch.Envelopes = append(batch.Envelopes, envelope)
	}
	accepted, err := store.AcceptRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, batch)
	require.NoError(t, err)
	require.Equal(t, int64(7), accepted.Through, "later durable hit cannot skip unresolved middle")
	require.Equal(t, batch.Envelopes[0].Digest, accepted.Digest)
	require.Equal(t, []string{sets[1].Digest}, accepted.MissingDigests)
	retry, err := store.AcceptRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, batch)
	require.NoError(t, err)
	require.Equal(t, accepted, retry, "lost manifest reply preserves exact hop mappings")
	bad := sets[1]
	bad.Chunks = append([]embedding.ChunkArtifact(nil), bad.Chunks...)
	bad.Chunks[0].VectorBytes = append([]byte(nil), bad.Chunks[0].VectorBytes...)
	bad.Chunks[0].VectorBytes[0] ^= 1
	batch.Artifacts = []embedding.EmbeddingArtifact{bad}
	_, err = store.AcceptRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, batch)
	require.Error(t, err)
	_, err = artifacts.StoredEmbeddingArtifact(ctx, project.UID, sets[1].Digest)
	require.ErrorIs(t, err, db.ErrNotFound, "bad bytes do not establish durable presence")
	batch.Artifacts = []embedding.EmbeddingArtifact{sets[1]}
	original := batch.Envelopes[2]
	broken := original
	broken.Body = []byte(`{"invalid":"manifest"}`)
	broken, err = db.SealRelayEnvelope(broken)
	require.NoError(t, err)
	batch.Envelopes[2] = broken
	_, err = store.AcceptRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, batch)
	require.Error(t, err, "failure after retaining a valid miss rolls back the entire transaction")
	_, err = artifacts.StoredEmbeddingArtifact(ctx, project.UID, sets[1].Digest)
	require.ErrorIs(t, err, db.ErrNotFound)
	batch.Envelopes[2] = original
	batch.Artifacts = nil
	retry, err = store.AcceptRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, batch)
	require.NoError(t, err)
	require.Equal(t, accepted, retry, "rolled back bytes and mappings leave the same missing prefix")
	batch.Artifacts = []embedding.EmbeddingArtifact{sets[1]}
	accepted, err = store.AcceptRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, batch)
	require.NoError(t, err)
	require.Equal(t, int64(21), accepted.Through)
	require.Equal(t, batch.Envelopes[2].Digest, accepted.Digest)
	require.Empty(t, accepted.MissingDigests)
	duplicateSource := batch.Envelopes[0]
	duplicateSource.Sequence = 30
	duplicateSource, err = db.SealRelayEnvelope(duplicateSource)
	require.NoError(t, err)
	_, err = store.AcceptRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayBatch{
		Stream: db.RelayStreamArtifact, After: 21, Envelopes: []db.RelayEnvelope{duplicateSource},
	})
	require.ErrorIs(t, err, db.ErrFederationIngestValidation, "an artifact source UID cannot be offered at a second sequence in the same epoch")
	retained, err := artifacts.StoredEmbeddingArtifact(ctx, project.UID, sets[1].Digest)
	require.NoError(t, err)
	require.Equal(t, sets[1], retained)
	retry, err = store.AcceptRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, batch)
	require.NoError(t, err)
	require.Equal(t, accepted, retry, "lost bytes reply preserves committed acceptance")
	_, _, err = store.RevokeAPIToken(ctx, parent.ID, "admin")
	require.NoError(t, err)
	_, err = store.AcceptRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, batch)
	require.Error(t, err, "retained offers do not preserve authority after revocation")

}

// RunArtifactManifestStaging exercises artifact manifest staging on the supplied native store.
// R7/A6: pre-content bytes and manifests never acknowledge an artifact, while
// ordinary issue delivery remains free to establish its durable retention.
func RunArtifactManifestStaging(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "artifact-relay-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "artifact-parent-test-token", Actor: "member", AdminActor: "admin"})
	require.NoError(t, err)
	//nolint:gosec // This token is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "artifact-peer-test-token"})
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Shared vectors", Body: "Original content", Author: "assistant"})
	require.NoError(t, err)
	identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	identity.IssueUID = "00000000000000000000000005"
	artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
	require.NoError(t, err)
	body, err := json.Marshal(artifact.Manifest())
	require.NoError(t, err)
	envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID, ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), SenderInstanceUID: grant.Enrollment.SpokeInstanceUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: grant.Enrollment.RelayResetEpoch, Sequence: 7, Stream: db.RelayStreamArtifact, Path: []string{grant.Enrollment.SpokeInstanceUID}, SourceUID: artifact.Digest, SourceHash: artifact.Digest, Body: body})
	require.NoError(t, err)
	batch := db.RelayBatch{Stream: db.RelayStreamArtifact, Envelopes: []db.RelayEnvelope{envelope}}
	for _, bytes := range []bool{false, true, false} {
		batch.Artifacts = nil
		if bytes {
			batch.Artifacts = []embedding.EmbeddingArtifact{artifact}
		}
		accepted, err := store.AcceptRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, batch)
		require.NoError(t, err)
		require.Zero(t, accepted.Through)
		require.Empty(t, accepted.Digest)
		require.Equal(t, []string{artifact.Digest}, accepted.MissingDigests)
	}
	_, err = store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(ctx, project.UID, artifact.Digest)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, UID: identity.IssueUID, Title: issue.Title, Body: issue.Body, Author: "assistant"})
	require.NoError(t, err, "independent issue creation is not blocked by artifact staging")
	batch.Artifacts = nil // Previously staged bytes must satisfy the retry after content arrives.
	accepted, err := store.AcceptRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, batch)
	require.NoError(t, err)
	require.Equal(t, envelope.Sequence, accepted.Through)
	require.Empty(t, accepted.MissingDigests)
	retained, err := store.(db.EmbeddingArtifactStorage).StoredEmbeddingArtifact(ctx, project.UID, artifact.Digest)
	require.NoError(t, err)
	require.Equal(t, artifact, retained, "manifest retry promotes exact staged bytes without a vector payload")
	replayed, err := store.AcceptRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, batch)
	require.NoError(t, err)
	require.Equal(t, accepted, replayed, "lost promotion reply retries the same durable prefix")
	_, err = store.AcceptRelayDeliveries(db.WithAuthorizedProjects(ctx, []string{}), grant.Enrollment.RelayBindingUID, batch)
	require.ErrorIs(t, err, db.ErrNotFound)
}

type relayArtifactDownloader interface {
	RelayArtifactPayloads(context.Context, string, int64, []string) ([]embedding.EmbeddingArtifact, error)
}

// RunArtifactManifestDownload exercises artifact manifest download on the supplied native store.
// R7/A2: only exact previously emitted offers in the live authorized epoch
// can disclose complete bytes; a manifest does not grant access after revoke.
func RunArtifactManifestDownload(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "artifact-relay-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "artifact-parent-test-token", Actor: "member", AdminActor: "admin"})
	require.NoError(t, err)
	//nolint:gosec // This token is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "artifact-peer-test-token"})
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Shared vectors", Body: "Original content", Author: "assistant"})
	require.NoError(t, err)
	identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
	require.NoError(t, err)

	downloader, ok := store.(relayArtifactDownloader)
	require.True(t, ok, "native exact-digest download is required")
	durable, err := store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
	require.NoError(t, err)
	require.True(t, durable)
	payloads, err := downloader.RelayArtifactPayloads(ctx, grant.Enrollment.RelayBindingUID, grant.Enrollment.RelayResetEpoch, []string{artifact.Digest})
	require.ErrorIs(t, err, db.ErrNotFound, "bytes cannot be requested before the corresponding offer was emitted")
	require.Empty(t, payloads)
	pending, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamArtifact, 32)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	for range 2 {
		payloads, err = downloader.RelayArtifactPayloads(ctx, grant.Enrollment.RelayBindingUID, grant.Enrollment.RelayResetEpoch, []string{artifact.Digest})
		require.NoError(t, err)
		require.Equal(t, []embedding.EmbeddingArtifact{artifact}, payloads, "lost download reply retries exact retained original float32 bytes")
	}
	privateProject, err := store.CreateProject(ctx, "unselected-artifact-project")
	require.NoError(t, err)
	privateIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: privateProject.ID, Title: "Private vector canary", Author: "assistant"})
	require.NoError(t, err)
	privateIdentity := identity
	privateIdentity.ProjectUID, privateIdentity.IssueUID = privateProject.UID, privateIssue.UID
	privateArtifact, err := embedding.NewArtifact(privateIdentity, embedding.EmbedText(privateIssue.Title, privateIssue.Body), [][]float32{{-1, -1}})
	require.NoError(t, err)
	_, err = store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, privateArtifact)
	require.NoError(t, err)
	payloads, err = downloader.RelayArtifactPayloads(ctx, grant.Enrollment.RelayBindingUID, grant.Enrollment.RelayResetEpoch, []string{artifact.Digest, privateArtifact.Digest})
	require.ErrorIs(t, err, db.ErrNotFound)
	require.Empty(t, payloads, "an unselected digest returns no partial bytes or existence information")
	payloads, err = downloader.RelayArtifactPayloads(ctx, grant.Enrollment.RelayBindingUID, grant.Enrollment.RelayResetEpoch+1, []string{artifact.Digest})
	require.Error(t, err)
	require.Empty(t, payloads)
	payloads, err = downloader.RelayArtifactPayloads(db.WithAuthorizedProjects(ctx, []string{}), grant.Enrollment.RelayBindingUID, grant.Enrollment.RelayResetEpoch, []string{artifact.Digest})
	require.ErrorIs(t, err, db.ErrNotFound)
	require.Empty(t, payloads)
	_, _, err = store.RevokeAPIToken(ctx, parent.ID, "admin")
	require.NoError(t, err)
	payloads, err = downloader.RelayArtifactPayloads(ctx, grant.Enrollment.RelayBindingUID, grant.Enrollment.RelayResetEpoch, []string{artifact.Digest})
	require.Error(t, err, "revocation between offer and download denies bytes")
	require.Empty(t, payloads)
}

// RunArtifactManifestOfferBounds exercises artifact manifest offer bounds on the supplied native store.
// R7: sender batches must fit the receiver's manifest admission bounds even
// when an existing API client asks for its ordinary 100-offer page.
func RunArtifactManifestOfferBounds(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "artifact-relay-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "artifact-parent-test-token", Actor: "member", AdminActor: "admin"})
	require.NoError(t, err)
	//nolint:gosec // This token is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "artifact-peer-test-token"})
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Shared vectors", Body: "Original content", Author: "assistant"})
	require.NoError(t, err)
	identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	for i := range 33 {
		artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{float32(i + 1), 0}})
		require.NoError(t, err)
		_, err = store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
		require.NoError(t, err)
	}
	pending, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamArtifact, 100)
	require.NoError(t, err)
	require.Len(t, pending, 32, "artifact pages honor the receiver's 32-offer bound")
	last := pending[len(pending)-1]
	require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, last.Epoch, db.RelayStreamArtifact, last.Sequence, last.Digest))
	pending, err = store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamArtifact, 100)
	require.NoError(t, err)
	require.Len(t, pending, 1, "remaining offer retains its independent delivery identity")
}

// RunArtifactManifestBootstrap exercises artifact manifest bootstrap on the supplied native store.
// R7: newly selected peers receive preexisting durable artifacts without a new
// source issue event or embedding call; expiring staging is never advertised.
func RunArtifactManifestBootstrap(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "artifact-relay-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "artifact-parent-test-token", Actor: "member", AdminActor: "admin"})
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Shared vectors", Body: "Original content", Author: "assistant"})
	require.NoError(t, err)
	identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
	require.NoError(t, err)

	_, err = store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
	require.NoError(t, err)
	stageIdentity := identity
	stageIdentity.IssueUID = "00000000000000000000000005"
	staged, err := embedding.NewArtifact(stageIdentity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{0, 1}})
	require.NoError(t, err)
	durable, err := store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, staged)
	require.NoError(t, err)
	require.False(t, durable)
	//nolint:gosec // This token is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "artifact-peer-test-token"})
	require.NoError(t, err)
	pending, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamArtifact, 32)
	require.NoError(t, err)
	require.Len(t, pending, 1, "preexisting original artifact is seeded into the newly selected binding")
	require.Equal(t, artifact.Digest, pending[0].SourceUID)
	var manifest embedding.ArtifactManifest
	require.NoError(t, json.Unmarshal(pending[0].Body, &manifest))
	require.Equal(t, artifact.Manifest(), manifest)
	require.NotContains(t, string(pending[0].Body), `"vector_bytes"`)
}

// RunArtifactManifestOutgoingBootstrap exercises artifact manifest outgoing bootstrap on the supplied native store.
// R7: installing the selected outgoing relay binding also advertises durable
// artifacts already computed locally, without requiring new issue edits.
func RunArtifactManifestOutgoingBootstrap(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "outgoing-artifact-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "upstream-member", PushEnabled: true, Enabled: true})
	require.NoError(t, err)
	rootUID := "00000000000000000000000002"
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Preexisting local vectors", Author: "source-assistant"})
	require.NoError(t, err)
	identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
	require.NoError(t, err)
	_, err = store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
	require.NoError(t, err)
	config := db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: "00000000000000000000000006", UpstreamInstanceUID: rootUID, AuthorityUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "local-member", ServeDownstream: true, ResetEpoch: 1}
	_, err = store.SetRelayBindingConfig(ctx, project.ID, config)
	require.NoError(t, err)
	pending, err := store.PendingRelayDeliveries(ctx, config.BindingUID, db.RelayStreamArtifact, 32)
	require.NoError(t, err)
	require.Len(t, pending, 1, "initial outgoing configuration queues preexisting complete artifacts")
	require.Equal(t, artifact.Digest, pending[0].SourceUID)
	_, err = store.SetRelayBindingConfig(ctx, project.ID, config)
	require.NoError(t, err)
	retry, err := store.PendingRelayDeliveries(ctx, config.BindingUID, db.RelayStreamArtifact, 32)
	require.NoError(t, err)
	require.Equal(t, pending, retry)
}

// RunArtifactSignedResetManifests exercises artifact signed reset manifests on the supplied native store.
// R7/A6: root-signed resets commit full chunk manifests, omit vector bytes,
// and emit complete artifacts independently in the new authenticated hop epoch.
func RunArtifactSignedResetManifests(t *testing.T, store db.Storage, compact func(context.Context, int64) error) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "artifact-relay-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "artifact-parent-test-token", Actor: "member", AdminActor: "admin"})
	require.NoError(t, err)
	//nolint:gosec // This token is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "artifact-peer-test-token"})
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Shared vectors", Body: "Original content", Author: "assistant"})
	require.NoError(t, err)
	identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
	require.NoError(t, err)

	_, err = store.(db.EmbeddingArtifactStorage).RetainEmbeddingArtifact(ctx, artifact)
	require.NoError(t, err)
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt, db.RelayStreamArtifact} {
		pending, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, stream, 32)
		require.NoError(t, err)
		if len(pending) > 0 {
			last := pending[len(pending)-1]
			require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, last.Epoch, stream, last.Sequence, last.Digest))
		}
	}
	require.NoError(t, compact(ctx, project.ID))
	checkpoint, err := store.(db.RelayResetStore).CreateRelayReset(ctx, grant.Enrollment.RelayBindingUID, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private})
	require.NoError(t, err)
	var manifests []embedding.ArtifactManifest
	require.NoError(t, json.Unmarshal(checkpoint.Snapshot.Artifacts, &manifests, json.RejectUnknownMembers(true)))
	require.Equal(t, []embedding.ArtifactManifest{artifact.Manifest()}, manifests)
	require.NotContains(t, string(checkpoint.Snapshot.Artifacts), `"vector_bytes"`)
	require.NoError(t, db.VerifyRootResetManifest(db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}, checkpoint.Manifest, checkpoint.Snapshot))
	require.Zero(t, checkpoint.Translation.HopBaselines.Artifacts, "metadata cannot acknowledge missing vectors")
	for _, mode := range []string{"duplicate", "foreign_project", "missing_issue", "bad_chunk"} {
		t.Run(mode, func(t *testing.T) {
			candidate := artifact.Manifest()
			set := []embedding.ArtifactManifest{candidate}
			switch mode {
			case "duplicate":
				set = append(set, candidate)
			case "foreign_project":
				set[0].ProjectUID = "00000000000000000000000007"
			case "missing_issue":
				set[0].IssueUID = "00000000000000000000000007"
			case "bad_chunk":
				set[0].Chunks[0].Index = 1
			}
			badSnapshot := checkpoint.Snapshot
			badSnapshot.Artifacts, err = json.Marshal(set)
			require.NoError(t, err)
			_, _, err = db.DecodeRootResetPayload(db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}, badSnapshot)
			require.Error(t, err, "signed metadata still requires complete valid project-scoped manifests")
		})
	}

	_, err = store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, checkpoint.Translation.Authority.Epoch+1), grant.Enrollment.RelayBindingUID, db.RelayStreamArtifact, 32)
	require.ErrorIs(t, err, db.ErrFederationIngestValidation, "an arbitrary epoch cannot activate a prepared reset")
	_, err = store.PendingRelayDeliveries(db.WithAuthorizedProjects(db.WithRelayRequestedEpoch(ctx, checkpoint.Translation.Authority.Epoch), []string{}), grant.Enrollment.RelayBindingUID, db.RelayStreamArtifact, 32)
	require.Error(t, err, "a prepared checkpoint does not confer project access")
	pending, err := store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, checkpoint.Translation.Authority.Epoch), grant.Enrollment.RelayBindingUID, db.RelayStreamArtifact, 32)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, artifact.Digest, pending[0].SourceUID)
	require.Equal(t, checkpoint.Translation.Authority.Epoch, pending[0].Epoch)
	_, err = store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, grant.Enrollment.RelayResetEpoch), grant.Enrollment.RelayBindingUID, db.RelayStreamArtifact, 32)
	require.ErrorIs(t, err, db.ErrFederationIngestValidation, "the activated namespace rejects retired epoch requests")
}

// RunArtifactResetInstallation exercises artifact reset installation on the supplied native store.
// R7/A6: a reset retains only its authoritative artifact set, promotes validated
// pre-content bytes after rebuilding the issue, and never invents bytes for a manifest.
func RunArtifactResetInstallation(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "artifact-reset-replica")
	require.NoError(t, err)
	obsolete, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Removed by reset", Author: "source-assistant"})
	require.NoError(t, err)
	makeArtifact := func(issueUID, title string) embedding.EmbeddingArtifact {
		identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issueUID, ProducerInstanceUID: "00000000000000000000000002", Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
		artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(title, ""), [][]float32{{1, 0}})
		require.NoError(t, err)
		return artifact
	}
	artifacts := store.(db.EmbeddingArtifactStorage)
	removedArtifact := makeArtifact(obsolete.UID, obsolete.Title)
	durable, err := artifacts.RetainEmbeddingArtifact(ctx, removedArtifact)
	require.NoError(t, err)
	require.True(t, durable)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	rootUID := "00000000000000000000000002"
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "member", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	bindingUID, err := uid.New()
	require.NoError(t, err)
	_, err = store.SetRelayBindingConfig(ctx, project.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID, AuthorityUID: rootUID, UpstreamInstanceUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "member", ResetEpoch: 1})
	require.NoError(t, err)
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt, db.RelayStreamArtifact} {
		pending, err := store.PendingRelayDeliveries(ctx, bindingUID, stream, 32)
		require.NoError(t, err)
		if len(pending) > 0 {
			last := pending[len(pending)-1]
			require.NoError(t, store.AckRelayDeliveries(ctx, bindingUID, last.Epoch, stream, last.Sequence, last.Digest))
		}
	}
	targetUID, err := uid.New()
	require.NoError(t, err)
	target := makeArtifact(targetUID, "Reset target")
	durable, err = artifacts.RetainEmbeddingArtifact(ctx, target)
	require.NoError(t, err)
	require.False(t, durable)
	missingUID, err := uid.New()
	require.NoError(t, err)
	missing := makeArtifact(missingUID, "Manifest only")
	entities := [][]byte{}
	for _, item := range []struct{ uid, title string }{{targetUID, "Reset target"}, {missingUID, "Manifest only"}} {
		source := newRemoteEvent(t, project, &item.uid, "issue.snapshot", "source-assistant", rootUID, 400, jsontext.Value(`{"uid":"`+item.uid+`","title":"`+item.title+`","author":"source-assistant","metadata":{},"comments":[],"labels":[],"links":[]}`))
		raw, err := db.EncodeRelaySourceEvent(source)
		require.NoError(t, err)
		entities = append(entities, raw)
	}
	entityBytes, err := json.Marshal(entities)
	require.NoError(t, err)
	provenance, err := json.Marshal(db.RootResetProvenance{Keys: []db.RootKeyPin{pin}, Receipts: []db.AttributionReceipt{}, Entities: []db.EntityProvenance{}})
	require.NoError(t, err)
	manifests, err := json.Marshal([]embedding.ArtifactManifest{target.Manifest(), missing.Manifest()})
	require.NoError(t, err)
	snapshot := db.RootResetSnapshot{Events: []byte(`[]`), Entities: entityBytes, Provenance: provenance, Artifacts: manifests}
	snapshotUID, err := uid.New()
	require.NoError(t, err)
	manifest, err := db.SignRootResetManifest(db.RootResetManifest{Version: 1, ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: pin.KeyID, ResetEpoch: 2, SnapshotUID: snapshotUID}, snapshot, private)
	require.NoError(t, err)
	translation := db.RelayResetTranslation{Authority: db.RelayHopAuthority{BindingUID: bindingUID, ProjectUID: project.UID, AuthorityUID: rootUID, SenderInstanceUID: rootUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: 2}, SnapshotUID: snapshotUID, SnapshotDigest: manifest.SnapshotDigest}
	installer := store.(db.RelayResetStore)
	require.ErrorIs(t, installer.InstallRelayReset(db.WithAuthorizedProjects(ctx, []string{}), bindingUID, manifest, snapshot, translation), db.ErrNotFound)
	unchanged, err := artifacts.StoredEmbeddingArtifact(ctx, project.UID, removedArtifact.Digest)
	require.NoError(t, err)
	require.Equal(t, removedArtifact, unchanged, "denied installation preserves the existing projection and bytes")
	require.NoError(t, installer.InstallRelayReset(ctx, bindingUID, manifest, snapshot, translation))
	_, err = artifacts.StoredEmbeddingArtifact(ctx, project.UID, removedArtifact.Digest)
	require.ErrorIs(t, err, db.ErrNotFound, "reset must remove portable bytes for removed entities")
	retained, err := artifacts.StoredEmbeddingArtifact(ctx, project.UID, target.Digest)
	require.NoError(t, err, "validated staging must reconcile after snapshot issue installation")
	require.Equal(t, target, retained)
	_, err = artifacts.StoredEmbeddingArtifact(ctx, project.UID, missing.Digest)
	require.ErrorIs(t, err, db.ErrNotFound, "a signed manifest does not supply durable vector bytes")
	require.NoError(t, installer.InstallRelayReset(ctx, bindingUID, manifest, snapshot, translation), "lost reply retries the same snapshot and artifact retention")
}

// RunArtifactPopulatedAdoption exercises artifact populated adoption on the supplied native store.
// R4/R7: adopting a populated local project preserves current and stale complete
// vectors under its selected hub UID, while failed adoption preserves old bytes.
func RunArtifactPopulatedAdoption(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "artifact-adoption-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Original input", Body: strings.Repeat("界", 2100), Author: "source-assistant"})
	require.NoError(t, err)
	identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	input := embedding.EmbedText(issue.Title, issue.Body)
	stale, err := embedding.NewArtifact(identity, input, [][]float32{{1, 0}, {0, 1}})
	require.NoError(t, err)
	artifacts := store.(db.EmbeddingArtifactStorage)
	_, err = artifacts.RetainEmbeddingArtifact(ctx, stale)
	require.NoError(t, err)
	title := "Current input"
	currentIssue, _, _, err := store.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &title, Actor: "member"})
	require.NoError(t, err)
	currentInput := embedding.EmbedText(currentIssue.Title, currentIssue.Body)
	current, err := embedding.NewArtifact(identity, currentInput, [][]float32{{0.6, 0.8}, {0.8, 0.6}})
	require.NoError(t, err)
	_, err = artifacts.RetainEmbeddingArtifact(ctx, current)
	require.NoError(t, err)
	occupied, err := store.CreateProject(ctx, "occupied-hub-identity")
	require.NoError(t, err)
	params := db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: occupied.UID, Actor: "member", RelayProtocolVersion: db.RelayProtocolVersion}
	_, err = store.AdoptProjectIntoFederation(ctx, params)
	require.Error(t, err)
	for _, original := range []embedding.EmbeddingArtifact{stale, current} {
		retained, err := artifacts.StoredEmbeddingArtifact(ctx, project.UID, original.Digest)
		require.NoError(t, err)
		require.Equal(t, original, retained, "failed adoption retains source identity and complete bytes")
	}
	newUID, err := uid.New()
	require.NoError(t, err)
	params.HubProjectUID = newUID
	adopted, err := store.AdoptProjectIntoFederation(ctx, params)
	require.NoError(t, err, "portable artifact FK must not block populated adoption")
	require.Equal(t, newUID, adopted.Project.UID)
	for i, original := range []embedding.EmbeddingArtifact{stale, current} {
		originalInput := input
		values := [][]float32{{1, 0}, {0, 1}}
		if i == 1 {
			originalInput = currentInput
			values = [][]float32{{0.6, 0.8}, {0.8, 0.6}}
		}
		reboundIdentity := identity
		reboundIdentity.ProjectUID = newUID
		expected, err := embedding.NewArtifact(reboundIdentity, originalInput, values)
		require.NoError(t, err)
		retained, err := artifacts.StoredEmbeddingArtifact(ctx, newUID, expected.Digest)
		require.NoError(t, err)
		require.Equal(t, expected, retained)
		require.NotEqual(t, original.Digest, retained.Digest, "project UID participates in exact artifact identity")
		require.Equal(t, original.Chunks, retained.Chunks, "adoption changes metadata and digest, never original vectors")
		_, err = artifacts.StoredEmbeddingArtifact(ctx, project.UID, original.Digest)
		require.ErrorIs(t, err, db.ErrNotFound)
	}
	retry, err := store.AdoptProjectIntoFederation(ctx, params)
	require.NoError(t, err)
	require.Equal(t, newUID, retry.Project.UID)
	manifests, err := artifacts.EmbeddingArtifactManifests(ctx, newUID, 10)
	require.NoError(t, err)
	require.Len(t, manifests, 2)
}

// RunArtifactIssuePurge exercises artifact issue purge on the supplied native store.
// R7/A13: permanent issue deletion removes all original portable bytes for
// that issue, including old inputs, without touching a sibling artifact.
func RunArtifactIssuePurge(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "artifact-purge-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Original portable input", Author: "member"})
	require.NoError(t, err)
	sibling, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Retained sibling", Author: "member"})
	require.NoError(t, err)
	artifacts := store.(db.EmbeddingArtifactStorage)
	identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	original, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
	require.NoError(t, err)
	durable, err := artifacts.RetainEmbeddingArtifact(ctx, original)
	require.NoError(t, err)
	require.True(t, durable)
	title := "New portable input"
	issue, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &title, Actor: "member"})
	require.NoError(t, err)
	current, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{0, 1}})
	require.NoError(t, err)
	durable, err = artifacts.RetainEmbeddingArtifact(ctx, current)
	require.NoError(t, err)
	require.True(t, durable)
	identity.IssueUID = sibling.UID
	retained, err := embedding.NewArtifact(identity, embedding.EmbedText(sibling.Title, sibling.Body), [][]float32{{1, 0}})
	require.NoError(t, err)
	durable, err = artifacts.RetainEmbeddingArtifact(ctx, retained)
	require.NoError(t, err)
	require.True(t, durable)
	// A root must not delete bytes while a live peer is owed their manifest.
	// Drain the exact emitted prefixes before allowing permanent deletion.
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "purge-parent-test-token", Actor: "member", AdminActor: "admin"})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "purge-peer-test-token"})
	require.NoError(t, err)
	_, err = store.PurgeIssue(ctx, issue.ID, "member", nil)
	require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
	_, err = store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	_, err = artifacts.StoredEmbeddingArtifact(ctx, project.UID, original.Digest)
	require.NoError(t, err, "refused purge preserves downloadable vectors")
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt, db.RelayStreamArtifact} {
		for {
			pending, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, stream, 32)
			require.NoError(t, err)
			if len(pending) == 0 {
				break
			}
			last := pending[len(pending)-1]
			require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, last.Epoch, stream, last.Sequence, last.Digest))
		}
	}

	checkpoint, err := store.(db.RelayResetStore).CreateRelayReset(ctx, grant.Enrollment.RelayBindingUID, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private})
	require.NoError(t, err)
	_, err = store.PurgeIssue(ctx, issue.ID, "member", nil)
	require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush, "a prepared checkpoint is owed even before new-epoch vectors are emitted")
	for _, stream := range []string{db.RelayStreamEvent, db.RelayStreamReceipt, db.RelayStreamArtifact} {
		for {
			pending, err := store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, checkpoint.Translation.Authority.Epoch), grant.Enrollment.RelayBindingUID, stream, 32)
			require.NoError(t, err)
			if len(pending) == 0 {
				break
			}
			last := pending[len(pending)-1]
			require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, last.Epoch, stream, last.Sequence, last.Digest))
		}
	}
	_, err = store.PurgeIssue(ctx, issue.ID, "member", nil)
	require.NoError(t, err)
	for _, artifact := range []embedding.EmbeddingArtifact{original, current} {
		_, err = artifacts.StoredEmbeddingArtifact(ctx, project.UID, artifact.Digest)
		require.ErrorIs(t, err, db.ErrNotFound, "purged original bytes must not remain readable")
	}
	got, err := artifacts.StoredEmbeddingArtifact(ctx, project.UID, retained.Digest)
	require.NoError(t, err)
	require.Equal(t, retained, got)
	manifests, err := artifacts.EmbeddingArtifactManifests(ctx, project.UID, 10)
	require.NoError(t, err)
	require.Len(t, manifests, 1)
	require.Equal(t, retained.Manifest(), manifests[0])
}

// RunEmbeddingProducerRootOwnership exercises embedding producer root ownership on the supplied native store.
// Revised R7: a writable replica may edit ordinary metadata but cannot select
// the shared project's producer. Producer attribution labels grant no authority.
func RunEmbeddingProducerRootOwnership(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "producer-authority-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "upstream-member"})
	require.NoError(t, err)
	producer := db.ProjectEmbeddingProducer{ProducerInstanceUID: store.InstanceUID(), Recipe: embedding.ArtifactIdentity{Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "l2", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, Encoding: "float32-le", RecipeFingerprint: strings.Repeat("a", 64)}}
	raw, err := json.Marshal(producer)
	require.NoError(t, err)
	_, err = store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "upstream-member", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw}})
	require.ErrorIs(t, err, db.ErrFederatedSpokeUnsupported, "a writable replica cannot assign its own producer authority")
	unchanged, err := store.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	configured, err := db.ProjectEmbeddingProducerFromMetadata(unchanged.Metadata)
	require.NoError(t, err)
	require.Nil(t, configured)
	_, err = store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "upstream-member", Patch: map[string]jsontext.Value{"area": jsontext.Value(`"example-area"`)}})
	require.NoError(t, err, "ordinary replica metadata keeps its established write contract")
}

// RunEmbeddingProducerIngressOwnership exercises embedding producer ingress ownership on the supplied native store.
// R7: a writable enrolled replica cannot replace the root's stable producer
// through either the legacy event ingress or an authenticated relay envelope.
func RunEmbeddingProducerIngressOwnership(t *testing.T, store db.Storage) {
	for _, transport := range []string{"legacy", "relay"} {
		t.Run(transport, func(t *testing.T) {
			ctx := t.Context()
			project, err := store.CreateProject(ctx, "producer-ingress-"+transport)
			require.NoError(t, err)
			_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
			require.NoError(t, err)
			public, private, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
			parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "producer-parent-" + transport, Actor: "member", AdminActor: "owner"})
			require.NoError(t, err)
			peer := "00000000000000000000000006"
			grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion, Token: "producer-enrollment-" + transport})
			require.NoError(t, err)
			producer := projectProducerTestConfig(store.InstanceUID())
			raw, err := json.Marshal(producer)
			require.NoError(t, err)
			_, err = store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "owner", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw}})
			require.NoError(t, err)
			producer.ProducerInstanceUID = peer
			replacement, err := json.Marshal(producer)
			require.NoError(t, err)
			payload := jsontext.Value(`{"project_uid":"` + project.UID + `","diff":{"federation_embedding":{"from":` + string(raw) + `,"to":` + string(replacement) + `}}}`)
			source := newRemoteEvent(t, project, nil, "project.metadata_updated", "member", peer, 300, payload)
			if transport == "legacy" {
				_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: peer, BoundActor: "member", Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: source}}})
			} else {
				body, encodeErr := db.EncodeRelaySourceEvent(source)
				require.NoError(t, encodeErr)
				envelope, sealErr := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID, ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), SenderInstanceUID: peer, ReceiverInstanceUID: store.InstanceUID(), Epoch: 1, Sequence: 1, Stream: db.RelayStreamEvent, Path: []string{peer}, SourceUID: source.EventUID, SourceHash: source.ContentHash, Body: body})
				require.NoError(t, sealErr)
				_, err = store.AcceptRelayDeliveries(db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}, "member"), grant.Enrollment.RelayBindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope}})
			}
			require.ErrorIs(t, err, db.ErrFederationIngestValidation)
			current, err := store.ProjectByID(ctx, project.ID)
			require.NoError(t, err)
			configured, err := db.ProjectEmbeddingProducerFromMetadata(current.Metadata)
			require.NoError(t, err)
			require.Equal(t, store.InstanceUID(), configured.ProducerInstanceUID, "failed policy change must roll back")
		})
	}
}

// RunDownstreamCannotForgeRootEmbeddingProducer verifies that an enrolled
// relay cannot submit a root-origin claim to replace the shared producer
// configuration. Relay envelope paths and hashes are hop commitments, not
// proof that the root authored the source event.
func RunDownstreamCannotForgeRootEmbeddingProducer(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "producer-downstream-forgery")
	require.NoError(t, err)
	rootUID := "00000000000000000000000002"
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: rootUID,
		KeyID: db.RootPublicKeyID(public), PublicKey: public,
	}))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://hub.example", HubProjectID: 42,
		HubProjectUID: project.UID, Actor: "local-member",
		PushEnabled: true, Enabled: true,
	})
	require.NoError(t, err)
	bindingUID, err := uid.New()
	require.NoError(t, err)
	_, err = store.SetRelayBindingConfig(ctx, project.ID, db.RelayBindingConfig{
		ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID,
		UpstreamInstanceUID: rootUID, AuthorityUID: rootUID,
		HubPath:    []string{rootUID, store.InstanceUID()},
		LocalActor: "local-member", ServeDownstream: true, ResetEpoch: 1,
	})
	require.NoError(t, err)
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{
		Actor: "local-member", AdminActor: "admin", PlaintextToken: "producer-forgery-parent-token",
	})
	require.NoError(t, err)
	peer := "00000000000000000000000006"
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: peer,
		ProtocolVersion: db.RelayProtocolVersion, Token: "producer-forgery-child-token",
		ServeDownstream: true,
	})
	require.NoError(t, err)

	producer, err := json.Marshal(projectProducerTestConfig(peer))
	require.NoError(t, err)
	source := newRemoteEvent(t, project, nil, "project.metadata_updated", "root-admin", rootUID, 500,
		jsontext.Value(`{"project_uid":"`+project.UID+`","diff":{"federation_embedding":{"from":null,"to":`+string(producer)+`}}}`))
	body, err := db.EncodeRelaySourceEvent(source)
	require.NoError(t, err)
	envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{
		Version: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID,
		ProjectUID: project.UID, AuthorityUID: rootUID,
		SenderInstanceUID: peer, ReceiverInstanceUID: store.InstanceUID(),
		Epoch: 1, Sequence: 1, Stream: db.RelayStreamEvent,
		Path: []string{rootUID, peer}, SourceUID: source.EventUID,
		SourceHash: source.ContentHash, Body: body,
	})
	require.NoError(t, err)
	_, err = store.AcceptRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID,
		db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope}})
	require.ErrorIs(t, err, db.ErrFederationIngestValidation)
	current, err := store.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	configured, err := db.ProjectEmbeddingProducerFromMetadata(current.Metadata)
	require.NoError(t, err)
	require.Nil(t, configured, "a downstream grant cannot choose the root-owned producer")
}

func projectProducerTestConfig(producerUID string) db.ProjectEmbeddingProducer {
	return db.ProjectEmbeddingProducer{ProducerInstanceUID: producerUID, Recipe: embedding.ArtifactIdentity{Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64), Encoding: embedding.ArtifactFloat32Encoding}}
}

// RunEmbeddingProducerRootPropagation exercises embedding producer root propagation on the supplied native store.
// Root-origin producer metadata continues downstream through authenticated hops.
func RunEmbeddingProducerRootPropagation(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "producer-propagation-project")
	require.NoError(t, err)
	rootUID := "00000000000000000000000002"
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "root-member", PushEnabled: true, Enabled: true})
	require.NoError(t, err)
	bindingUID, err := uid.New()
	require.NoError(t, err)
	_, err = store.SetRelayBindingConfig(ctx, project.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID, UpstreamInstanceUID: rootUID, AuthorityUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "local-member", ServeDownstream: true, ResetEpoch: 1})
	require.NoError(t, err)
	producer := projectProducerTestConfig(rootUID)
	raw, err := json.Marshal(producer)
	require.NoError(t, err)
	source := newRemoteEvent(t, project, nil, "project.metadata_updated", "root-member", rootUID, 300, jsontext.Value(`{"project_uid":"`+project.UID+`","diff":{"federation_embedding":{"from":null,"to":`+string(raw)+`}}}`))
	body, err := db.EncodeRelaySourceEvent(source)
	require.NoError(t, err)
	envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: bindingUID, ProjectUID: project.UID, AuthorityUID: rootUID, SenderInstanceUID: rootUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: 1, Sequence: 1, Stream: db.RelayStreamEvent, Path: []string{rootUID}, SourceUID: source.EventUID, SourceHash: source.ContentHash, Body: body})
	require.NoError(t, err)
	_, err = store.AcceptRelayDeliveries(ctx, bindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope}})
	require.NoError(t, err)
	current, err := store.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	configured, err := db.ProjectEmbeddingProducerFromMetadata(current.Metadata)
	require.NoError(t, err)
	if configured == nil {
		t.Fatal("root producer configuration was not retained")
		return
	}
	require.Equal(t, producer, *configured)
}

// RunEmbeddingProducerConfigurationValidation exercises embedding producer configuration validation on the supplied native store.
// Review16726/R7: invalid reserved producer configuration must be rejected
// before it can poison every project's shared embedding fill loop.
func RunEmbeddingProducerConfigurationValidation(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "producer-validation-project")
	require.NoError(t, err)
	producer := projectProducerTestConfig(store.InstanceUID())
	valid, err := json.Marshal(producer)
	require.NoError(t, err)
	_, err = store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "owner", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: valid}})
	require.NoError(t, err)
	before, err := store.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	producer.Recipe.Dimensions = 0
	zeroDims, err := json.Marshal(producer)
	require.NoError(t, err)
	for _, bad := range []jsontext.Value{zeroDims, jsontext.Value(`"not-a-producer"`), jsontext.Value(`{"unexpected":true}`)} {
		_, err = store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "owner", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: bad}})
		require.ErrorIs(t, err, metadata.ErrInvalidValue)
		current, err := store.ProjectByID(ctx, project.ID)
		require.NoError(t, err)
		require.Equal(t, before.Revision, current.Revision)
		require.JSONEq(t, string(before.Metadata), string(current.Metadata))
	}
	_, err = store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "owner", Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: jsontext.Value(`null`)}})
	require.NoError(t, err)
	current, err := store.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	retained, err := db.ProjectEmbeddingProducerFromMetadata(current.Metadata)
	require.NoError(t, err)
	require.Nil(t, retained, "clearing the reserved key remains supported")
}
