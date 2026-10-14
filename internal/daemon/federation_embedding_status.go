package daemon

import (
	"context"
	"errors"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

const federationArtifactStatusLimit = 32

func addFederationEmbeddingStatus(ctx context.Context, cfg ServerConfig, body *api.FederationStatusBody) error {
	for i := range body.Statuses {
		project, err := cfg.DB.ProjectByUID(ctx, body.Statuses[i].ProjectUID)
		if err != nil {
			return internalAPIError(err)
		}
		status, err := federationEmbeddingStatus(ctx, cfg, project)
		if err != nil {
			return internalAPIError(err)
		}
		body.Statuses[i].Embedding = status
	}
	return nil
}

func federationEmbeddingStatus(ctx context.Context, cfg ServerConfig, project db.Project) (*api.FederationEmbeddingStatus, error) {
	if !db.ProjectAttributionVisible(ctx, project.UID) {
		return nil, db.ErrNotFound
	}
	status := &api.FederationEmbeddingStatus{State: "unconfigured", ArtifactLimit: federationArtifactStatusLimit, Artifacts: []api.FederationEmbeddingArtifactStatus{}}
	producer, err := db.ProjectEmbeddingProducerFromMetadata(project.Metadata)
	if err != nil {
		status.State = "rejected"
		// A selection made before this daemon's recipe version changed no
		// longer validates; this daemon still needs a new root selection.
		if db.ProjectEmbeddingProducerInstance(project.Metadata) == cfg.DB.InstanceUID() {
			status.Reason = "producer_recipe_changed"
		}
		return status, nil
	}
	status.Producer = producer
	if producer != nil {
		status.State = "waiting"
	}
	if project.DeletedAt != nil {
		status.State = "inactive"
		return status, nil
	}
	artifacts, ok := cfg.DB.(db.EmbeddingArtifactStorage)
	if !ok {
		return status, nil
	}
	manifests, err := artifacts.EmbeddingArtifactManifests(ctx, project.UID, federationArtifactStatusLimit)
	if err != nil {
		return nil, err
	}
	status.Limited = len(manifests) == federationArtifactStatusLimit
	var localRecipe *embedding.ArtifactIdentity
	if cfg.Embedder != nil {
		recipe, err := cfg.Embedder.ArtifactIdentity("", "", "")
		if err != nil {
			return nil, err
		}
		localRecipe = &recipe
	}
	if producer != nil && localRecipe != nil && producer.ProducerInstanceUID == cfg.DB.InstanceUID() {
		configured := *localRecipe
		configured.ProjectUID, configured.IssueUID, configured.InputHash, configured.ProducerInstanceUID = "", "", "", ""
		if producer.Recipe != configured {
			status.Reason = "producer_recipe_changed"
		}
	}
	// A failure belongs to the selection it was observed under. Another
	// producer, or a newer selection, makes it obsolete.
	if status.Reason == "" && producer != nil && producer.ProducerInstanceUID == cfg.DB.InstanceUID() && cfg.ReconcilerHealth != nil {
		if failure, ok := cfg.ReconcilerHealth().ProducerErrors[project.UID]; ok && failure.Message != "" && failure.Producer == *producer {
			status.Reason, status.LastError = "producer_unavailable", failure.Message
		}
	}
	for _, manifest := range manifests {
		issue, err := cfg.DB.IssueByUID(ctx, manifest.IssueUID, db.IncludeDeletedNo)
		if errors.Is(err, db.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if issue.ProjectID != project.ID {
			continue
		}
		artifact := api.FederationEmbeddingArtifactStatus{IssueUID: manifest.IssueUID, Digest: manifest.Digest, State: "reused"}
		if manifest.ProducerInstanceUID == cfg.DB.InstanceUID() {
			artifact.State = "generated"
		}
		matchesRecipe := false
		if localRecipe != nil {
			received := manifest.ArtifactIdentity
			expected := *localRecipe
			received.ProjectUID, received.IssueUID, received.InputHash, received.ProducerInstanceUID = "", "", "", ""
			expected.ProjectUID, expected.IssueUID, expected.InputHash, expected.ProducerInstanceUID = "", "", "", ""
			matchesRecipe = received == expected
		}
		switch {
		case manifest.InputHash != embedding.ArtifactInputHash(embedding.EmbedText(issue.Title, issue.Body)):
			artifact.State, artifact.Reason = "incompatible", "input_changed"
		case cfg.VectorIndex == nil || cfg.Embedder == nil || !cfg.VectorIndex.ArtifactDimensionsSupported(manifest.Dimensions):
			artifact.State, artifact.Reason = "stored_unindexed", "index_unavailable"
		case !matchesRecipe:
			artifact.State, artifact.Reason = "incompatible", "recipe_mismatch"
		case cfg.VectorIndex.ArtifactRejected(manifest.Digest):
			artifact.State, artifact.Reason = "incompatible", "invalid_artifact"
		}
		status.Artifacts = append(status.Artifacts, artifact)
		if status.State == "waiting" || status.State == "unconfigured" {
			status.State = artifact.State
		}
	}
	return status, nil
}
