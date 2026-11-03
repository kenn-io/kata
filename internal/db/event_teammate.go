package db

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"

	"go.kenn.io/kata/internal/teammate"
)

type eventTeammateSuppressionKey struct{}

// WithoutEventTeammate suppresses display attribution for writes derived from
// imported or provider-owned history while preserving request cancellation.
func WithoutEventTeammate(ctx context.Context) context.Context {
	return context.WithValue(teammate.WithRequest(ctx, ""), eventTeammateSuppressionKey{}, true)
}

// StampEventTeammate appends the display field before native event hashing.
// Existing payload spelling is retained; imported events never call this helper.
func StampEventTeammate(ctx context.Context, kind, actor, payload string) (string, error) {
	handle, _ := teammate.FromRequest(ctx)
	if actor == "system" || ctx.Value(recurringWriteKey{}) == true ||
		ctx.Value(eventTeammateSuppressionKey{}) == true || !contributorEvent(kind) {
		return payload, nil
	}
	// These two legacy event shapes have a display fallback to their routing
	// teammate. New native writes must retain an explicit empty contributor so
	// routing is not shown as authorship when the request selected no teammate.
	emptyDisplay := handle == "" && (kind == "issue.created" || kind == "issue.commented")
	if handle == "" && !emptyDisplay {
		return payload, nil
	}
	if handle != "" {
		if err := teammate.Validate(handle); err != nil {
			return payload, err
		}
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(payload), &fields); err != nil {
		return payload, err
	}
	if fields == nil {
		return payload, nil
	}
	if _, ok := fields["attributed_teammate"]; ok {
		return payload, nil
	}
	encoded, err := json.Marshal(handle)
	if err != nil {
		return payload, err
	}
	separator := ""
	if len(fields) > 0 {
		separator = ","
	}
	end := strings.LastIndexByte(payload, '}')
	return payload[:end] + separator + `"attributed_teammate":` + string(encoded) + payload[end:], nil
}

func contributorEvent(kind string) bool {
	switch kind {
	case "issue.created", "issue.updated", "issue.commented", "issue.comment_edited",
		"issue.assigned", "issue.unassigned", "issue.assignment_renewed",
		"issue.priority_set", "issue.priority_cleared", "issue.labeled", "issue.unlabeled",
		"issue.metadata_updated", "issue.linked", "issue.unlinked", "issue.links_changed",
		"issue.closed", "issue.reopened", "issue.soft_deleted", "issue.restored", "issue.moved",
		"issue.external_root_bound", "issue.external_root_unbound", "issue.external_root_paused",
		"issue.external_root_resumed", "issue.external_field_resolved", "issue.external_field_conflicted", "issue.external_comment_resolved",
		"claim.acquired", "claim.released", "claim.force_released", "claim.violated":
		return true
	default:
		return false
	}
}
