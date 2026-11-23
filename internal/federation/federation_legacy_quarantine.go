package federation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
)

func retryPushQuarantinedEvents(ctx context.Context, store db.Storage, binding *db.FederationBinding, q db.FederationQuarantine, events []db.Event) error {
	retry, reason, err := retryRetainedQuarantine(ctx, store, binding, q, events)
	if err != nil {
		return recordFederationSyncError(ctx, store, binding.ProjectID, err)
	}
	if !retry {
		return recordFederationSyncError(ctx, store, binding.ProjectID, ErrFederationPushQuarantined)
	}
	_, err = store.RetryFederationQuarantine(ctx, db.RetryFederationQuarantineParams{ID: q.ID, ProjectID: binding.ProjectID, Actor: binding.Actor, Reason: reason, Now: time.Now().UTC()})
	if err != nil {
		return recordFederationSyncError(ctx, store, binding.ProjectID, err)
	}
	return nil
}

// retryRetainedQuarantine recovers bindings created before retained boundaries existed.
func retryRetainedQuarantine(ctx context.Context, store db.Storage, binding *db.FederationBinding, q db.FederationQuarantine, events []db.Event) (bool, string, error) {
	if retry, reason := autoRetryFederationQuarantine(q, events); retry {
		return retry, reason, nil
	}
	hubError, ok := federationQuarantineHubError(q.Error)
	if !ok || hubError.Code != "validation" || q.Direction != db.FederationQuarantineDirectionPush {
		return false, "", nil
	}
	var rejected db.Event
	for _, event := range events {
		if event.OriginInstanceUID == store.InstanceUID() && event.Actor != binding.Actor && event.CreatedAt.Before(binding.CreatedAt) && strings.Contains(hubError.Message, fmt.Sprintf("event %s actor %q does not match bound actor", event.UID, event.Actor)) {
			rejected = event
			break
		}
	}
	if rejected.ID == 0 {
		return false, "", nil
	}
	if binding.RetainedThroughEventID == 0 {
		var boundary, after int64
		for {
			page, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: binding.ProjectID, AfterID: after, Limit: 1000})
			if err != nil {
				return false, "", err
			}
			for _, event := range page {
				if event.OriginInstanceUID == store.InstanceUID() && event.CreatedAt.Before(binding.CreatedAt) {
					boundary = event.ID
				}
			}
			if len(page) < 1000 {
				break
			}
			after = page[len(page)-1].ID
		}
		if boundary < rejected.ID {
			return false, "", nil
		}
		updated := *binding
		updated.RetainHistoryThrough = &boundary
		persisted, err := store.UpsertFederationBinding(ctx, updated)
		if err != nil {
			return false, "", err
		}
		*binding = persisted
	}
	if rejected.ID > binding.RetainedThroughEventID {
		return false, "", nil
	}
	return true, "auto-retry retained history from before rejoin", nil
}
