package vector

import (
	"context"
	"errors"

	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kit/embedclient"
	kitvec "go.kenn.io/kit/vector"
)

// Chunking bounds what one encode input carries; the recipe itself no longer
// truncates. Runes approximate tokens loosely; 2000 runes keeps chunks well
// under common embedding-model context limits.
const (
	splitMaxRunes = embedding.RecipeSplitMaxRunes
	splitOverlap  = embedding.RecipeSplitOverlap
)

// Fill embeds every pending mirror document into the generation keyed by key.
// scanBatch <= 0 uses kit's default. batchOptions controls encode batching.
//
// Only an input rejection skips a document (poison-document stamping): kit
// classifies a failed response from its status and the provider's error code
// or message, and reports InputRejected only for an input that is too long or
// refused by policy. A 400 kit cannot attribute to the input (bad model,
// unsupported field, malformed request) and every non-400 error abort the
// fill, so the reconciler backs off instead of stamping the corpus as
// skipped. An auth failure never stamps anything.
func (ix *Index) Fill(ctx context.Context, key string, enc kitvec.EncodeFunc, scanBatch int, batchOptions []kitvec.BatchOption, onDocument func(bool)) (kitvec.FillStats, error) {
	return fill(ctx, ix.flowStore, key, enc, scanBatch, batchOptions, onDocument)
}

func fill(ctx context.Context, backing kitvec.Store[string, string], key string, enc kitvec.EncodeFunc, scanBatch int, batchOptions []kitvec.BatchOption, onDocument func(bool), prepare ...func(context.Context, kitvec.Pending[string]) ([]kitvec.PreparedChunk, error)) (kitvec.FillStats, error) {
	split := kitvec.SplitOptions{MaxRunes: splitMaxRunes, Overlap: splitOverlap}
	store := progressStore{Store: backing, onDocument: onDocument}
	options := []kitvec.FillOption[string]{
		kitvec.WithFillScanBatch[string](scanBatch),
		kitvec.WithFillSplit[string](split),
		kitvec.WithFillBatch[string](batchOptions...),
		kitvec.WithFillBatchErrorIsolation[string](isInputRejected),
		kitvec.WithFillEncodeError[string](func(_ string, err error) bool {
			return isInputRejected(err)
		}),
	}
	if len(prepare) > 0 && prepare[0] != nil {
		options = append(options, kitvec.WithFillPrepared[string](prepare[0]))
	}
	return kitvec.Fill(ctx, store, key, enc, options...)
}

func isInputRejected(err error) bool {
	apiErr, ok := errors.AsType[*embedclient.APIError](err)
	return ok && apiErr.InputRejected()
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
