package db

import (
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/uid"
)

// ProjectEmbeddingMetadataKey names the root-owned stable producer configuration in project metadata.
const ProjectEmbeddingMetadataKey = "federation_embedding"

// ProjectEmbeddingProducer is a stable project configuration, not a document
// job. Dynamic document and producer attribution stay in individual artifacts.
type ProjectEmbeddingProducer struct {
	ProducerInstanceUID string                     `json:"producer_instance_uid"`
	Recipe              embedding.ArtifactIdentity `json:"recipe"`
}

// Validate checks the stable producer UID and exact preferred recipe identity.
func (p ProjectEmbeddingProducer) Validate() error {
	r := p.Recipe
	fingerprint, err := hex.DecodeString(r.RecipeFingerprint)
	if !uid.Valid(p.ProducerInstanceUID) || err != nil || len(fingerprint) != 32 || r.Provider == "" || r.Model == "" || r.Dimensions <= 0 || r.Dimensions > embedding.MaxArtifactDimensions || r.RecipeVersion != embedding.RecipeVersion || r.SplitMaxRunes != embedding.RecipeSplitMaxRunes || r.SplitOverlap != embedding.RecipeSplitOverlap || r.Encoding != embedding.ArtifactFloat32Encoding || r.ProjectUID != "" || r.IssueUID != "" || r.InputHash != "" || r.ProducerInstanceUID != "" {
		return errors.New("invalid project embedding producer or preferred recipe")
	}
	return nil
}

// ProjectEmbeddingProducerFromMetadata decodes and validates the reserved producer configuration.
func ProjectEmbeddingProducerFromMetadata(metadata JSONBlob) (*ProjectEmbeddingProducer, error) {
	if metadata == "" {
		return nil, nil
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(metadata), &fields); err != nil {
		return nil, err
	}
	raw, found := fields[ProjectEmbeddingMetadataKey]
	if !found {
		return nil, nil
	}
	var producer ProjectEmbeddingProducer
	if err := json.Unmarshal(raw, &producer, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	if err := producer.Validate(); err != nil {
		return nil, err
	}
	return &producer, nil
}

// ValidateEmbeddingProducerEvent protects the root-owned producer configuration.
// authorityUID comes from the authenticated binding's pinned root; legacy spoke
// ingress passes an empty authority because it cannot author root policy.
func ValidateEmbeddingProducerEvent(event RemoteEvent, authorityUID string) error {
	if event.Type != "project.metadata_updated" {
		return nil
	}
	diff := PayloadMap(PayloadMap(event.Payload)["diff"])
	if _, change := diff[ProjectEmbeddingMetadataKey]; change && (authorityUID == "" || event.OriginInstanceUID != authorityUID) {
		return fmt.Errorf("%w: producer configuration requires root origin", ErrFederationIngestValidation)
	}
	return nil
}
