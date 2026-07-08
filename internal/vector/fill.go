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

// probeText verifies that the embedding endpoint accepts well-formed input.
// Any trivially embeddable string works; the probe only proves the request
// path is healthy.
const probeText = "kata encode probe"

// Fill embeds every pending mirror document into the generation keyed by key.
// scanBatch and encodeBatch <= 0 use kit defaults / single-batch respectively.
//
// Only a content-definitive HTTP 400 skips a document (poison-document
// stamping). The embedding API reports request-level problems (bad model,
// malformed request) with the same status, so the first 400 in a run is
// verified with a one-off probe encode of known-good text: if the probe also
// fails, the 400 is request-level and the fill aborts so the reconciler can
// back off instead of stamping the corpus as skipped. Every non-400 error
// aborts unconditionally — an auth failure must never stamp anything.
func (ix *Index) Fill(ctx context.Context, key string, enc kitvec.EncodeFunc, scanBatch, encodeBatch int) (kitvec.FillStats, error) {
	probed, healthy := false, false
	return kitvec.Fill(ctx, ix.store, key, enc, kitvec.FillOptions[string]{
		ScanBatch: scanBatch,
		Split:     kitvec.SplitOptions{MaxRunes: splitMaxRunes, Overlap: splitOverlap},
		Batch:     kitvec.BatchOptions{BatchSize: encodeBatch},
		OnEncodeError: func(_ string, err error) bool {
			var apiErr *embedding.APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
				return false
			}
			// kit serializes OnEncodeError on the calling goroutine, so the
			// captured locals need no locking.
			if !probed {
				_, perr := enc(ctx, []string{probeText})
				probed, healthy = true, perr == nil
			}
			return healthy
		},
	})
}
