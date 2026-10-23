package vector

import (
	"context"
	"errors"
	"log/slog"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// RecaptureArtifacts rebuilds missing portable artifacts from vectors already
// covered by key. It is deliberately separate from Fill: old documents do
// not become pending merely because artifact retention was added, and reading
// the current index avoids a second paid provider call.
//
// docs must all belong to projectUID and should be a bounded page. Documents
// that are not currently covered, have no vector chunks, or already have a
// matching current-input artifact are ignored. The caller pages the project
// so this method never scans an entire hub in one pass.
func (ix *Index) RecaptureArtifacts(ctx context.Context, key string, source db.Storage, recipe embedding.ArtifactIdentity, projectUID string, docs []string) (int, error) {
	retainer, ok := source.(db.EmbeddingArtifactStorage)
	if !ok || len(docs) == 0 {
		return 0, nil
	}
	exporter, ok := source.(db.EmbeddingArtifactExporter)
	if !ok {
		return 0, nil
	}
	issues, err := source.IssuesByUIDs(ctx, docs, db.IncludeDeletedNo)
	if err != nil {
		return 0, err
	}
	stored, err := ix.LookupVectors(ctx, key, projectUID, docs)
	if err != nil {
		return 0, err
	}
	existing := make(map[string]bool, len(docs))
	for record, exportErr := range exporter.ExportEmbeddingArtifacts(ctx, db.ExportFilter{}, docs...) {
		if exportErr != nil {
			return 0, exportErr
		}
		portable, ok := record.(*db.EmbeddingArtifactExport)
		if !ok {
			return 0, db.ErrFederationIngestValidation
		}
		artifact := portable.EmbeddingArtifact
		issue, present := issues[artifact.IssueUID]
		if !present || artifact.ProjectUID != projectUID || artifact.RecipeFingerprint != recipe.RecipeFingerprint {
			continue
		}
		if artifact.InputHash == embedding.ArtifactInputHash(embedding.EmbedText(issue.Title, issue.Body)) {
			existing[artifact.IssueUID] = true
		}
	}
	recaptured := 0
	for _, issueUID := range docs {
		if existing[issueUID] {
			continue
		}
		issue, present := issues[issueUID]
		if !present {
			continue
		}
		sv, covered := stored[issueUID]
		if !covered || len(sv.Chunks) == 0 {
			continue
		}
		input := embedding.EmbedText(issue.Title, issue.Body)
		if sv.ContentSHA256 != ContentSHA256(input) {
			// The mirror and canonical store moved between the two reads. The
			// next pass will retry with a fresh content/revision pair.
			slog.Debug("skipping recapture for changed issue", "project_uid", projectUID, "issue_uid", issueUID)
			continue
		}
		vectors := make([][]float32, len(sv.Chunks))
		valid := true
		for i, chunk := range sv.Chunks {
			if chunk.ChunkIndex != i {
				valid = false
				break
			}
			vectors[i] = append([]float32(nil), chunk.Vector...)
		}
		if !valid {
			slog.Warn("skipping recapture for invalid stored chunks", "project_uid", projectUID, "issue_uid", issueUID)
			continue
		}
		identity := recipe
		identity.ProjectUID, identity.IssueUID, identity.InputHash = projectUID, issueUID, ""
		artifact, err := embedding.NewArtifact(identity, input, vectors)
		if err != nil {
			slog.Warn("skipping recapture for incompatible stored vectors", "project_uid", projectUID, "issue_uid", issueUID, "err", err)
			continue
		}
		durable, err := retainer.RetainEmbeddingArtifact(ctx, artifact)
		if err != nil {
			if errors.Is(err, db.ErrEmbeddingArtifactMiss) || errors.Is(err, db.ErrNotFound) || errors.Is(err, db.ErrFederationIngestValidation) {
				// The issue may have changed between the mirror read and
				// retention. Leave it for a later pass without starving the
				// rest of this bounded page.
				continue
			}
			return recaptured, err
		}
		if durable {
			recaptured++
		}
	}
	return recaptured, nil
}
