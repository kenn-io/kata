package db

import (
	"context"
	"encoding/json/v2"
	"errors"
	"slices"
)

type relayForwardPathKey struct{}

// WithRelayForwardPath carries an already validated ingress path only within
// the storage transaction. It is routing state, never an authorization grant.
func WithRelayForwardPath(ctx context.Context, path []string) context.Context {
	return context.WithValue(ctx, relayForwardPathKey{}, slices.Clone(path))
}

// RelayForwardPath returns a copy of the validated ingress routing path.
func RelayForwardPath(ctx context.Context) []string {
	path, _ := ctx.Value(relayForwardPathKey{}).([]string)
	return slices.Clone(path)
}

// Checkpoint capture and owner replay retain existing durable state without
// creating new ordinary deliveries. Only native snapshot/replay paths set this;
// no request field or transported actor label can establish it.
type relayStateCaptureKey struct{}

// WithRelayStateCapture marks native checkpoint or owner replay without new delivery.
func WithRelayStateCapture(ctx context.Context) context.Context {
	return context.WithValue(ctx, relayStateCaptureKey{}, true)
}

// RelayStateCapture reports the native checkpoint or owner replay marker.
func RelayStateCapture(ctx context.Context) bool {
	capture, _ := ctx.Value(relayStateCaptureKey{}).(bool)
	return capture
}

type relayArtifactSourceKey struct{}

// WithRelayArtifactSourceUID retains a validated incoming artifact offer's
// identity during forwarding. This is routing state, never project authority.
func WithRelayArtifactSourceUID(ctx context.Context, sourceUID string) context.Context {
	return context.WithValue(ctx, relayArtifactSourceKey{}, sourceUID)
}

// RelayArtifactSourceUID returns the validated offer identity being forwarded.
func RelayArtifactSourceUID(ctx context.Context) string {
	sourceUID, _ := ctx.Value(relayArtifactSourceKey{}).(string)
	return sourceUID
}

// RelayEventReferencedProjects discovers only structured native references.
func RelayEventReferencedProjects(event RemoteEvent, refs []string, lookup *EventIssueLookup, commentIssue, shortIssue func(string) (Issue, error)) ([]int64, error) {
	projects := []int64{}
	record := func(issue Issue, err error) error {
		if err != nil {
			return err
		}
		if !slices.Contains(projects, issue.ProjectID) {
			projects = append(projects, issue.ProjectID)
		}
		return nil
	}
	refs = slices.Clone(refs)
	if event.IssueUID != nil {
		refs = append(refs, *event.IssueUID)
	}
	if event.RelatedIssueUID != nil {
		refs = append(refs, *event.RelatedIssueUID)
	}
	var payload struct {
		ParentUID     *string    `json:"parent_uid"`
		ParentShortID *string    `json:"parent_short_id"`
		Evidence      []Evidence `json:"evidence"`
		ReplyToUID    *string    `json:"reply_to_uid"`
		Comments      []struct {
			ReplyToUID *string `json:"reply_to_uid"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return nil, errors.Join(ErrNotFound, err)
	}
	if event.Type == "issue.closed" {
		fields := PayloadMap(event.Payload)
		for _, key := range []string{"parent_uid", "parent_short_id"} {
			if raw, exists := fields[key]; exists && string(raw) == "null" {
				return nil, ErrNotFound
			}
		}
		if _, exists := fields["evidence"]; exists && payload.Evidence == nil {
			return nil, ErrNotFound
		}

		if payload.ParentUID != nil && *payload.ParentUID != "" {
			issue, err := lookup.IssueByUID(*payload.ParentUID)
			if err := record(issue, err); err != nil {
				return nil, err
			}
			if payload.ParentShortID != nil && *payload.ParentShortID != "" && issue.ShortID != *payload.ParentShortID {
				return nil, ErrNotFound
			}
		} else if payload.ParentShortID != nil && *payload.ParentShortID != "" {
			if err := record(shortIssue(*payload.ParentShortID)); err != nil {
				return nil, err
			}
		}
		for _, item := range payload.Evidence {
			if item.Type == "duplicate-of" || item.Type == "superseded-by" {
				if err := record(lookup.IssueByRef(event.ProjectUID, item.IssueRef)); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, ref := range refs {
		if ref == "" {
			return nil, ErrNotFound
		}
		if err := record(lookup.IssueByUID(ref)); err != nil {
			return nil, err
		}
	}
	replies := []*string{payload.ReplyToUID}
	for _, comment := range payload.Comments {
		replies = append(replies, comment.ReplyToUID)
	}
	for _, reply := range replies {
		if reply != nil && *reply != "" {
			if err := record(commentIssue(*reply)); err != nil {
				return nil, err
			}
		}
	}
	return projects, nil
}
