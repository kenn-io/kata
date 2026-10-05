package embedding

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"math"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kata/internal/config"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func artifactTestHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
func artifactTestIdentity() ArtifactIdentity {
	return ArtifactIdentity{ProjectUID: "00000000000000000000000001", IssueUID: "00000000000000000000000002", ProducerInstanceUID: "00000000000000000000000003", Provider: "openai-compatible", Model: "example-model", ModelRevision: "revision-1", Dimensions: 2, InputType: "none", Normalization: "l2", Preprocessing: "kata.issue/v2", RecipeVersion: 2, SplitMaxRunes: 2000, SplitOverlap: 200, RecipeFingerprint: strings.Repeat("a", 64)}
}

// R7/A12: retain every actual recipe2 chunk and original float32 bits. A
// first-chunk or averaged export cannot satisfy this round trip.
func TestEmbeddingArtifactRoundTrip(t *testing.T) {
	input := EmbedText("Title", strings.Repeat("界", 2500))
	artifact, err := NewArtifact(artifactTestIdentity(), input, [][]float32{{1, 0}, {0, 1}})
	require.NoError(t, err)
	require.NoError(t, ValidateArtifact(artifact, input, strings.Repeat("a", 64)))
	require.Len(t, artifact.Chunks, 2)
	require.Equal(t, artifactTestHash("Title\n\n"+strings.Repeat("界", 1993)), artifact.Chunks[0].InputHash)
	require.Equal(t, artifactTestHash(strings.Repeat("界", 707)), artifact.Chunks[1].InputHash)
	require.Equal(t, []byte{0, 0, 128, 63, 0, 0, 0, 0}, artifact.Chunks[0].VectorBytes)
	require.Equal(t, []byte{0, 0, 0, 0, 0, 0, 128, 63}, artifact.Chunks[1].VectorBytes)
	raw, err := json.Marshal(artifact)
	require.NoError(t, err)
	var restored EmbeddingArtifact
	require.NoError(t, json.Unmarshal(raw, &restored, json.RejectUnknownMembers(true)))
	require.NoError(t, ValidateArtifact(restored, input, strings.Repeat("a", 64)))
	require.Equal(t, artifact, restored)
	manifest := artifact.Manifest()
	require.NoError(t, ValidateArtifactManifest(manifest))
	require.Equal(t, 2, manifest.ChunkCount)
	require.Equal(t, int64(16), manifest.VectorByteSize)
	offer, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NotContains(t, string(offer), "vector_bytes\"")
	require.NotContains(t, string(offer), "AACAPwAAAAA=")
	require.Equal(t, artifact.Digest, manifest.Digest)
}

func TestEmbeddingArtifactRejectsInvalidCompleteSets(t *testing.T) {
	input := strings.Repeat("x", 2100)
	valid, err := NewArtifact(artifactTestIdentity(), input, [][]float32{{1, 0}, {0, 1}})
	require.NoError(t, err)
	for name, damage := range map[string]func(*EmbeddingArtifact){
		"missing chunk":       func(a *EmbeddingArtifact) { a.Chunks = a.Chunks[:1] },
		"duplicate index":     func(a *EmbeddingArtifact) { a.Chunks[1].Index = 0 },
		"nonzero first index": func(a *EmbeddingArtifact) { a.Chunks[0].Index = 1 },
		"wrong dimensions":    func(a *EmbeddingArtifact) { a.Dimensions = 3 },
		"invalid dimensions":  func(a *EmbeddingArtifact) { a.Dimensions = 32769 },
		"nonfinite component": func(a *EmbeddingArtifact) {
			binary.LittleEndian.PutUint32(a.Chunks[0].VectorBytes, math.Float32bits(float32(math.Inf(1))))
		},
		"nan component": func(a *EmbeddingArtifact) {
			binary.LittleEndian.PutUint32(a.Chunks[0].VectorBytes, math.Float32bits(float32(math.NaN())))
		},
		"changed vector":   func(a *EmbeddingArtifact) { a.Chunks[0].VectorBytes[0] ^= 1 },
		"changed input":    func(a *EmbeddingArtifact) { a.InputHash = strings.Repeat("b", 64) },
		"changed recipe":   func(a *EmbeddingArtifact) { a.ModelRevision = "revision-2" },
		"changed producer": func(a *EmbeddingArtifact) { a.ProducerInstanceUID = "00000000000000000000000004" },
		"bad digest":       func(a *EmbeddingArtifact) { a.Digest = strings.Repeat("b", 64) },
		"bad encoding":     func(a *EmbeddingArtifact) { a.Encoding = "halfvec" },
		"too many chunks":  func(a *EmbeddingArtifact) { a.Chunks = make([]ChunkArtifact, 4097) },
		"oversize vectors": func(a *EmbeddingArtifact) { a.Chunks[0].VectorBytes = make([]byte, (16<<20)+1) },
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(valid)
			require.NoError(t, err)
			var a EmbeddingArtifact
			require.NoError(t, json.Unmarshal(raw, &a))
			damage(&a)
			require.Error(t, ValidateArtifact(a, input, strings.Repeat("a", 64)))
		})
	}
	_, err = NewArtifact(artifactTestIdentity(), input, [][]float32{{1, 0}})
	require.Error(t, err, "a producer cannot emit an incomplete chunk set")
	require.Error(t, ValidateArtifact(valid, strings.Repeat("b", 64), strings.Repeat("a", 64)))
	require.Error(t, ValidateArtifact(valid, input, strings.Repeat("b", 64)))
	manifest := valid.Manifest()
	manifest.VectorByteSize = 16<<20 + 1
	require.Error(t, ValidateArtifactManifest(manifest))
	manifest = valid.Manifest()
	manifest.ChunkCount++
	require.Error(t, ValidateArtifactManifest(manifest))
}

// Exact portable float32 bits survive round trips across finite input values,
// including negative zero; changing any vector bytes invalidates the digest.
func TestEmbeddingArtifactFiniteRoundTrips(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sign := rapid.Uint32Range(0, 1).Draw(t, "sign")
		exponent := rapid.Uint32Range(0, 254).Draw(t, "finite exponent")
		mantissa := rapid.Uint32Range(0, 0x7fffff).Draw(t, "mantissa")
		x := math.Float32frombits(sign<<31 | exponent<<23 | mantissa)
		dimensions := rapid.IntRange(1, 32768).Draw(t, "dimensions")
		identity := artifactTestIdentity()
		identity.Dimensions = dimensions
		identity.Normalization = "none"
		values := make([]float32, dimensions)
		for i := range values {
			values[i] = 1
		}
		values[0] = x
		artifact, err := NewArtifact(identity, "Input", [][]float32{values})
		require.NoError(t, err)
		require.NoError(t, ValidateArtifact(artifact, "Input", strings.Repeat("a", 64)))
		require.Equal(t, math.Float32bits(x), binary.LittleEndian.Uint32(artifact.Chunks[0].VectorBytes))
		artifact.Chunks[0].VectorBytes[0] ^= 1
		require.Error(t, ValidateArtifact(artifact, "Input", strings.Repeat("a", 64)))
	})
}

func FuzzEmbeddingArtifactValidation(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"chunks":[],"dimensions":32769}`))
	f.Fuzz(func(_ *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			return
		}
		var artifact EmbeddingArtifact
		if json.Unmarshal(raw, &artifact, json.RejectUnknownMembers(true)) == nil {
			_ = ValidateArtifact(artifact, "", "")
			_ = ValidateArtifactManifest(artifact.Manifest())
		}
	})
}

// R7: a self-consistent digest over an incomplete or mismatched chunk set is
// insufficient to attach vectors to the recipient's current canonical input.
func TestEmbeddingArtifactCurrentInputRequiresEveryChunk(t *testing.T) {
	input := strings.Repeat("x", 2100)
	for _, kind := range []string{"missing chunk", "wrong chunk input"} {
		t.Run(kind, func(t *testing.T) {
			artifact, err := NewArtifact(artifactTestIdentity(), input, [][]float32{{1, 0}, {0, 1}})
			require.NoError(t, err)
			if kind == "missing chunk" {
				artifact.Chunks = artifact.Chunks[:1]
			} else {
				artifact.Chunks[1].InputHash = strings.Repeat("b", 64)
			}
			artifact.Digest, err = completeArtifactDigest(artifact)
			require.NoError(t, err)
			require.Error(t, ValidateArtifact(artifact, input, artifact.RecipeFingerprint))
		})
	}
}

// R7/A12: portability uses the full current client recipe, excluding operational
// endpoint/credential/batch choices while distinguishing model revision and width.
func TestEmbeddingArtifactClientIdentity(t *testing.T) {
	makeIdentity := func(cfg Config) ArtifactIdentity {
		client, err := New(cfg)
		require.NoError(t, err)
		portable, ok := any(client).(interface {
			ArtifactIdentity(string, string, string) (ArtifactIdentity, error)
		})
		require.True(t, ok, "embedding client must expose its exact portable recipe identity")
		identity, err := portable.ArtifactIdentity("00000000000000000000000001", "00000000000000000000000002", "00000000000000000000000003")
		require.NoError(t, err)
		return identity
	}
	cfg := Config{BaseURL: "https://encoder.example/v1", Model: "example-model", Salt: "revision-1", Dims: 3, Credential: config.EmbeddingCredential{Key: "example-secret-token", Source: "inline"}}
	first := makeIdentity(cfg)
	require.Equal(t, "openai-compatible", first.Provider)
	require.Equal(t, "example-model", first.Model)
	require.Equal(t, "revision-1", first.ModelRevision)
	require.Equal(t, 3, first.Dimensions)
	require.Equal(t, "none", first.InputType)
	require.Equal(t, "l2", first.Normalization)
	require.Equal(t, "kata.issue/v2", first.Preprocessing)
	require.Equal(t, 2, first.RecipeVersion)
	require.Equal(t, 2000, first.SplitMaxRunes)
	require.Equal(t, 200, first.SplitOverlap)
	require.Equal(t, "float32-le", first.Encoding)
	require.Len(t, first.RecipeFingerprint, 64)
	_, err := hex.DecodeString(first.RecipeFingerprint)
	require.NoError(t, err)
	operational := cfg
	operational.BaseURL = "https://other-encoder.example/v1"
	operational.Credential.Key = "another-example-secret"
	operational.BatchSize = 1
	operational.Timeout = time.Second
	require.Equal(t, first, makeIdentity(operational), "moving an unpinned compatible endpoint must reuse the same recipe")
	for _, mode := range []string{"model", "revision", "dimensions"} {
		changed := cfg
		switch mode {
		case "model":
			changed.Model = "different-model"
		case "revision":
			changed.Salt = "revision-2"
		case "dimensions":
			changed.Dims = 4
		}
		require.NotEqual(t, first.RecipeFingerprint, makeIdentity(changed).RecipeFingerprint, mode)
	}
	raw, err := json.Marshal(first)
	require.NoError(t, err)
	require.NotContains(t, string(raw), cfg.Credential.Key)
	require.NotContains(t, string(raw), cfg.BaseURL)
}
