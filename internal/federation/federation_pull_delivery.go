package federation

import (
	"context"
	"errors"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/httpurl"
)

type pulledOwnership struct{ issue, project string }

type pulledDuplicateRecovery struct {
	store    db.Storage
	binding  db.FederationBinding
	resume   bool
	moves    map[pulledOwnership]int64
	bindings map[string]db.FederationBinding
	statuses map[int64]db.FederationSyncStatus
}

func newPulledDuplicateRecovery(store db.Storage, binding db.FederationBinding, page []api.EventEnvelope, resume bool) *pulledDuplicateRecovery {
	r := &pulledDuplicateRecovery{store: store, binding: binding, resume: resume, moves: map[pulledOwnership]int64{}, bindings: map[string]db.FederationBinding{binding.HubProjectUID: binding}, statuses: map[int64]db.FederationSyncStatus{}}
	for _, event := range page {
		if event.Type == "issue.moved" && event.IssueUID != nil {
			to, _ := db.StringValue(db.PayloadMap(event.Payload)["to_project_uid"])
			key := pulledOwnership{*event.IssueUID, to}
			r.moves[key] = max(r.moves[key], event.EventID)
		}
	}
	return r
}

// Normal overlap/backfill duplicates stay silent. An unfinished source pull can
// still need apply-time view invalidation; only a recorded failure allows a
// different stream to recover its hooks. Recovery remains at-least-once when
// existing cursors cannot prove acknowledgement; there is no outbox.
func (r *pulledDuplicateRecovery) recover(ctx context.Context, event api.EventEnvelope) (publish, invalidate bool, err error) {
	source, err := r.projectBinding(ctx, event.ProjectUID)
	if err != nil {
		return false, false, err
	}
	if source.ProjectID != 0 && source.PullCursorEventID >= event.EventID {
		return false, false, nil
	}
	publish, invalidate = r.resume, r.resume
	if !r.resume && source.ProjectID != 0 && source.ProjectID != r.binding.ProjectID {
		status, known := r.statuses[source.ProjectID]
		if !known {
			status, err = r.store.FederationSyncStatusByProject(ctx, source.ProjectID)
			if err != nil && !errors.Is(err, db.ErrNotFound) {
				return false, false, err
			}
			r.statuses[source.ProjectID] = status
		}
		invalidate = status.LastPullStartedAt != nil && (status.LastPullSuccessAt == nil || status.LastPullStartedAt.After(*status.LastPullSuccessAt))
		publish = invalidate && status.LastError != nil && *status.LastError != "" &&
			status.LastErrorAt != nil && !status.LastErrorAt.Before(*status.LastPullStartedAt)
	}
	if !invalidate {
		return false, false, nil
	}
	if event.IssueUID != nil {
		issue, err := r.store.IssueByUID(ctx, *event.IssueUID, db.IncludeDeletedYes)
		if errors.Is(err, db.ErrNotFound) {
			return publish, invalidate, nil
		}
		if err != nil {
			return false, false, err
		}
		owner, err := r.store.FederationBindingByProject(ctx, issue.ProjectID)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return false, false, err
		}
		// A destination cursor may predate ownership and pass unrelated source
		// history. Require its incoming move cursor as acknowledgement evidence.
		move := r.moves[pulledOwnership{*event.IssueUID, owner.HubProjectUID}]
		if err == nil && r.compatible(owner) && move > 0 && owner.PullCursorEventID >= max(move, event.EventID) {
			return false, false, nil
		}
	}
	return publish, invalidate, nil
}

func (r *pulledDuplicateRecovery) projectBinding(ctx context.Context, projectUID string) (db.FederationBinding, error) {
	if binding, known := r.bindings[projectUID]; known {
		return binding, nil
	}
	project, err := r.store.ProjectByUID(ctx, projectUID)
	if err != nil {
		return db.FederationBinding{}, err
	}
	binding, err := r.store.FederationBindingByProject(ctx, project.ID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return db.FederationBinding{}, err
	}
	if err != nil || !r.compatible(binding) {
		binding = db.FederationBinding{}
	}
	r.bindings[projectUID] = binding
	return binding, nil
}

func (r *pulledDuplicateRecovery) compatible(binding db.FederationBinding) bool {
	if binding.Role != db.FederationRoleSpoke || binding.RelayConfig != nil {
		return false
	}
	origin, err := httpurl.CanonicalHTTPOrigin(binding.HubURL)
	current, currentErr := httpurl.CanonicalHTTPOrigin(r.binding.HubURL)
	return err == nil && currentErr == nil && origin == current
}
