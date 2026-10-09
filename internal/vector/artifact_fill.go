package vector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	kitvec "go.kenn.io/kit/vector"
)

// FillWithArtifacts uses the existing kit fill flow and local serialization.
// Canonical artifacts are retained before backend-specific vector conversion.
func (ix *Index) FillWithArtifacts(ctx context.Context, key string, source db.Storage, recipe embedding.ArtifactIdentity, enc kitvec.EncodeFunc, scanBatch int, batchOptions []kitvec.BatchOption, onDocument func(bool), eligibility ...func(context.Context, string) (bool, error)) (kitvec.FillStats, error) {
	stats, _, err := ix.FillWithArtifactsDetailed(ctx, key, source, recipe, enc, scanBatch, batchOptions, onDocument, eligibility...)
	return stats, err
}

// FillWithArtifactsDetailed reports documents still pending only because the
// caller denied generation eligibility. They remain unstamped and retryable,
// but do not block cutover once all eligible work is complete.
func (ix *Index) FillWithArtifactsDetailed(ctx context.Context, key string, source db.Storage, recipe embedding.ArtifactIdentity, enc kitvec.EncodeFunc, scanBatch int, batchOptions []kitvec.BatchOption, onDocument func(bool), eligibility ...func(context.Context, string) (bool, error)) (kitvec.FillStats, int, error) {
	artifacts, ok := source.(db.EmbeddingArtifactStorage)
	if !ok {
		stats, err := ix.Fill(ctx, key, enc, scanBatch, batchOptions, onDocument)
		return stats, 0, err
	}
	exporter, _ := source.(db.EmbeddingArtifactExporter)
	backing := &artifactCaptureStore{Store: ix.flowStore, index: ix, source: source, artifacts: artifacts, exporter: exporter, recipe: recipe, generation: key, pending: map[string]artifactPending{}, ineligible: map[string]struct{}{}}
	if len(eligibility) > 0 {
		backing.eligible = eligibility[0]
	}
	stats, err := fill(ctx, backing, key, backing.beforeDispatch(enc), scanBatch, batchOptions, onDocument, backing.prepare)
	deferred, deferredErr := backing.ineligiblePendingCount(ctx, key)
	if err == nil {
		err = deferredErr
	}
	return stats, deferred, err
}

type artifactPending struct {
	kitvec.Pending[string]
	projectUID string
}

type artifactCaptureStore struct {
	kitvec.Store[string, string]
	index       *Index
	source      db.Storage
	artifacts   db.EmbeddingArtifactStorage
	exporter    db.EmbeddingArtifactExporter
	recipe      embedding.ArtifactIdentity
	generation  string
	pending     map[string]artifactPending
	prepared    map[string]kitvec.Pending[string]
	eligible    func(context.Context, string) (bool, error)
	deferred    map[string]bool
	ineligible  map[string]struct{}
	dispatchMu  sync.Mutex
	dispatchErr error
}

func (s *artifactCaptureStore) ineligiblePendingCount(ctx context.Context, key string) (int, error) {
	if len(s.ineligible) == 0 {
		return 0, nil
	}
	backlog, err := s.index.Backlog(ctx, key)
	if err != nil || backlog == 0 {
		return 0, err
	}
	limit := int(backlog)
	if int64(limit) < backlog {
		limit = int(^uint(0) >> 1)
	}
	pending, err := s.Store.PendingForGeneration(ctx, key, limit)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, doc := range pending {
		if _, ok := s.ineligible[doc.Doc]; ok {
			count++
		}
	}
	return count, nil
}

// Kit prepares an entire page before encoding its chunk batches. A later
// preparation can observe revocation or receive artifacts for an earlier one.
// Recheck the outstanding page at the actual encoder boundary. Changed pages
// remain unstamped for the existing worker's next normal retry.
func (s *artifactCaptureStore) beforeDispatch(enc kitvec.EncodeFunc) kitvec.EncodeFunc {
	if enc == nil {
		return nil
	}
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		err := func() (err error) {
			// Batch options may permit parallel encoder calls. Serialize these
			// checks and their artifact reconciliation, preserving HTTP parallelism.
			s.dispatchMu.Lock()
			defer func() {
				if err != nil {
					s.dispatchErr = err
				}
				s.dispatchMu.Unlock()
			}()
			if s.dispatchErr != nil {
				return s.dispatchErr
			}
			for _, doc := range s.prepared {
				ready, err := s.checkPending(ctx, doc)
				if err != nil {
					return err
				}
				if !ready {
					return kitvec.ErrStale
				}
			}
			return nil
		}()
		if err != nil {
			return nil, err
		}
		return enc(ctx, texts)
	}
}

// Recheck portable bytes after the pending read, immediately before handing
// unresolved documents to the existing fill flow. Imported documents disappear
// from the native pending scan; continue scanning when a whole page was reused.
func (s *artifactCaptureStore) PendingForGeneration(ctx context.Context, key string, limit int) ([]kitvec.Pending[string], error) {
	for {
		pending, err := s.scanPending(ctx, key, limit)
		if err != nil || len(pending) == 0 || s.exporter == nil {
			return pending, err
		}
		issueUIDs := make([]string, 0, len(pending))
		for _, doc := range pending {
			issueUIDs = append(issueUIDs, doc.Doc)
		}
		if err := s.reuseArtifacts(ctx, key, issueUIDs...); err != nil {
			return nil, err
		}
		remaining := pending[:0]
		for _, doc := range pending {
			if _, ok := s.pending[doc.Doc]; ok {
				remaining = append(remaining, doc)
			}
		}
		if len(remaining) > 0 {
			return remaining, nil
		}
	}
}

func (s *artifactCaptureStore) reuseArtifacts(ctx context.Context, key string, issueUIDs ...string) error {
	if s.exporter == nil {
		return nil
	}
	for record, err := range s.exporter.ExportEmbeddingArtifacts(ctx, db.ExportFilter{}, issueUIDs...) {
		if err != nil {
			return err
		}
		portable, ok := record.(*db.EmbeddingArtifactExport)
		if !ok {
			return db.ErrFederationIngestValidation
		}
		artifact := portable.EmbeddingArtifact
		scanned, ok := s.pending[artifact.IssueUID]
		if !ok || scanned.projectUID != artifact.ProjectUID || embedding.ArtifactInputHash(scanned.Content) != artifact.InputHash {
			continue
		}
		expected := s.recipe
		expected.ProjectUID, expected.IssueUID = artifact.ProjectUID, artifact.IssueUID
		received := artifact.ArtifactIdentity
		received.InputHash, received.ProducerInstanceUID = "", ""
		comparison := expected
		comparison.InputHash, comparison.ProducerInstanceUID = "", ""
		if received != comparison {
			continue
		}
		if err := s.index.ImportArtifact(ctx, key, artifact, expected, scanned.Revision); err != nil {
			return err
		}
		delete(s.pending, artifact.IssueUID)
	}
	return nil
}

func (s *artifactCaptureStore) scanPending(ctx context.Context, key string, limit int) ([]kitvec.Pending[string], error) {
	pending, err := s.Store.PendingForGeneration(ctx, key, limit)
	if err != nil {
		return nil, err
	}
	s.pending = make(map[string]artifactPending, len(pending))
	s.prepared = make(map[string]kitvec.Pending[string], len(pending))
	s.deferred = make(map[string]bool)
	for _, doc := range pending {
		var projectUID string
		if s.index.pg != nil {
			executor, leaseErr := s.index.pg.reconcilerExecutor()
			if leaseErr != nil {
				return nil, leaseErr
			}
			err = executor.QueryRowContext(ctx, `SELECT project_uid FROM issue_vector_mirror WHERE issue_uid=$1`, doc.Doc).Scan(&projectUID)
		} else {
			err = s.index.db.QueryRowContext(ctx, `SELECT project_uid FROM issue_mirror WHERE issue_uid=?`, doc.Doc).Scan(&projectUID)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, kitvec.ErrStale
		}
		if err != nil {
			return nil, err
		}
		s.pending[doc.Doc] = artifactPending{Pending: doc, projectUID: projectUID}
	}
	return pending, nil
}

// The existing fill flow treats these unstamped documents like a changed
// eligibility snapshot, so later private documents are still scanned this run.
func (s *artifactCaptureStore) checkPending(ctx context.Context, doc kitvec.Pending[string]) (bool, error) {
	scanned, ok := s.pending[doc.Doc]
	if !ok {
		return false, kitvec.ErrStale
	}
	if s.eligible != nil {
		allowed, err := s.eligible(ctx, scanned.projectUID)
		if err != nil {
			return false, err
		}
		if !allowed {
			s.deferred[doc.Doc] = true
			s.ineligible[doc.Doc] = struct{}{}
			return false, nil
		}
	}
	// Eligibility may pull new content after the mirror scan. Narrow the native
	// read to the admitted project and defer stale input before paid dispatch.
	if !db.ProjectAttributionVisible(ctx, scanned.projectUID) {
		s.deferred[doc.Doc] = true
		return false, nil
	}
	scope := db.WithAuthorizedProjects(ctx, []string{scanned.projectUID})
	project, err := s.source.ProjectByUID(scope, scanned.projectUID)
	if errors.Is(err, db.ErrNotFound) || (err == nil && project.DeletedAt != nil) {
		s.deferred[doc.Doc] = true
		return false, nil
	}
	if err != nil {
		return false, err
	}
	current, err := s.source.IssueByUID(scope, doc.Doc, db.IncludeDeletedNo)
	if errors.Is(err, db.ErrNotFound) || (err == nil && (current.ProjectID != project.ID || embedding.EmbedText(current.Title, current.Body) != scanned.Content)) {
		s.deferred[doc.Doc] = true
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Eligibility can wait for an upstream sync. Recheck bytes that arrived
	// during that check before handing this document to the paid encoder.
	if err := s.reuseArtifacts(ctx, s.generation, doc.Doc); err != nil {
		return false, err
	}
	if _, pending := s.pending[doc.Doc]; !pending {
		// Preserve the imported chunks; the existing stale exclusion continues
		// scanning without stamping this document with an empty prepared result.
		s.deferred[doc.Doc] = true
		return false, nil
	}

	return true, nil
}

func (s *artifactCaptureStore) prepare(ctx context.Context, doc kitvec.Pending[string]) ([]kitvec.PreparedChunk, error) {
	ready, err := s.checkPending(ctx, doc)
	if err != nil || !ready {
		return nil, err
	}
	chunks := kitvec.Split(doc.Content, kitvec.SplitOptions{MaxRunes: splitMaxRunes, Overlap: splitOverlap})
	prepared := make([]kitvec.PreparedChunk, len(chunks))
	for i, chunk := range chunks {
		prepared[i] = kitvec.PreparedChunk{Index: chunk.Index, Text: chunk.Text}
	}
	s.prepared[doc.Doc] = doc
	return prepared, nil
}

func (s *artifactCaptureStore) SaveVectors(ctx context.Context, key, doc string, revision any, vectors []kitvec.ChunkVector) error {
	// Kit serializes saves, but other chunk batches may still enter the
	// encoder boundary. Keep retention, index save and page bookkeeping
	// atomic with those checks so our own just-produced artifact is not
	// mistaken for an asynchronously received artifact.
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	if s.deferred[doc] {
		return kitvec.ErrStale
	}
	if len(vectors) > 0 {
		scanned, ok := s.pending[doc]
		if !ok {
			return fmt.Errorf("%w: no scanned artifact input", db.ErrFederationIngestValidation)
		}
		// Transport bounds do not shrink the existing local index contract.
		// An oversized successful local result remains local; it must not be
		// stamped as a partial artifact or trigger another paid fill attempt.
		if len(scanned.Content) > embedding.MaxArtifactInputBytes || len(vectors) > embedding.MaxArtifactChunks || s.recipe.Dimensions > embedding.MaxArtifactDimensions || int64(len(vectors))*int64(s.recipe.Dimensions)*4 > embedding.MaxArtifactVectorBytes {
			return s.saveVectors(ctx, key, doc, revision, vectors)
		}
		chunks := kitvec.Split(scanned.Content, kitvec.SplitOptions{MaxRunes: splitMaxRunes, Overlap: splitOverlap})
		if len(chunks) != len(vectors) {
			return db.ErrFederationIngestValidation
		}
		values := make([][]float32, len(vectors))
		for i, chunk := range vectors {
			if chunk.ChunkIndex != chunks[i].Index {
				return db.ErrFederationIngestValidation
			}
			values[i] = []float32(chunk.Vector)
		}
		identity := s.recipe
		identity.ProjectUID, identity.IssueUID = scanned.projectUID, doc
		artifact, err := embedding.NewArtifact(identity, scanned.Content, values)
		if err != nil {
			return err
		}
		durable, err := s.artifacts.RetainEmbeddingArtifact(ctx, artifact)
		if err != nil {
			return err
		}
		if !durable {
			return kitvec.ErrStale
		}
	}
	return s.saveVectors(ctx, key, doc, revision, vectors)
}

func (s *artifactCaptureStore) saveVectors(ctx context.Context, key, doc string, revision any, vectors []kitvec.ChunkVector) error {
	if err := s.Store.SaveVectors(ctx, key, doc, revision, vectors); err != nil {
		return err
	}
	delete(s.pending, doc)
	delete(s.prepared, doc)
	return nil
}
