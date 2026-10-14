package db

import (
	"context"
	"errors"
	"iter"
	"time"

	"go.kenn.io/kata/internal/embedding"
)

// Staging limits bound temporary bytes that do not establish durable acceptance.
const (
	MaxStagedEmbeddingArtifacts      = 32
	MaxStagedEmbeddingBytes          = 32 << 20
	EmbeddingArtifactStagingLifetime = 24 * time.Hour
)

// ErrEmbeddingArtifactMiss is retryable with the same artifact identity.
// Temporary staging and its overflow never establish a durable relay prefix.
var ErrEmbeddingArtifactMiss = errors.New("retryable artifact miss")

// EmbeddingArtifactStorage retains original portable vectors independently of
// backend indexing. A false retention result means pre-content staging only.
type EmbeddingArtifactStorage interface {
	RetainEmbeddingArtifact(context.Context, embedding.EmbeddingArtifact) (bool, error)
	StoredEmbeddingArtifact(context.Context, string, string) (embedding.EmbeddingArtifact, error)
	EmbeddingArtifactManifests(context.Context, string, int) ([]embedding.ArtifactManifest, error)
}

// RelayArtifactStorage serves exact misses from already emitted artifact offers
// under the current binding authority and reset epoch.
type RelayArtifactStorage interface {
	RelayArtifactPayloads(context.Context, string, int64, []string) ([]embedding.EmbeddingArtifact, error)
}

// EmbeddingArtifactExport contains only durable original portable bytes;
// staging expires and is recovered from unacknowledged sender delivery.
type EmbeddingArtifactExport struct{ embedding.EmbeddingArtifact }

// ImportKind identifies durable vector bytes in owner backups.
func (*EmbeddingArtifactExport) ImportKind() string { return "federation_embedding_artifact" }

// EmbeddingArtifactExporter streams durable artifacts for admitted projects and issues.
type EmbeddingArtifactExporter interface {
	ExportEmbeddingArtifacts(context.Context, ExportFilter, ...string) iter.Seq2[ImportRecord, error]
}
