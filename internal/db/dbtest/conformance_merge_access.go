package dbtest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// RunProjectMergeAccessAndArtifacts checks that a merge keeps moved issues'
// portable vectors and never widens who can see the source's issues.
func RunProjectMergeAccessAndArtifacts(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	artifacts, ok := store.(db.EmbeddingArtifactStorage)
	require.True(t, ok, "native portable artifact storage is required")
	team, _, err := store.CreateTeam(ctx, "merge-team", "admin")
	require.NoError(t, err)
	restrict := func(project db.Project, teamUIDs ...string) {
		t.Helper()
		_, _, err := store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: project.UID, Visibility: "teams", TeamUIDs: teamUIDs}, "admin")
		require.NoError(t, err)
	}
	merge := func(source, target db.Project) error {
		_, err := store.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: source.ID, TargetProjectID: target.ID, Actor: "admin"})
		return err
	}

	source, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Portable vectors", Body: "Merge keeps vectors", Author: "assistant"})
	require.NoError(t, err)
	identity := embedding.ArtifactIdentity{ProjectUID: source.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", ModelRevision: "revision-1", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
	require.NoError(t, err)
	_, err = artifacts.RetainEmbeddingArtifact(ctx, artifact)
	require.NoError(t, err)
	restrict(source, team.UID)

	open, err := store.CreateProject(ctx, "open-target-project")
	require.NoError(t, err)
	require.ErrorIs(t, merge(source, open), db.ErrProjectMergeAccessPolicy, "a team-restricted source must not merge into an open target")
	stillThere, err := store.ProjectByID(ctx, source.ID)
	require.NoError(t, err)
	assert.Equal(t, source.UID, stillThere.UID)

	target, err := store.CreateProject(ctx, "restricted-target-project")
	require.NoError(t, err)
	restrict(target, team.UID)
	require.NoError(t, merge(source, target))
	policy, err := store.ProjectAccessPolicy(ctx, target.UID)
	require.NoError(t, err)
	assert.Equal(t, "teams", policy.Visibility)
	assert.Equal(t, []string{team.UID}, policy.TeamUIDs)
	manifests, err := artifacts.EmbeddingArtifactManifests(ctx, target.UID, 10)
	require.NoError(t, err)
	require.Len(t, manifests, 1, "moved issues keep their portable vectors")
	moved, err := artifacts.StoredEmbeddingArtifact(ctx, target.UID, manifests[0].Digest)
	require.NoError(t, err)
	wantIdentity := artifact.ArtifactIdentity
	wantIdentity.ProjectUID = target.UID
	assert.Equal(t, wantIdentity, moved.ArtifactIdentity)
	assert.Equal(t, artifact.Chunks, moved.Chunks)

	// An explicit open source only narrows when it joins a restricted target.
	openSource, err := store.CreateProject(ctx, "open-source-project")
	require.NoError(t, err)
	_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: openSource.UID, Visibility: "all"}, "admin")
	require.NoError(t, err)
	require.NoError(t, merge(openSource, target))
}

// RunProjectMergeDuplicateArtifact covers an issue that moved to the target
// before the merge. Its old artifact stays under the source UID while the
// same vectors were retained again under the target UID; the merge must keep
// one durable copy instead of failing on the duplicate.
func RunProjectMergeDuplicateArtifact(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	artifacts, ok := store.(db.EmbeddingArtifactStorage)
	require.True(t, ok, "native portable artifact storage is required")
	source, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Moved vectors", Body: "Retained twice", Author: "assistant"})
	require.NoError(t, err)
	identity := embedding.ArtifactIdentity{ProjectUID: source.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", ModelRevision: "revision-1", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
	input := embedding.EmbedText(issue.Title, issue.Body)
	original, err := embedding.NewArtifact(identity, input, [][]float32{{1, 0}})
	require.NoError(t, err)
	durable, err := artifacts.RetainEmbeddingArtifact(ctx, original)
	require.NoError(t, err)
	require.True(t, durable)

	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "mover"})
	require.NoError(t, err)
	identity.ProjectUID = target.UID
	retained, err := embedding.NewArtifact(identity, input, [][]float32{{1, 0}})
	require.NoError(t, err)
	durable, err = artifacts.RetainEmbeddingArtifact(ctx, retained)
	require.NoError(t, err)
	require.True(t, durable)

	_, err = store.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: source.ID, TargetProjectID: target.ID, Actor: "admin"})
	require.NoError(t, err)
	manifests, err := artifacts.EmbeddingArtifactManifests(ctx, target.UID, 10)
	require.NoError(t, err)
	require.Len(t, manifests, 1)
	assert.Equal(t, retained.Digest, manifests[0].Digest)
	kept, err := artifacts.StoredEmbeddingArtifact(ctx, target.UID, retained.Digest)
	require.NoError(t, err)
	assert.Equal(t, retained, kept)
}

const (
	earlierStagingExpiry = "2030-01-01T00:00:05Z"
	laterStagingExpiry   = "2030-01-01T00:00:05.5Z"
)

// RunProjectMergeArtifactRetention merges a source and target that hold the
// same artifact bytes with different retention. A durable copy must stay
// durable; two staged copies keep the chronologically later expiry. The
// expiries differ by half a second inside one RFC3339 second, where text
// order and time order disagree.
func RunProjectMergeArtifactRetention(t *testing.T, store db.Storage, backend Backend) {
	t.Helper()
	require.NotNil(t, backend.SetArtifactStagingExpiry)
	require.NotNil(t, backend.ArtifactStagingExpiry)
	ctx := t.Context()
	artifacts, ok := store.(db.EmbeddingArtifactStorage)
	require.True(t, ok, "native portable artifact storage is required")
	build := func(projectUID, issueUID string) embedding.EmbeddingArtifact {
		t.Helper()
		identity := embedding.ArtifactIdentity{ProjectUID: projectUID, IssueUID: issueUID, ProducerInstanceUID: store.InstanceUID(), Provider: "openai-compatible", Model: "example-model", ModelRevision: "revision-1", Dimensions: 2, InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
		artifact, err := embedding.NewArtifact(identity, embedding.EmbedText("Shared vectors", "Same bytes"), [][]float32{{1, 0}})
		require.NoError(t, err)
		return artifact
	}
	retain := func(projectUID, issueUID string, wantDurable bool) embedding.EmbeddingArtifact {
		t.Helper()
		artifact := build(projectUID, issueUID)
		durable, err := artifacts.RetainEmbeddingArtifact(ctx, artifact)
		require.NoError(t, err)
		require.Equal(t, wantDurable, durable)
		return artifact
	}
	stage := func(artifact embedding.EmbeddingArtifact, expiry string) {
		t.Helper()
		require.NoError(t, backend.SetArtifactStagingExpiry(ctx, store, artifact.ProjectUID, artifact.Digest, expiry))
	}
	pair := func(name string) (db.Project, db.Project) {
		t.Helper()
		source, err := store.CreateProject(ctx, name+"-source")
		require.NoError(t, err)
		target, err := store.CreateProject(ctx, name+"-target")
		require.NoError(t, err)
		return source, target
	}
	merge := func(source, target db.Project) {
		t.Helper()
		_, err := store.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: source.ID, TargetProjectID: target.ID, Actor: "admin"})
		require.NoError(t, err)
	}
	assertDurable := func(target db.Project, artifact embedding.EmbeddingArtifact) {
		t.Helper()
		kept, err := artifacts.StoredEmbeddingArtifact(ctx, target.UID, artifact.Digest)
		require.NoError(t, err, "a durable copy must stay durable after the merge")
		assert.Equal(t, artifact, kept)
		expiry, err := backend.ArtifactStagingExpiry(ctx, store, target.UID, artifact.Digest)
		require.NoError(t, err)
		assert.Nil(t, expiry)
	}
	assertStaged := func(target db.Project, artifact embedding.EmbeddingArtifact, want string) {
		t.Helper()
		_, err := artifacts.StoredEmbeddingArtifact(ctx, target.UID, artifact.Digest)
		require.ErrorIs(t, err, db.ErrNotFound, "two staged copies must not become durable")
		expiry, err := backend.ArtifactStagingExpiry(ctx, store, target.UID, artifact.Digest)
		require.NoError(t, err)
		require.NotNil(t, expiry)
		assert.Equal(t, want, *expiry)
	}

	// Durable source copy, staged target copy for an issue still in the source.
	source, target := pair("durable-source")
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Source issue", Author: "assistant"})
	require.NoError(t, err)
	retain(source.UID, issue.UID, true)
	stage(retain(target.UID, issue.UID, false), laterStagingExpiry)
	merge(source, target)
	assertDurable(target, build(target.UID, issue.UID))

	// Staged source copy, durable target copy for an issue already in the target.
	source, target = pair("durable-target")
	issue, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: target.ID, Title: "Target issue", Author: "assistant"})
	require.NoError(t, err)
	stage(retain(source.UID, issue.UID, false), laterStagingExpiry)
	durableTarget := retain(target.UID, issue.UID, true)
	merge(source, target)
	assertDurable(target, durableTarget)

	// Two staged copies for an issue that has not arrived in either project.
	for _, laterInSource := range []bool{true, false} {
		source, target = pair(map[bool]string{true: "later-source", false: "later-target"}[laterInSource])
		pending := map[bool]string{true: "01J0000000000000000000B001", false: "01J0000000000000000000B002"}[laterInSource]
		sourceExpiry, targetExpiry := laterStagingExpiry, earlierStagingExpiry
		if !laterInSource {
			sourceExpiry, targetExpiry = earlierStagingExpiry, laterStagingExpiry
		}
		stage(retain(source.UID, pending, false), sourceExpiry)
		staged := retain(target.UID, pending, false)
		stage(staged, targetExpiry)
		merge(source, target)
		assertStaged(target, staged, laterStagingExpiry)
	}
}
