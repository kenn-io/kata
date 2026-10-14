package dbtest

import (
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// RunArchivedPortableDataExport ensures live-only backups retain signed
// attribution and portable vectors for issues in an archived project.
func RunArchivedPortableDataExport(t *testing.T, store db.Storage, backend Backend) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleHub,
		HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true,
	})
	require.NoError(t, err)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: store.InstanceUID(),
		KeyID: db.RootPublicKeyID(publicKey), PublicKey: publicKey,
	}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	issue, _, err := store.CreateIssue(db.WithRootAttribution(ctx,
		db.RootAttributionSigner{AuthorityUID: pin.AuthorityUID, PrivateKey: privateKey}, "example-actor"),
		db.CreateIssueParams{ProjectID: project.ID, Title: "Portable task", Body: "Retained content", Author: "assistant"},
	)
	require.NoError(t, err)

	artifactStore, ok := store.(db.EmbeddingArtifactStorage)
	require.True(t, ok, "native portable artifact storage is required")
	identity := embedding.ArtifactIdentity{
		ProjectUID: project.UID, IssueUID: issue.UID, ProducerInstanceUID: store.InstanceUID(),
		Provider: "openai-compatible", Model: "example-model", Dimensions: 2,
		InputType: "none", Normalization: "none", Preprocessing: "kata.issue/v2",
		RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200,
		RecipeFingerprint: strings.Repeat("a", 64),
	}
	artifact, err := embedding.NewArtifact(identity, embedding.EmbedText(issue.Title, issue.Body), [][]float32{{1, 0}})
	require.NoError(t, err)
	durable, err := artifactStore.RetainEmbeddingArtifact(ctx, artifact)
	require.NoError(t, err)
	require.True(t, durable)

	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: project.ID, Actor: "example-actor", Force: true,
	})
	require.NoError(t, err)

	filter := db.ExportFilter{IncludeDeleted: false}
	records, err := CollectImportRecords(ctx, store, filter)
	require.NoError(t, err)
	attribution, ok := store.(db.AttributionStorage)
	require.True(t, ok)
	var receiptFound, entityFound bool
	for record, exportErr := range attribution.ExportAttribution(ctx, filter) {
		require.NoError(t, exportErr)
		switch value := record.(type) {
		case *db.AttributionReceipt:
			receiptFound = receiptFound || value.ProjectUID == project.UID
		case *db.EntityProvenance:
			entityFound = entityFound || (value.ProjectUID == project.UID && value.EntityUID == issue.UID)
		}
		records = append(records, record)
	}
	require.True(t, receiptFound, "archived issues retain their signed event receipt in a live-only export")
	require.True(t, entityFound, "archived issues retain their creation reference in a live-only export")

	exporter, ok := store.(db.EmbeddingArtifactExporter)
	require.True(t, ok, "native portable artifact export is required")
	var artifactFound bool
	for record, exportErr := range exporter.ExportEmbeddingArtifacts(ctx, filter) {
		require.NoError(t, exportErr)
		value, ok := record.(*db.EmbeddingArtifactExport)
		require.True(t, ok)
		artifactFound = artifactFound || (value.ProjectUID == project.UID && value.Digest == artifact.Digest)
		records = append(records, record)
	}
	require.True(t, artifactFound, "archived projects retain portable vectors in a live-only export")

	target := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportReplay(ctx, records, db.ImportOptions{}))
	targetAttribution := target.(db.AttributionStorage)
	restoredReceipt, err := targetAttribution.EntityAttribution(ctx, project.UID, "issue", issue.UID)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootReceipt(pin, restoredReceipt))
	targetArtifacts := target.(db.EmbeddingArtifactExporter)
	var restoredArtifact bool
	for record, exportErr := range targetArtifacts.ExportEmbeddingArtifacts(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, exportErr)
		value, ok := record.(*db.EmbeddingArtifactExport)
		require.True(t, ok)
		if value.ProjectUID == project.UID && value.Digest == artifact.Digest {
			require.Equal(t, artifact, value.EmbeddingArtifact)
			restoredArtifact = true
		}
	}
	require.True(t, restoredArtifact, "replay preserves portable vectors for archived projects")

	var archived bool
	for record, exportErr := range target.ExportProjects(ctx, db.ExportFilter{}) {
		require.NoError(t, exportErr)
		if record.UID == project.UID {
			archived = record.DeletedAt != nil
		}
	}
	require.True(t, archived, "backup replay preserves the archived project state")
}
