package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// Project configuration selects who may generate; source actor labels and
// artifact producer attribution never grant generation authority.
func (r *Reconciler) projectGenerationAllowed(ctx context.Context, projectUID string, recipe embedding.ArtifactIdentity, syncArtifacts bool) (bool, error) {
	project, err := r.store.ProjectByUID(ctx, projectUID)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if project.DeletedAt != nil {
		return false, nil
	}
	binding, err := r.store.FederationBindingByProject(ctx, project.ID)
	if errors.Is(err, db.ErrNotFound) {
		return true, nil
	} // Private generation stays local.
	if err != nil {
		return false, err
	}
	if !binding.Enabled && binding.Role == db.FederationRoleSpoke && binding.RelayConfig != nil {
		return false, nil
	}
	if binding.Role == db.FederationRoleSpoke {
		// Negotiated replicas require a live authenticated producer check.
		// Legacy direct federation retains its existing local worker behavior.
		if binding.RelayConfig == nil {
			return true, nil
		}
		if r.cfg.FederationProducerAllowed == nil {
			return false, nil
		}
		fork, _ := ctx.Value(producerAdmissionContextKey{}).(activity.Admission)
		return r.cfg.FederationProducerAllowed(ctx, projectUID, recipe, fork, syncArtifacts)
	}
	producer, err := db.ProjectEmbeddingProducerFromMetadata(project.Metadata)
	if err != nil {
		// A retained owner backup may predate validation of this reserved key.
		// Keep this project pending without stopping unrelated private work.
		return false, nil
	}
	if producer == nil {
		pin, err := r.store.RootAuthority(ctx, projectUID)
		if errors.Is(err, db.ErrNotFound) {
			return true, nil
		} // Legacy direct hub.
		if err != nil {
			return false, err
		}
		if pin.AuthorityUID != r.store.InstanceUID() {
			return false, nil
		}
		recipe.ProjectUID, recipe.IssueUID, recipe.InputHash, recipe.ProducerInstanceUID = "", "", "", ""
		candidate := db.ProjectEmbeddingProducer{ProducerInstanceUID: r.store.InstanceUID(), Recipe: recipe}
		if err := candidate.Validate(); err != nil {
			return false, err
		}
		raw, err := json.Marshal(candidate)
		if err != nil {
			return false, err
		}
		changed, err := r.store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: project.ID, Actor: "system", IfMatchRev: &project.Revision, Patch: map[string]jsontext.Value{db.ProjectEmbeddingMetadataKey: raw}})
		if _, conflict := errors.AsType[*db.RevisionConflictError](err); conflict {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if changed.Changed && r.cfg.OnProjectEvent != nil {
			fork, _ := ctx.Value(producerAdmissionContextKey{}).(activity.Admission)
			r.cfg.OnProjectEvent(changed.Event, fork)
		}
		producer = &candidate
	}
	if producer.ProducerInstanceUID != r.store.InstanceUID() {
		return false, nil
	}
	recipe.ProjectUID, recipe.IssueUID, recipe.InputHash, recipe.ProducerInstanceUID = "", "", "", ""
	return producer.Recipe == recipe, nil
}

type producerAdmissionContextKey struct{}
