// Package embedding produces vector embeddings of issue text via an
// OpenAI-compatible HTTP endpoint. It is storage-free: it imports neither
// internal/db nor internal/daemon and operates on plain strings.
package embedding

import kitvec "go.kenn.io/kit/vector"

// RecipeVersion is part of the generation fingerprint. Bump it when EmbedText
// changes, so every stored embedding is recomputed against the new recipe.
const RecipeVersion = 2

// ChunkMaxRunes and ChunkOverlapRunes bound what one encode input carries;
// the recipe itself does not truncate. Runes approximate tokens loosely; 2000
// runes keeps chunks well under common embedding-model context limits. Both
// values are part of the generation fingerprint: nodes that exchange vectors
// must split identical text into identical chunks.
const (
	ChunkMaxRunes     = 2000
	ChunkOverlapRunes = 200
)

// SplitOptions is the chunking every fill and every replica import uses.
func SplitOptions() kitvec.SplitOptions {
	return kitvec.SplitOptions{MaxRunes: ChunkMaxRunes, Overlap: ChunkOverlapRunes}
}

// EmbedText is the v2 recipe: title and body joined, untruncated. Chunking
// (kit vector.Split) bounds what is sent to the embedder; the recipe no longer
// truncates. Comments are intentionally excluded (see the design note).
func EmbedText(title, body string) string {
	return title + "\n\n" + body
}
