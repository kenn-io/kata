package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"strings"

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
		allowed, err := r.cfg.FederationProducerAllowed(ctx, projectUID, recipe, fork, syncArtifacts)
		if err != nil && errors.Is(err, db.ErrEmbeddingProducerUnavailable) {
			// Keep other projects filling; the project retries on the next fill.
			slog.Warn("embedding producer check failed", "project_uid", projectUID, "err", err)
			var selected db.ProjectEmbeddingProducer
			if producer, decodeErr := db.ProjectEmbeddingProducerFromMetadata(project.Metadata); decodeErr == nil && producer != nil {
				selected = *producer
			}
			r.setProducerError(projectUID, ProducerFailure{Message: err.Error(), Producer: selected})
			return false, nil
		}
		if err == nil {
			r.setProducerError(projectUID, ProducerFailure{})
		}
		return allowed, err
	}
	self := r.store.InstanceUID()
	recipe.ProjectUID, recipe.IssueUID, recipe.InputHash, recipe.ProducerInstanceUID = "", "", "", ""
	producer, err := db.ProjectEmbeddingProducerFromMetadata(project.Metadata)
	switch {
	case err != nil:
		// A retained owner backup or an older recipe version can fail
		// validation. Keep this project pending without stopping unrelated
		// private work, unless this root's own selection can be replaced.
		if db.ProjectEmbeddingProducerInstance(project.Metadata) != self {
			return false, nil
		}
	case producer != nil && producer.ProducerInstanceUID != self:
		return false, nil
	case producer != nil && producer.Recipe == recipe:
		return true, nil
	}
	return r.selectLocalProducer(ctx, project, recipe, producer == nil && err == nil)
}

// selectLocalProducer records this root as the project's producer with its
// configured recipe. A root that produces for itself follows its own model and
// recipe changes; otherwise its outdated selection would stop all generation.
// unselected reports that no producer was recorded, which a legacy direct hub
// without a pinned root may generate for.
func (r *Reconciler) selectLocalProducer(ctx context.Context, project db.Project, recipe embedding.ArtifactIdentity, unselected bool) (bool, error) {
	pin, err := r.store.RootAuthority(ctx, project.UID)
	if errors.Is(err, db.ErrNotFound) {
		return unselected, nil
	}
	if err != nil {
		return false, err
	}
	if pin.AuthorityUID != r.store.InstanceUID() {
		return false, nil
	}
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
	return true, nil
}

// maxProducerErrorBytes bounds the failure text kept for status.
const maxProducerErrorBytes = 512

// setProducerError records a failure, or clears it when Message is empty.
func (r *Reconciler) setProducerError(projectUID string, failure ProducerFailure) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if failure.Message == "" {
		delete(r.health.ProducerErrors, projectUID)
		return
	}
	if len(failure.Message) > maxProducerErrorBytes {
		failure.Message = strings.ToValidUTF8(failure.Message[:maxProducerErrorBytes], "")
	}
	if r.health.ProducerErrors == nil {
		r.health.ProducerErrors = make(map[string]ProducerFailure)
	}
	r.health.ProducerErrors[projectUID] = failure
}

type producerAdmissionContextKey struct{}
