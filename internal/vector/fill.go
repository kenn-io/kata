package vector

import (
	"context"
	"errors"
	"net/http"

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
// Only a content-definitive HTTP 400 skips a document (poison-document
// stamping); every other error aborts the fill so the reconciler can back
// off — an auth failure must never stamp the corpus as skipped.
func (ix *Index) Fill(ctx context.Context, key string, enc kitvec.EncodeFunc, scanBatch, encodeBatch int) (kitvec.FillStats, error) {
	return kitvec.Fill(ctx, ix.store, key, enc, kitvec.FillOptions[string]{
		ScanBatch: scanBatch,
		Split:     kitvec.SplitOptions{MaxRunes: splitMaxRunes, Overlap: splitOverlap},
		Batch:     kitvec.BatchOptions{BatchSize: encodeBatch},
		OnEncodeError: func(_ string, err error) bool {
			var apiErr *embedding.APIError
			return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest
		},
	})
}
