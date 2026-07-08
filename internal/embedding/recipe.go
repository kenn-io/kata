// Package embedding produces vector embeddings of issue text via an
// OpenAI-compatible HTTP endpoint. It is storage-free: it imports neither
// internal/db nor internal/daemon and operates on plain strings.
package embedding

// RecipeVersion is part of the generation fingerprint. Bump it when EmbedText
// changes, so every stored embedding is recomputed against the new recipe.
const RecipeVersion = 2

// EmbedText is the v2 recipe: title and body joined, untruncated. Chunking
// (kit vector.Split) bounds what is sent to the embedder; the recipe no longer
// truncates. Comments are intentionally excluded (see the design note).
func EmbedText(title, body string) string {
	return title + "\n\n" + body
}
