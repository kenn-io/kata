package vector

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	kitvec "go.kenn.io/kit/vector"
)

// ImportArtifact reuses a complete current-input artifact under the existing
// reconciler lease and backend revision fence. Portable bytes stay canonical;
// SaveVectors performs the backend-specific index conversion.
func (ix *Index) ImportArtifact(ctx context.Context, key string, artifact embedding.EmbeddingArtifact, expected embedding.ArtifactIdentity, revision any) error {
	if !db.ProjectAttributionVisible(ctx, artifact.ProjectUID) {
		return db.ErrNotFound
	}
	received := artifact.ArtifactIdentity
	received.InputHash, received.ProducerInstanceUID = "", ""
	expected.InputHash, expected.ProducerInstanceUID = "", ""
	if received != expected {
		return fmt.Errorf("%w: incompatible artifact identity", db.ErrFederationIngestValidation)
	}
	var input, projectUID string
	var err error
	if ix.pg != nil {
		executor, leaseErr := ix.pg.reconcilerExecutor()
		if leaseErr != nil {
			return leaseErr
		}
		err = executor.QueryRowContext(ctx, `SELECT content,project_uid FROM issue_vector_mirror WHERE issue_uid=$1`, artifact.IssueUID).Scan(&input, &projectUID)
	} else {
		err = ix.db.QueryRowContext(ctx, `SELECT content,project_uid FROM issue_mirror WHERE issue_uid=?`, artifact.IssueUID).Scan(&input, &projectUID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return db.ErrNotFound
	}
	if err != nil {
		return err
	}
	if projectUID != artifact.ProjectUID {
		return db.ErrNotFound
	}
	if err := embedding.ValidateArtifact(artifact, input, expected.RecipeFingerprint); err != nil {
		return fmt.Errorf("%w: %v", db.ErrFederationIngestValidation, err)
	}
	vectors := make([]kitvec.ChunkVector, len(artifact.Chunks))
	for i, chunk := range artifact.Chunks {
		vector := make(kitvec.Vector, artifact.Dimensions)
		for j := range vector {
			vector[j] = math.Float32frombits(binary.LittleEndian.Uint32(chunk.VectorBytes[j*4:]))
		}
		vectors[i] = kitvec.ChunkVector{ChunkIndex: chunk.Index, Vector: vector}
	}
	return ix.flowStore.SaveVectors(ctx, key, artifact.IssueUID, revision, vectors)
}

// maxRejectedArtifacts bounds the process-local record of rejected digests.
// Further rejections are still skipped and logged.
const maxRejectedArtifacts = 1024

// importReusable imports one candidate artifact. An artifact that fails
// validation is skipped, logged and recorded for status, so it cannot stop
// reuse for other documents; its document stays pending for local fill.
func (ix *Index) importReusable(ctx context.Context, key string, artifact embedding.EmbeddingArtifact, expected embedding.ArtifactIdentity, revision any) (bool, error) {
	err := ix.ImportArtifact(ctx, key, artifact, expected, revision)
	if errors.Is(err, db.ErrFederationIngestValidation) {
		ix.rejectArtifact(artifact, err)
		return false, nil
	}
	return err == nil, err
}

// rejectArtifact records a retained artifact that failed validation so status
// can name it; the document stays pending for local fill.
func (ix *Index) rejectArtifact(artifact embedding.EmbeddingArtifact, err error) {
	slog.Warn("skipping invalid embedding artifact", "project_uid", artifact.ProjectUID, "issue_uid", artifact.IssueUID, "digest", artifact.Digest, "err", err)
	ix.rejectedMu.Lock()
	defer ix.rejectedMu.Unlock()
	if ix.rejected == nil {
		ix.rejected = make(map[string]bool)
	}
	if len(ix.rejected) < maxRejectedArtifacts {
		ix.rejected[artifact.Digest] = true
	}
}

// ArtifactRejected reports whether this process skipped the retained artifact
// because it failed validation against the current input and recipe.
func (ix *Index) ArtifactRejected(digest string) bool {
	if ix == nil {
		return false
	}
	ix.rejectedMu.Lock()
	defer ix.rejectedMu.Unlock()
	return ix.rejected[digest]
}

// ReconcileArtifacts reuses one bounded pending page before local fill. The
// fill worker rechecks subsequent pages immediately before dispatch. Already
// indexed documents cause no portable-byte reads or vector-index rewrites.
func (ix *Index) ReconcileArtifacts(ctx context.Context, key string, source db.Storage, recipe embedding.ArtifactIdentity) (int, error) {
	exporter, ok := source.(db.EmbeddingArtifactExporter)
	if !ok {
		return 0, nil
	}
	docs, err := ix.flowStore.PendingForGeneration(ctx, key, 256)
	if err != nil || len(docs) == 0 {
		return 0, err
	}
	pending := make(map[string]kitvec.Pending[string], len(docs))
	issueUIDs := make([]string, len(docs))
	for i, doc := range docs {
		pending[doc.Doc] = doc
		issueUIDs[i] = doc.Doc
	}
	reused := 0
	for record, err := range exporter.ExportEmbeddingArtifacts(ctx, db.ExportFilter{}, issueUIDs...) {
		if err != nil {
			return reused, err
		}
		portable, ok := record.(*db.EmbeddingArtifactExport)
		if !ok {
			return reused, db.ErrFederationIngestValidation
		}
		artifact := portable.EmbeddingArtifact
		doc, ok := pending[artifact.IssueUID]
		if !ok || embedding.ArtifactInputHash(doc.Content) != artifact.InputHash {
			continue
		}
		expected := recipe
		expected.ProjectUID, expected.IssueUID = artifact.ProjectUID, artifact.IssueUID
		received := artifact.ArtifactIdentity
		received.InputHash, received.ProducerInstanceUID = "", ""
		comparison := expected
		comparison.InputHash, comparison.ProducerInstanceUID = "", ""
		if received != comparison {
			continue
		}
		imported, err := ix.reuseArtifact(ctx, key, source, artifact, expected, doc.Revision)
		if errors.Is(err, kitvec.ErrStale) || (err == nil && !imported) {
			continue
		}
		if err != nil {
			return reused, err
		}
		delete(pending, artifact.IssueUID)
		reused++
	}
	return reused, nil
}

// reuseArtifact imports retained bytes for a pending document. Reused bytes
// are current content again, so peers that pruned them after an intervening
// edit need a fresh offer; retention queues one only when a newer content
// event made the bytes current. Retention runs before the import stamps the
// document, so a failed re-offer leaves the document pending and is retried.
func (ix *Index) reuseArtifact(ctx context.Context, key string, source db.Storage, artifact embedding.EmbeddingArtifact, expected embedding.ArtifactIdentity, revision any) (bool, error) {
	if retainer, ok := source.(db.EmbeddingArtifactStorage); ok {
		_, err := retainer.RetainEmbeddingArtifact(ctx, artifact)
		if errors.Is(err, db.ErrFederationIngestValidation) {
			// Retention validates the bytes against the current input, like
			// the import would; an invalid artifact is skipped, not fatal.
			ix.rejectArtifact(artifact, err)
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("reoffer reused embedding artifact %s: %w", artifact.Digest, err)
		}
	}
	return ix.importReusable(ctx, key, artifact, expected, revision)
}
