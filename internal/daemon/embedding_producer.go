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
		// A direct spoke generates locally unless an enabled binding names
		// another instance as the producer, in which case it defers to it.
		if binding.RelayConfig == nil {
			return r.directSpokeGenerationAllowed(project, binding, recipe), nil
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
	return r.selectLocalProducer(ctx, project, binding, recipe)
}

// directSpokeGenerationAllowed denies a direct spoke's local generation when
// its binding is enabled and the project's readable producer is another
// instance. An absent or unreadable selection, a selection of this instance, or
// a disabled binding keeps the local worker. A remote producer whose recipe
// differs from this daemon's is reported so status can say why nothing here
// will use its artifacts. A matching recipe clears only that recorded
// mismatch, leaving fetch failures to their own reporter.
func (r *Reconciler) directSpokeGenerationAllowed(project db.Project, binding db.FederationBinding, recipe embedding.ArtifactIdentity) bool {
	if !binding.Enabled {
		// Local generation resumes, so a recorded fetch failure no longer applies.
		r.setProducerError(project.UID, ProducerFailure{})
		return true
	}
	producer, err := db.ProjectEmbeddingProducerFromMetadata(project.Metadata)
	if err != nil || producer == nil || producer.ProducerInstanceUID == r.store.InstanceUID() {
		return true
	}
	recipe.ProjectUID, recipe.IssueUID, recipe.InputHash, recipe.ProducerInstanceUID = "", "", "", ""
	if producer.Recipe != recipe {
		r.setProducerError(project.UID, ProducerFailure{Message: recipeMismatchMessage, Producer: *producer})
	} else {
		r.clearProducerError(project.UID, recipeMismatchMessage)
	}
	return false
}

// selectLocalProducer records this root as the project's producer with its
// configured recipe. A root that produces for itself follows its own model and
// recipe changes; otherwise its outdated selection would stop all generation.
// A hub with no pinned root records itself too, so spokes can find its
// producer; a pin owned by another authority, or any non-hub role without a
// pin, records nothing and reports false.
func (r *Reconciler) selectLocalProducer(ctx context.Context, project db.Project, binding db.FederationBinding, recipe embedding.ArtifactIdentity) (bool, error) {
	pin, err := r.store.RootAuthority(ctx, project.UID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		if binding.Role != db.FederationRoleHub {
			return false, nil
		}
		if !binding.Enabled {
			// A paused unpinned hub keeps generating locally and records nothing.
			return true, nil
		}
	case err != nil:
		return false, err
	case pin.AuthorityUID != r.store.InstanceUID():
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

// selectIdleHubProducers records this root as the producer of every enabled
// hub project that has not selected it, so spokes find an idle hub's producer
// without waiting for new content. Failures are logged per project and never
// stop the pass: a lost IfMatchRev race returns false and retries next time.
func (r *Reconciler) selectIdleHubProducers(ctx context.Context, recipe embedding.ArtifactIdentity) error {
	bindings, err := r.store.ListFederationBindings(ctx)
	if err != nil {
		slog.Warn("idle hub producer selection: list bindings failed", "err", err)
		return nil
	}
	self := r.store.InstanceUID()
	recipe.ProjectUID, recipe.IssueUID, recipe.InputHash, recipe.ProducerInstanceUID = "", "", "", ""
	for _, binding := range bindings {
		if !binding.Enabled || binding.Role != db.FederationRoleHub {
			continue
		}
		project, err := r.store.ProjectByID(ctx, binding.ProjectID)
		if errors.Is(err, db.ErrNotFound) {
			continue
		}
		if err != nil {
			slog.Warn("idle hub producer selection: load project failed", "project_id", binding.ProjectID, "err", err)
			continue
		}
		if project.DeletedAt != nil {
			continue
		}
		producer, err := db.ProjectEmbeddingProducerFromMetadata(project.Metadata)
		switch {
		case err != nil:
			// Never overwrite an unreadable selection that names another instance.
			if db.ProjectEmbeddingProducerInstance(project.Metadata) != self {
				continue
			}
		case producer != nil && producer.ProducerInstanceUID != self:
			continue
		case producer != nil:
			current := producer.Recipe
			current.ProjectUID, current.IssueUID, current.InputHash, current.ProducerInstanceUID = "", "", "", ""
			if current == recipe {
				continue
			}
		}
		if _, err := r.selectLocalProducer(ctx, project, binding, recipe); err != nil {
			slog.Warn("idle hub producer selection failed", "project_uid", project.UID, "err", err)
		}
	}
	return nil
}

// recipeMismatchMessage is recorded for a direct spoke whose remote producer
// uses a different recipe than this daemon.
const recipeMismatchMessage = "producer recipe differs from this daemon's embedding recipe"

// clearProducerError removes a project's recorded failure only when its
// message is exactly message.
func (r *Reconciler) clearProducerError(projectUID, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.health.ProducerErrors[projectUID].Message == message {
		delete(r.health.ProducerErrors, projectUID)
	}
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
