package vector

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
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
		if err := ix.ImportArtifact(ctx, key, artifact, expected, doc.Revision); err != nil {
			if errors.Is(err, kitvec.ErrStale) {
				continue
			}
			return reused, err
		}
		delete(pending, artifact.IssueUID)
		reused++
	}
	return reused, nil
}
