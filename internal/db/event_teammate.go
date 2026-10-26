package db

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"

	"go.kenn.io/kata/internal/teammate"
)

type eventTeammateKey struct{}

// WithEventTeammate sets request attribution; an empty handle clears an inherited value.
func WithEventTeammate(ctx context.Context, handle string) context.Context {
	return context.WithValue(ctx, eventTeammateKey{}, handle)
}

// EventTeammateFromContext returns the request's teammate, or an empty string.
func EventTeammateFromContext(ctx context.Context) string {
	handle, _ := ctx.Value(eventTeammateKey{}).(string)
	return handle
}

// StampEventTeammate appends request attribution without rewriting existing JSON bytes.
// Record-owned events and system actors retain their original attribution.
func StampEventTeammate(ctx context.Context, eventType, actor, payload string) (string, error) {
	handle := EventTeammateFromContext(ctx)
	if handle == "" || actor == "system" || eventTeammateOwnedByRecord(eventType) {
		return payload, nil
	}
	if err := teammate.Validate(handle); err != nil {
		return payload, err
	}
	var value jsontext.Value
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return payload, err
	}
	if value.Kind() != '{' {
		return payload, nil
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(value, &fields); err != nil {
		return payload, err
	}
	if _, present := fields["teammate"]; present {
		return payload, nil
	}
	encoded, err := json.Marshal(handle)
	if err != nil {
		return payload, err
	}
	separator := ""
	if len(fields) != 0 {
		separator = ","
	}
	// Editing only the closing delimiter preserves member order, whitespace,
	// numeric precision and string escape spellings in the original value.
	end := strings.LastIndexByte(payload, '}')
	return payload[:end] + separator + `"teammate":` + string(encoded) + payload[end:], nil
}

func eventTeammateOwnedByRecord(eventType string) bool {
	switch eventType {
	case "issue.created", "issue.commented", "cron.run.observed", "cron.run.snapshot":
		return true
	default:
		return false
	}
}

// EventTeammate derives valid attribution; a present top-level key takes precedence.
func EventTeammate(eventType, payload string) string {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(payload), &fields); err != nil {
		return ""
	}
	if _, present := fields["teammate"]; present {
		handle, _ := IssueTeammate(JSONBlob(payload))
		return handle
	}
	if eventType == "issue.created" {
		handle, _ := IssueTeammate(JSONBlob(fields["metadata"]))
		return handle
	}
	return ""
}
