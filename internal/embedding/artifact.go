package embedding

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash"
	"math"
	"strings"
	"unicode/utf8"

	"go.kenn.io/kata/internal/uid"
	kitvec "go.kenn.io/kit/vector"
)

// Portable artifact bounds cap vector bytes, manifests, dimensions and source text.
const (
	MaxArtifactVectorBytes  = 16 << 20
	MaxArtifactBatchBytes   = 64 << 20
	MaxArtifactChunks       = 4096
	MaxArtifactDimensions   = 32768
	MaxArtifactInputBytes   = 32 << 20
	ArtifactFloat32Encoding = "float32-le"
)

// ArtifactIdentity contains portable input and recipe identity, never a local
// revision, credential or quantized backend representation.
type ArtifactIdentity struct {
	ProjectUID          string `json:"project_uid" required:"false"`
	IssueUID            string `json:"issue_uid" required:"false"`
	InputHash           string `json:"input_hash" required:"false"`
	RecipeFingerprint   string `json:"recipe_fingerprint"`
	Provider            string `json:"provider"`
	Model               string `json:"model"`
	ModelRevision       string `json:"model_revision" required:"false"`
	Dimensions          int    `json:"dimensions"`
	InputType           string `json:"input_type"`
	Normalization       string `json:"normalization"`
	Preprocessing       string `json:"preprocessing"`
	RecipeVersion       int    `json:"recipe_version"`
	SplitMaxRunes       int    `json:"split_max_runes"`
	SplitOverlap        int    `json:"split_overlap"`
	Encoding            string `json:"encoding"`
	ProducerInstanceUID string `json:"producer_instance_uid" required:"false"`
}

// ChunkArtifact contains one chunk’s identity and canonical float32 vector bytes.
type ChunkArtifact struct {
	Index        int    `json:"index"`
	InputHash    string `json:"input_hash"`
	VectorDigest string `json:"vector_digest"`
	VectorBytes  []byte `json:"vector_bytes"`
}

// ChunkManifest describes one chunk without transferring its vector bytes.
type ChunkManifest struct {
	Index        int    `json:"index"`
	InputHash    string `json:"input_hash"`
	VectorDigest string `json:"vector_digest"`
}

// EmbeddingArtifact contains a complete portable recipe, chunk manifest and canonical vectors.
//
//nolint:revive // The protocol/schema name distinguishes portable embedding artifacts from other federation artifacts.
type EmbeddingArtifact struct {
	ArtifactIdentity
	Chunks []ChunkArtifact `json:"chunks"`
	Digest string          `json:"digest"`
}

// ArtifactManifest is a projection of the complete artifact. Its digest is
// accepted as present only after exact validated bytes are durably retained.
type ArtifactManifest struct {
	ArtifactIdentity
	Chunks         []ChunkManifest `json:"chunks"`
	ChunkCount     int             `json:"chunk_count"`
	VectorByteSize int64           `json:"vector_byte_size"`
	Digest         string          `json:"digest"`
}

// ArtifactInputHash hashes the exact UTF-8 text supplied to the embedding recipe.
func ArtifactInputHash(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}
func artifactDigestValid(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func validateArtifactIdentity(identity ArtifactIdentity) error {
	if !uid.Valid(identity.ProjectUID) || !uid.Valid(identity.IssueUID) || !uid.Valid(identity.ProducerInstanceUID) {
		return errors.New("invalid artifact project, issue or producer identity")
	}
	if !artifactDigestValid(identity.InputHash) || !artifactDigestValid(identity.RecipeFingerprint) {
		return errors.New("invalid artifact input or recipe fingerprint")
	}
	if identity.Dimensions < 1 || identity.Dimensions > MaxArtifactDimensions || identity.RecipeVersion < 1 || identity.SplitMaxRunes < 1 || identity.SplitMaxRunes > MaxArtifactInputBytes || identity.SplitOverlap < 0 || identity.SplitOverlap >= identity.SplitMaxRunes || identity.Encoding != ArtifactFloat32Encoding {
		return errors.New("invalid artifact recipe, dimensions or encoding")
	}
	for _, s := range []string{identity.Provider, identity.Model, identity.InputType, identity.Normalization, identity.Preprocessing} {
		if s == "" || len(s) > 512 || !utf8.ValidString(s) {
			return errors.New("invalid artifact recipe metadata")
		}
	}
	if len(identity.ModelRevision) > 512 || !utf8.ValidString(identity.ModelRevision) {
		return errors.New("invalid artifact model revision")
	}
	return nil
}

// Manifest returns complete chunk metadata without the vector bytes.
func (artifact EmbeddingArtifact) Manifest() ArtifactManifest {
	manifest := ArtifactManifest{ArtifactIdentity: artifact.ArtifactIdentity, ChunkCount: len(artifact.Chunks), Digest: artifact.Digest}
	// Projection stays bounded even for hostile input passed to a validator.
	if len(artifact.Chunks) > MaxArtifactChunks {
		return manifest
	}
	manifest.Chunks = make([]ChunkManifest, len(artifact.Chunks))
	for i, chunk := range artifact.Chunks {
		manifest.Chunks[i] = ChunkManifest{Index: chunk.Index, InputHash: chunk.InputHash, VectorDigest: chunk.VectorDigest}
		manifest.VectorByteSize += int64(len(chunk.VectorBytes))
	}
	return manifest
}

// ValidateArtifactManifest validates recipe identity, chunk ordering and transfer bounds.
func ValidateArtifactManifest(manifest ArtifactManifest) error {
	if err := validateArtifactIdentity(manifest.ArtifactIdentity); err != nil {
		return err
	}
	if manifest.ChunkCount < 1 || manifest.ChunkCount > MaxArtifactChunks || manifest.ChunkCount != len(manifest.Chunks) || manifest.VectorByteSize < 1 || manifest.VectorByteSize > MaxArtifactVectorBytes || manifest.VectorByteSize != int64(manifest.ChunkCount)*int64(manifest.Dimensions)*4 || !artifactDigestValid(manifest.Digest) {
		return errors.New("invalid artifact manifest size or digest")
	}
	for i, chunk := range manifest.Chunks {
		if chunk.Index != i || !artifactDigestValid(chunk.InputHash) || !artifactDigestValid(chunk.VectorDigest) {
			return errors.New("invalid artifact ordered chunk manifest")
		}
	}
	return nil
}

func artifactWriteSized(h hash.Hash, raw []byte) {
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(len(raw)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(raw)
}
func completeArtifactDigest(artifact EmbeddingArtifact) (string, error) {
	manifest := artifact.Manifest()
	manifest.Digest = ""
	raw, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte("kata.embedding-artifact.v1\x00"))
	artifactWriteSized(h, raw)
	for _, chunk := range artifact.Chunks {
		artifactWriteSized(h, chunk.VectorBytes)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// NewArtifact captures original vectors before backend conversion. Splitting
// uses the current worker's exact rune windows and omission of blank windows.
func NewArtifact(identity ArtifactIdentity, input string, vectors [][]float32) (EmbeddingArtifact, error) {
	if len(input) > MaxArtifactInputBytes || !utf8.ValidString(input) {
		return EmbeddingArtifact{}, errors.New("artifact input exceeds bounds or is not UTF-8")
	}
	inputHash := ArtifactInputHash(input)
	if identity.InputHash != "" && identity.InputHash != inputHash {
		return EmbeddingArtifact{}, errors.New("artifact input hash differs from content")
	}
	identity.InputHash = inputHash
	if identity.Encoding == "" {
		identity.Encoding = ArtifactFloat32Encoding
	}
	if err := validateArtifactIdentity(identity); err != nil {
		return EmbeddingArtifact{}, err
	}
	if len(vectors) < 1 || len(vectors) > MaxArtifactChunks || int64(len(vectors))*int64(identity.Dimensions)*4 > MaxArtifactVectorBytes {
		return EmbeddingArtifact{}, errors.New("artifact vector set exceeds bounds")
	}
	// Bound the splitter's raw windows before it allocates rune/chunk slices.
	maxRunes := int64(identity.SplitMaxRunes) + int64(MaxArtifactChunks-1)*int64(identity.SplitMaxRunes-identity.SplitOverlap)
	if int64(utf8.RuneCountInString(input)) > maxRunes {
		return EmbeddingArtifact{}, errors.New("artifact input exceeds chunk window limit")
	}
	chunks := kitvec.Split(input, kitvec.SplitOptions{MaxRunes: identity.SplitMaxRunes, Overlap: identity.SplitOverlap})
	if len(chunks) != len(vectors) {
		return EmbeddingArtifact{}, errors.New("artifact requires the complete input chunk set")
	}
	artifact := EmbeddingArtifact{ArtifactIdentity: identity, Chunks: make([]ChunkArtifact, len(chunks))}
	for i, chunk := range chunks {
		//nolint:gosec // The complete chunk/vector lengths are checked equal immediately above.
		if len(vectors[i]) != identity.Dimensions {
			return EmbeddingArtifact{}, errors.New("artifact vector dimensions differ from recipe")
		}
		raw := make([]byte, identity.Dimensions*4)
		//nolint:gosec // The complete chunk/vector lengths are checked equal immediately above.
		for j, value := range vectors[i] {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return EmbeddingArtifact{}, errors.New("artifact vector contains a nonfinite component")
			}
			binary.LittleEndian.PutUint32(raw[j*4:], math.Float32bits(value))
		}
		artifact.Chunks[i] = ChunkArtifact{Index: i, InputHash: ArtifactInputHash(chunk.Text), VectorDigest: ArtifactInputHash(string(raw)), VectorBytes: raw}
	}
	digest, err := completeArtifactDigest(artifact)
	if err != nil {
		return EmbeddingArtifact{}, err
	}
	artifact.Digest = digest
	return artifact, ValidateArtifact(artifact, input, identity.RecipeFingerprint)
}

// ValidateArtifact verifies full portable bytes. Nonempty expected identities
// additionally enforce attachment to the caller's current input and recipe.
func ValidateArtifact(artifact EmbeddingArtifact, expectedInput, expectedRecipe string) error {
	if err := ValidateArtifactManifest(artifact.Manifest()); err != nil {
		return err
	}
	if expectedInput != "" && artifact.InputHash != ArtifactInputHash(expectedInput) || expectedRecipe != "" && artifact.RecipeFingerprint != expectedRecipe {
		return errors.New("artifact differs from expected input or recipe")
	}
	if expectedInput != "" {
		maxRunes := int64(artifact.SplitMaxRunes) + int64(MaxArtifactChunks-1)*int64(artifact.SplitMaxRunes-artifact.SplitOverlap)
		if len(expectedInput) > MaxArtifactInputBytes || !utf8.ValidString(expectedInput) || int64(utf8.RuneCountInString(expectedInput)) > maxRunes {
			return errors.New("artifact input exceeds bounds or is not UTF-8")
		}
		chunks := kitvec.Split(expectedInput, kitvec.SplitOptions{MaxRunes: artifact.SplitMaxRunes, Overlap: artifact.SplitOverlap})
		if len(chunks) != len(artifact.Chunks) {
			return errors.New("artifact requires the complete input chunk set")
		}
		for i, chunk := range chunks {
			if artifact.Chunks[i].InputHash != ArtifactInputHash(chunk.Text) {
				return errors.New("artifact chunk differs from expected input")
			}
		}
	}
	for _, chunk := range artifact.Chunks {
		if len(chunk.VectorBytes) != artifact.Dimensions*4 || ArtifactInputHash(string(chunk.VectorBytes)) != chunk.VectorDigest {
			return errors.New("artifact chunk vector size or digest differs")
		}
		for offset := 0; offset < len(chunk.VectorBytes); offset += 4 {
			value := math.Float32frombits(binary.LittleEndian.Uint32(chunk.VectorBytes[offset:]))
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return errors.New("artifact vector contains a nonfinite component")
			}
		}
	}
	digest, err := completeArtifactDigest(artifact)
	if err != nil {
		return err
	}
	if digest != artifact.Digest {
		return fmt.Errorf("artifact digest differs from complete bytes")
	}
	return nil
}

// ArtifactIdentity describes the exact configured document recipe. Credentials,
// batching, timeouts and unpinned endpoints never enter portable metadata.
func (c *Client) ArtifactIdentity(projectUID, issueUID, producerUID string) (ArtifactIdentity, error) {
	inputIdentity, err := c.space.InputIdentity()
	if err != nil {
		return ArtifactIdentity{}, err
	}
	recipe := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%d/%d", inputIdentity, ArtifactFloat32Encoding, RecipeSplitMaxRunes, RecipeSplitOverlap)))
	return ArtifactIdentity{
		ProjectUID: projectUID, IssueUID: issueUID, ProducerInstanceUID: producerUID,
		RecipeFingerprint: hex.EncodeToString(recipe[:]), Provider: "openai-compatible",
		Model: c.space.Model.Name, ModelRevision: c.space.Model.Revision,
		Dimensions: c.space.Model.Dimensions, InputType: string(c.space.Roles.InputType),
		Normalization: string(c.space.Model.Normalization), Preprocessing: c.space.Input.Recipe,
		RecipeVersion: RecipeVersion, SplitMaxRunes: RecipeSplitMaxRunes, SplitOverlap: RecipeSplitOverlap, Encoding: ArtifactFloat32Encoding,
	}, nil
}

// RebindArtifactProject changes local pre-federation identity during explicit
// adoption. Existing emitted relay identities are never rewritten this way.
// Stale artifacts do not need their original text to preserve validated bytes.
func RebindArtifactProject(artifact EmbeddingArtifact, projectUID string) (EmbeddingArtifact, error) {
	if err := ValidateArtifact(artifact, "", ""); err != nil {
		return EmbeddingArtifact{}, err
	}
	if !uid.Valid(projectUID) {
		return EmbeddingArtifact{}, errors.New("invalid adopted artifact project UID")
	}
	artifact.ProjectUID = projectUID
	digest, err := completeArtifactDigest(artifact)
	if err != nil {
		return EmbeddingArtifact{}, err
	}
	artifact.Digest = digest
	return artifact, ValidateArtifact(artifact, "", "")
}
