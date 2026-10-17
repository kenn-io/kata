package vector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync"
	"unicode/utf8"

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
	backing := &artifactCaptureStore{Store: ix.flowStore, index: ix, source: source, artifacts: artifacts, exporter: exporter, recipe: recipe, generation: key, pending: map[string]artifactPending{}, ineligible: map[string]struct{}{}, stale: map[string]struct{}{}}
	if len(eligibility) > 0 {
		backing.eligible = eligibility[0]
	}
	stats, err := fill(ctx, backing, key, backing.beforeDispatch(enc), scanBatch, batchOptions, onDocument, backing.prepare, backing.deferEncodedDocument)
	deferred, deferredErr := backing.ineligiblePendingCount(ctx, key)
	if err == nil {
		err = deferredErr
	}
	return stats, deferred, err
}

// errDispatchDeferred leaves a document pending without aborting the fill.
// Kit isolates it to the deferred document; the other documents proceed.
var errDispatchDeferred = errors.New("document deferred before paid dispatch")

type preparedDocument struct {
	kitvec.Pending[string]
	texts []string
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
	pageOrder   []string
	prepared    map[string]preparedDocument
	eligible    func(context.Context, string) (bool, error)
	pageAllowed map[string]bool
	pageChecked map[string]bool
	deferred    map[string]bool
	ineligible  map[string]struct{}
	// stale mirrors kit's per-Fill set: a document whose save reported
	// ErrStale is not prepared again during this Fill.
	stale       map[string]struct{}
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
// Recheck the documents in each encoder batch at the actual encoder boundary,
// authorizing each project once per dispatch. Changed documents remain
// unstamped for the existing worker's next normal retry.
func (s *artifactCaptureStore) beforeDispatch(enc kitvec.EncodeFunc) kitvec.EncodeFunc {
	if enc == nil {
		return nil
	}
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		if err := s.admitDispatch(ctx, texts); err != nil {
			return nil, err
		}
		return enc(ctx, texts)
	}
}

func (s *artifactCaptureStore) admitDispatch(ctx context.Context, texts []string) (err error) {
	// Batch options may permit parallel encoder calls. Serialize these
	// checks and their artifact reconciliation, preserving HTTP parallelism.
	s.dispatchMu.Lock()
	defer func() {
		if err != nil && !errors.Is(err, errDispatchDeferred) {
			s.dispatchErr = err
		}
		s.dispatchMu.Unlock()
	}()
	if s.dispatchErr != nil {
		return s.dispatchErr
	}
	inBatch := make(map[string]bool, len(texts))
	for _, text := range texts {
		inBatch[text] = true
	}
	allowed := make(map[string]bool)
	deferred := false
	var admitted []string
	for _, doc := range s.prepared {
		if !slices.ContainsFunc(doc.texts, func(text string) bool { return inBatch[text] }) {
			continue
		}
		ok, err := s.admitPending(ctx, doc.Doc, allowed)
		if err != nil {
			return err
		}
		if !ok {
			deferred = true
			continue
		}
		admitted = append(admitted, doc.Doc)
	}
	// Every project in this batch is authorized before any content is read,
	// so bytes an eligibility check pulled are visible to the recheck.
	if err := s.checkPendingContents(ctx, admitted); err != nil {
		return err
	}
	for _, doc := range admitted {
		deferred = deferred || s.deferred[doc]
	}
	if deferred {
		return errDispatchDeferred
	}
	return nil
}

// Kit reports a deferred batch against each document it isolates. A document
// that only shares chunk text with a deferred one also waits for the next fill.
func (s *artifactCaptureStore) deferEncodedDocument(doc string, err error) bool {
	if !errors.Is(err, errDispatchDeferred) {
		return false
	}
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	s.deferred[doc] = true
	return true
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
		imported, err := s.index.reuseArtifact(ctx, key, s.source, artifact, expected, scanned.Revision)
		if err != nil {
			return err
		}
		if imported {
			delete(s.pending, artifact.IssueUID)
		}
	}
	return nil
}

func (s *artifactCaptureStore) scanPending(ctx context.Context, key string, limit int) ([]kitvec.Pending[string], error) {
	pending, err := s.Store.PendingForGeneration(ctx, key, limit)
	if err != nil {
		return nil, err
	}
	s.pending = make(map[string]artifactPending, len(pending))
	s.pageOrder = make([]string, 0, len(pending))
	s.prepared = make(map[string]preparedDocument, len(pending))
	s.pageAllowed = make(map[string]bool)
	s.pageChecked = make(map[string]bool)
	s.deferred = make(map[string]bool)
	for _, doc := range pending {
		s.pageOrder = append(s.pageOrder, doc.Doc)
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
// allowed caches project authorization for one page preparation or dispatch.
func (s *artifactCaptureStore) admitPending(ctx context.Context, doc string, allowed map[string]bool) (bool, error) {
	scanned, ok := s.pending[doc]
	if !ok || s.deferred[doc] {
		s.deferred[doc] = true
		return false, nil
	}
	if s.eligible != nil {
		permitted, checked := allowed[scanned.projectUID]
		if !checked {
			var err error
			permitted, err = s.eligible(ctx, scanned.projectUID)
			if err != nil {
				return false, err
			}
			allowed[scanned.projectUID] = permitted
		}
		if !permitted {
			s.deferred[doc] = true
			s.ineligible[doc] = struct{}{}
			return false, nil
		}
	}
	return true, nil
}

// checkPendingContents rechecks admitted documents against canonical content
// and retained artifacts, deferring each one that is no longer ready. Reads
// are batched per project and the artifact recheck covers the whole batch.
func (s *artifactCaptureStore) checkPendingContents(ctx context.Context, docs []string) error {
	byProject := make(map[string][]string)
	var projects []string
	for _, doc := range docs {
		scanned, ok := s.pending[doc]
		if !ok {
			return kitvec.ErrStale
		}
		if _, seen := byProject[scanned.projectUID]; !seen {
			projects = append(projects, scanned.projectUID)
		}
		byProject[scanned.projectUID] = append(byProject[scanned.projectUID], doc)
	}
	var current []string
	for _, projectUID := range projects {
		projectDocs := byProject[projectUID]
		deferAll := func() {
			for _, doc := range projectDocs {
				s.deferred[doc] = true
			}
		}
		// Eligibility may pull new content after the mirror scan. Narrow the
		// native read to the admitted project and defer stale input before
		// paid dispatch.
		if !db.ProjectAttributionVisible(ctx, projectUID) {
			deferAll()
			continue
		}
		scope := db.WithAuthorizedProjects(ctx, []string{projectUID})
		project, err := s.source.ProjectByUID(scope, projectUID)
		if errors.Is(err, db.ErrNotFound) || (err == nil && project.DeletedAt != nil) {
			deferAll()
			continue
		}
		if err != nil {
			return err
		}
		issues, err := s.source.IssuesByUIDs(scope, projectDocs, db.IncludeDeletedNo)
		if err != nil {
			return err
		}
		for _, doc := range projectDocs {
			issue, found := issues[doc]
			if !found || issue.ProjectID != project.ID || embedding.EmbedText(issue.Title, issue.Body) != s.pending[doc].Content {
				s.deferred[doc] = true
				continue
			}
			current = append(current, doc)
		}
	}
	if len(current) == 0 {
		return nil
	}
	// Eligibility can wait for an upstream sync. Recheck bytes that arrived
	// during that check before handing these documents to the paid encoder.
	if err := s.reuseArtifacts(ctx, s.generation, current...); err != nil {
		return err
	}
	for _, doc := range current {
		if _, pending := s.pending[doc]; !pending {
			// Preserve the imported chunks; the existing stale exclusion
			// continues scanning without stamping this document with an empty
			// prepared result.
			s.deferred[doc] = true
		}
	}
	return nil
}

// checkPageDocument rechecks an admitted document's content together with
// the unchecked documents in this page that share its already admitted
// project and that kit will still prepare. The paid encoder boundary rechecks
// each document again before dispatch.
func (s *artifactCaptureStore) checkPageDocument(ctx context.Context, doc string) error {
	projectUID := s.pending[doc].projectUID
	docs := []string{doc}
	s.pageChecked[doc] = true
	for _, peer := range s.pageOrder {
		scanned, ok := s.pending[peer]
		_, stale := s.stale[peer]
		if peer == doc || !ok || stale || scanned.projectUID != projectUID || s.deferred[peer] || s.pageChecked[peer] {
			continue
		}
		s.pageChecked[peer] = true
		docs = append(docs, peer)
	}
	return s.checkPendingContents(ctx, docs)
}

func (s *artifactCaptureStore) prepare(ctx context.Context, doc kitvec.Pending[string]) ([]kitvec.PreparedChunk, error) {
	admitted, err := s.admitPending(ctx, doc.Doc, s.pageAllowed)
	if err != nil || !admitted {
		return nil, err
	}
	if !s.pageChecked[doc.Doc] {
		if err := s.checkPageDocument(ctx, doc.Doc); err != nil {
			return nil, err
		}
	}
	if s.deferred[doc.Doc] {
		return nil, nil
	}
	chunks := kitvec.Split(doc.Content, kitvec.SplitOptions{MaxRunes: splitMaxRunes, Overlap: splitOverlap})
	prepared := make([]kitvec.PreparedChunk, len(chunks))
	texts := make([]string, len(chunks))
	for i, chunk := range chunks {
		prepared[i] = kitvec.PreparedChunk{Index: chunk.Index, Text: chunk.Text}
		texts[i] = chunk.Text
	}
	s.prepared[doc.Doc] = preparedDocument{Pending: doc, texts: texts}
	return prepared, nil
}

func (s *artifactCaptureStore) SaveVectors(ctx context.Context, key, doc string, revision any, vectors []kitvec.ChunkVector) (err error) {
	// Kit serializes saves, but other chunk batches may still enter the
	// encoder boundary. Keep retention, index save and page bookkeeping
	// atomic with those checks so our own just-produced artifact is not
	// mistaken for an asynchronously received artifact.
	s.dispatchMu.Lock()
	defer func() {
		if errors.Is(err, kitvec.ErrStale) {
			s.stale[doc] = struct{}{}
		}
		s.dispatchMu.Unlock()
	}()
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
		if len(scanned.Content) > embedding.MaxArtifactInputBytes ||
			artifactRawWindowExceeded(scanned.Content, s.recipe) ||
			len(vectors) > embedding.MaxArtifactChunks ||
			s.recipe.Dimensions > embedding.MaxArtifactDimensions ||
			int64(len(vectors))*int64(s.recipe.Dimensions)*4 > embedding.MaxArtifactVectorBytes {
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

func artifactRawWindowExceeded(input string, identity embedding.ArtifactIdentity) bool {
	if !utf8.ValidString(input) ||
		identity.SplitMaxRunes < 1 ||
		identity.SplitMaxRunes > embedding.MaxArtifactInputBytes ||
		identity.SplitOverlap < 0 ||
		identity.SplitOverlap >= identity.SplitMaxRunes {
		return false
	}
	maxRunes := int64(identity.SplitMaxRunes) + int64(embedding.MaxArtifactChunks-1)*int64(identity.SplitMaxRunes-identity.SplitOverlap)
	return int64(utf8.RuneCountInString(input)) > maxRunes
}

func (s *artifactCaptureStore) saveVectors(ctx context.Context, key, doc string, revision any, vectors []kitvec.ChunkVector) error {
	if err := s.Store.SaveVectors(ctx, key, doc, revision, vectors); err != nil {
		return err
	}
	delete(s.pending, doc)
	delete(s.prepared, doc)
	return nil
}
