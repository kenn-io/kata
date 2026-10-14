package dbtest

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

type embeddingArtifactStore = db.EmbeddingArtifactStorage

// RunEmbeddingArtifactStorage exercises embedding artifact storage on the supplied native store.
// R7: only complete validated durable portable bytes establish an exact hit.
// Index compatibility and another digest for the same input never replace it.
func RunEmbeddingArtifactStorage(t *testing.T, store db.Storage) {
	ctx := t.Context()
	artifacts, ok := store.(embeddingArtifactStore)
	require.True(t, ok, "native portable artifact storage is required")
	project, err := store.CreateProject(ctx, "artifact-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Portable vectors", Body: strings.Repeat("界", 2100), Author: "assistant"})
	require.NoError(t, err)
	input := embedding.EmbedText(issue.Title, issue.Body)
	identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", ModelRevision: "revision-1", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	artifact, err := embedding.NewArtifact(identity, input, [][]float32{{1, 0}, {0, 1}})
	require.NoError(t, err)
	_, err = artifacts.StoredEmbeddingArtifact(ctx, project.UID, artifact.Digest)
	require.ErrorIs(t, err, db.ErrNotFound)
	durable, err := artifacts.RetainEmbeddingArtifact(ctx, artifact)
	require.NoError(t, err)
	require.True(t, durable)
	retained, err := artifacts.StoredEmbeddingArtifact(ctx, project.UID, artifact.Digest)
	require.NoError(t, err)
	require.Equal(t, artifact, retained)
	durable, err = artifacts.RetainEmbeddingArtifact(ctx, artifact)
	require.NoError(t, err)
	require.True(t, durable, "lost reply retries find the same committed bytes")
	manifests, err := artifacts.EmbeddingArtifactManifests(ctx, project.UID, 10)
	require.NoError(t, err)
	require.Equal(t, []embedding.ArtifactManifest{artifact.Manifest()}, manifests)
	raw, err := json.Marshal(manifests)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "vector_bytes\"")
	alternate, err := embedding.NewArtifact(identity, input, [][]float32{{0, 1}, {1, 0}})
	require.NoError(t, err)
	require.NotEqual(t, artifact.Digest, alternate.Digest)
	_, err = artifacts.StoredEmbeddingArtifact(ctx, project.UID, alternate.Digest)
	require.ErrorIs(t, err, db.ErrNotFound, "same compatible recipe/input with different exact bytes is a miss")
	bad := artifact
	bad.Chunks = bad.Chunks[:1]
	_, err = artifacts.RetainEmbeddingArtifact(ctx, bad)
	require.Error(t, err)
	hidden := db.WithAuthorizedProjects(ctx, []string{})
	_, err = artifacts.StoredEmbeddingArtifact(hidden, project.UID, artifact.Digest)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = artifacts.EmbeddingArtifactManifests(hidden, project.UID, 10)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = artifacts.RetainEmbeddingArtifact(hidden, artifact)
	require.ErrorIs(t, err, db.ErrNotFound)
	// Valid vectors above PostgreSQL's halfvec dimension limit remain portable.
	identity.Dimensions = 4001
	vectors := [][]float32{make([]float32, 4001), make([]float32, 4001)}
	large, err := embedding.NewArtifact(identity, input, vectors)
	require.NoError(t, err)
	durable, err = artifacts.RetainEmbeddingArtifact(ctx, large)
	require.NoError(t, err)
	require.True(t, durable)
	retained, err = artifacts.StoredEmbeddingArtifact(ctx, project.UID, large.Digest)
	require.NoError(t, err)
	require.Equal(t, large, retained)

	// Project merge rekeys retained bytes without regenerating stale vectors.
	source, err := store.CreateProject(ctx, "artifact-merge-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "artifact-merge-target")
	require.NoError(t, err)
	mergeIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Before merge", Author: "member"})
	require.NoError(t, err)
	mergeIdentity := embedding.ArtifactIdentity{ProjectUID: source.UID, IssueUID: mergeIssue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("b", 64)}
	stale, err := embedding.NewArtifact(mergeIdentity, embedding.EmbedText(mergeIssue.Title, mergeIssue.Body), [][]float32{{0.25, 0.75}})
	require.NoError(t, err)
	durable, err = artifacts.RetainEmbeddingArtifact(ctx, stale)
	require.NoError(t, err)
	require.True(t, durable)
	changedTitle := "After merge"
	_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: mergeIssue.ID, Title: &changedTitle, Actor: "member"})
	require.NoError(t, err)
	rebound, err := embedding.RebindArtifactProject(stale, target.UID)
	require.NoError(t, err)
	require.NotEqual(t, stale.Digest, rebound.Digest, "project identity is part of the artifact digest")
	_, err = store.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: source.ID, TargetProjectID: target.ID, Actor: "member"})
	require.NoError(t, err)
	retained, err = artifacts.StoredEmbeddingArtifact(ctx, target.UID, rebound.Digest)
	require.NoError(t, err, "merge must preserve even stale original portable vectors")
	require.Equal(t, rebound, retained)
	require.Equal(t, stale.Chunks[0].VectorBytes, retained.Chunks[0].VectorBytes, "rekeying keeps the original float32 bytes")
	manifests, err = artifacts.EmbeddingArtifactManifests(ctx, target.UID, 10)
	require.NoError(t, err)
	require.Equal(t, []embedding.ArtifactManifest{rebound.Manifest()}, manifests)
}

// RunArtifactIssuePurgeAfterMove verifies a move rehomes portable bytes and
// purge removes the rehomed artifact with the issue.
func RunArtifactIssuePurgeAfterMove(t *testing.T, store db.Storage) {
	ctx := t.Context()
	artifacts := store.(embeddingArtifactStore)
	// Purge follows an issue across its project move to remove old namespaces.
	moveSource, err := store.CreateProject(ctx, "artifact-move-source")
	require.NoError(t, err)
	moveTarget, err := store.CreateProject(ctx, "artifact-move-target")
	require.NoError(t, err)
	movingIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: moveSource.ID, Title: "Portable vectors to purge", Author: "member"})
	require.NoError(t, err)
	moveIdentity := embedding.ArtifactIdentity{ProjectUID: moveSource.UID, IssueUID: movingIssue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("c", 64)}
	movedArtifact, err := embedding.NewArtifact(moveIdentity, embedding.EmbedText(movingIssue.Title, movingIssue.Body), [][]float32{{0.5, 0.5}})
	require.NoError(t, err)
	durable, err := artifacts.RetainEmbeddingArtifact(ctx, movedArtifact)
	require.NoError(t, err)
	require.True(t, durable)
	moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: movingIssue.ID, FromProjectID: moveSource.ID, ToProjectID: moveTarget.ID, IfMatchRev: movingIssue.Revision, Actor: "member"})
	require.NoError(t, err)
	require.Equal(t, moveTarget.ID, moved.Issue.ProjectID)
	_, err = artifacts.StoredEmbeddingArtifact(ctx, moveSource.UID, movedArtifact.Digest)
	require.ErrorIs(t, err, db.ErrNotFound, "moved artifacts no longer remain under the source project")
	rebound, err := embedding.RebindArtifactProject(movedArtifact, moveTarget.UID)
	require.NoError(t, err)
	retained, err := artifacts.StoredEmbeddingArtifact(ctx, moveTarget.UID, rebound.Digest)
	require.NoError(t, err, "the destination project owns the rekeyed artifact after the move")
	require.Equal(t, rebound, retained)
	require.Equal(t, movedArtifact.Chunks[0].VectorBytes, retained.Chunks[0].VectorBytes)
	_, err = store.PurgeIssue(ctx, movingIssue.ID, "member", nil)
	require.NoError(t, err)
	_, err = artifacts.StoredEmbeddingArtifact(ctx, moveSource.UID, movedArtifact.Digest)
	require.ErrorIs(t, err, db.ErrNotFound, "purge removes artifacts by issue UID across project namespaces")
	_, err = artifacts.StoredEmbeddingArtifact(ctx, moveTarget.UID, rebound.Digest)
	require.ErrorIs(t, err, db.ErrNotFound, "purge removes rehomed artifacts under the issue's new project UID")
}

// RunArtifactIssueMovePreservesPortableBytes verifies issue moves rehome the
// exact retained vectors into the destination namespace. Backups enumerate
// artifacts by the issue's current project, so leaving them under the source
// UID would silently omit them.
func RunArtifactIssueMovePreservesPortableBytes(t *testing.T, store db.Storage) {
	ctx := t.Context()
	artifacts := store.(embeddingArtifactStore)
	source, err := store.CreateProject(ctx, "artifact-move-preserve-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "artifact-move-preserve-target")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: source.ID, Title: "Original vectors", Body: "move without regeneration", Author: "member",
	})
	require.NoError(t, err)
	identity := embedding.ArtifactIdentity{
		ProjectUID: source.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(),
		Provider: "openai-compatible", Model: "example-model", Dimensions: 2,
		InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2",
		RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200,
		RecipeFingerprint: strings.Repeat("d", 64),
	}
	original, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{0.25, 0.75}})
	require.NoError(t, err)
	durable, err := artifacts.RetainEmbeddingArtifact(ctx, original)
	require.NoError(t, err)
	require.True(t, durable)

	moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID,
		IfMatchRev: issue.Revision, Actor: "member",
	})
	require.NoError(t, err)
	require.Equal(t, target.ID, moved.Issue.ProjectID)
	rebound, err := embedding.RebindArtifactProject(original, target.UID)
	require.NoError(t, err)
	retained, err := artifacts.StoredEmbeddingArtifact(ctx, target.UID, rebound.Digest)
	require.NoError(t, err, "the target project must retain a portable artifact after the issue move")
	require.Equal(t, rebound, retained)
	require.Equal(t, original.Chunks[0].VectorBytes, retained.Chunks[0].VectorBytes,
		"moving an issue preserves the original float32 vector bytes")
	_, err = artifacts.StoredEmbeddingArtifact(ctx, source.UID, original.Digest)
	require.ErrorIs(t, err, db.ErrNotFound, "the source namespace no longer owns moved issue artifacts")

	exporter, ok := store.(db.EmbeddingArtifactExporter)
	require.True(t, ok, "native portable artifact export is required")
	var exported []db.EmbeddingArtifactExport
	for record, exportErr := range exporter.ExportEmbeddingArtifacts(ctx, db.ExportFilter{ProjectID: &target.ID}) {
		require.NoError(t, exportErr)
		artifactRecord, ok := record.(*db.EmbeddingArtifactExport)
		require.True(t, ok, "artifact export returns the portable artifact record")
		exported = append(exported, *artifactRecord)
	}
	require.Len(t, exported, 1, "the target backup includes the moved artifact")
	require.Equal(t, target.UID, exported[0].ProjectUID)
	require.Equal(t, rebound.Digest, exported[0].Digest)
}

// RunArtifactStagingExpiryRetry exercises artifact staging expiry retry on the supplied native store.
// R7: pre-content bytes are bounded expiring staging, never an exact durable
// hit. Restart/expiry must preserve retry identity without blocking content.
func RunArtifactStagingExpiryRetry(t *testing.T, store db.Storage, restart func() db.Storage, expire func(context.Context, string)) {
	ctx := t.Context()
	artifacts := store.(embeddingArtifactStore)
	project, err := store.CreateProject(ctx, "staging-project")
	require.NoError(t, err)
	input := embedding.EmbedText("Future task", "Portable content")
	identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: "00000000000000000000000050", ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	artifact, err := embedding.NewArtifact(identity, input, [][]float32{{1, 0}})
	require.NoError(t, err)
	durable, err := artifacts.RetainEmbeddingArtifact(ctx, artifact)
	require.NoError(t, err)
	require.False(t, durable)
	store = restart()
	artifacts = store.(embeddingArtifactStore)
	_, err = artifacts.StoredEmbeddingArtifact(ctx, project.UID, artifact.Digest)
	require.ErrorIs(t, err, db.ErrNotFound, "staging cannot establish have even after restart")
	manifests, err := artifacts.EmbeddingArtifactManifests(ctx, project.UID, 32)
	require.NoError(t, err)
	require.Empty(t, manifests)
	durable, err = artifacts.RetainEmbeddingArtifact(ctx, artifact)
	require.NoError(t, err)
	require.False(t, durable, "same pre-content retry remains unaccepted")
	expire(ctx, project.UID)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, UID: identity.IssueUID, Title: "Future task", Body: "Portable content", Author: "assistant"})
	require.NoError(t, err, "issue delivery remains independent of staging")
	durable, err = artifacts.RetainEmbeddingArtifact(ctx, artifact)
	require.NoError(t, err)
	require.True(t, durable, "expired staging is recovered by the exact original bytes")
	retained, err := artifacts.StoredEmbeddingArtifact(ctx, project.UID, artifact.Digest)
	require.NoError(t, err)
	require.Equal(t, artifact, retained)
	// 32 separate pre-content artifacts fit; a 33rd is explicitly retryable.
	for i := range 33 {
		identity.IssueUID = fmt.Sprintf("%026d", 100+i)
		next, err := embedding.NewArtifact(identity, input, [][]float32{{1, 0}})
		require.NoError(t, err)
		durable, err = artifacts.RetainEmbeddingArtifact(ctx, next)
		require.False(t, durable)
		if i == 32 {
			require.ErrorIs(t, err, db.ErrEmbeddingArtifactMiss, "staging overflow is a retryable miss")
			break
		}
		require.NoError(t, err)
	}
	expire(ctx, project.UID)
	identity.IssueUID = "00000000000000000000000132"
	retry, err := embedding.NewArtifact(identity, input, [][]float32{{1, 0}})
	require.NoError(t, err)
	durable, err = artifacts.RetainEmbeddingArtifact(ctx, retry)
	require.NoError(t, err, "expiry frees capacity for identical overflow retry")
	require.False(t, durable)
}

// RunArtifactStagingByteBudget exercises artifact staging byte budget on the supplied native store.
// R7's staging byte bound covers retained portable data, including base64 and
// manifests, so two 16-MiB vector sets cannot exceed a 32-MiB staging area.
func RunArtifactStagingByteBudget(t *testing.T, store db.Storage) {
	ctx := t.Context()
	artifacts := store.(db.EmbeddingArtifactStorage)
	project, err := store.CreateProject(ctx, "byte-budget-project")
	require.NoError(t, err)
	identity := embedding.ArtifactIdentity{ProjectUID: project.UID, IssueUID: "00000000000000000000000050", ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", Dimensions: 32768, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	input := strings.Repeat("x", 2000+127*1800)
	values := make([]float32, identity.Dimensions)
	vectors := make([][]float32, 128)
	for i := range vectors {
		vectors[i] = values
	}
	for i := range 2 {
		identity.IssueUID = fmt.Sprintf("%026d", 50+i)
		artifact, err := embedding.NewArtifact(identity, input, vectors)
		require.NoError(t, err)
		require.Equal(t, int64(16<<20), artifact.Manifest().VectorByteSize)
		raw, err := json.Marshal(artifact)
		require.NoError(t, err)
		require.Greater(t, len(raw), 16<<20, "portable JSON includes vector encoding overhead")
		durable, err := artifacts.RetainEmbeddingArtifact(ctx, artifact)
		require.False(t, durable)
		if i == 0 {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, db.ErrEmbeddingArtifactMiss, "staging cannot retain over 32 MiB of portable data")
		}
	}
}
