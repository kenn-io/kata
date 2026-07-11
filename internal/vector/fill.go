package vector

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"go.kenn.io/kata/internal/embedding"
	kitvec "go.kenn.io/kit/vector"
)

// Chunking bounds what one encode input carries; the recipe itself no longer
// truncates. Runes approximate tokens loosely; 2000 runes keeps chunks well
// under common embedding-model context limits.
const (
	splitMaxRunes = 2000
	splitOverlap  = 200
)

// Fill embeds every pending mirror document into the generation keyed by key.
// scanBatch and encodeBatch <= 0 use kit defaults / single-batch respectively.
//
// Only a content-definitive HTTP 400 skips a document (poison-document
// stamping). The embedding API reports request-level problems (bad model,
// malformed request, oversized batch) with the same status, so a 400 is
// verified by replaying the document's exact request shape with benign text:
// if the replay also fails, the rejection was not the content and the fill
// aborts so the reconciler can back off instead of stamping the corpus as
// skipped. Every non-400 error aborts unconditionally — an auth failure must
// never stamp anything.
func (ix *Index) Fill(ctx context.Context, key string, enc kitvec.EncodeFunc, scanBatch, encodeBatch int, onDocument func(bool)) (kitvec.FillStats, error) {
	split := kitvec.SplitOptions{MaxRunes: splitMaxRunes, Overlap: splitOverlap}
	batch := kitvec.BatchOptions{BatchSize: encodeBatch}
	store := progressStore{Store: ix.store, onDocument: onDocument}
	return kitvec.Fill(ctx, store, key, enc, kitvec.FillOptions[string]{
		ScanBatch: scanBatch,
		Split:     split,
		Batch:     batch,
		OnEncodeError: func(doc string, err error) bool {
			var apiErr *embedding.APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
				return false
			}
			return ix.contentSpecific400(ctx, doc, enc, split, batch)
		},
	})
}

type progressStore struct {
	kitvec.Store[string, string]
	onDocument func(bool)
}

func (s progressStore) SaveVectors(ctx context.Context, gen, doc string, revision any, vectors []kitvec.ChunkVector) error {
	if err := s.Store.SaveVectors(ctx, gen, doc, revision, vectors); err != nil {
		return err
	}
	if s.onDocument != nil {
		s.onDocument(len(vectors) > 0)
	}
	return nil
}

// contentSpecific400 reports whether a 400 from encoding doc is provably
// content-specific. It rebuilds the document's request shape — same chunk
// count, same per-chunk rune lengths, same batching — with benign text and
// re-encodes it. Success means the endpoint accepts that shape, so the
// rejection was the content itself and poison-skip is safe. Any replay
// failure (shape-level 400, transient error, missing mirror row) keeps the
// document pending by aborting the fill.
func (ix *Index) contentSpecific400(ctx context.Context, doc string, enc kitvec.EncodeFunc, split kitvec.SplitOptions, batch kitvec.BatchOptions) bool {
	var content string
	if err := ix.db.QueryRowContext(ctx,
		`SELECT content FROM issue_mirror WHERE issue_uid = ?`, doc).Scan(&content); err != nil {
		return false
	}
	chunks := kitvec.Split(content, split)
	for i := range chunks {
		chunks[i].Text = strings.Repeat("a", utf8.RuneCountInString(chunks[i].Text))
	}
	_, err := kitvec.EncodeBatched(ctx, enc, chunks, batch)
	return err == nil
}
