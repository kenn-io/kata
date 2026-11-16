package dbtest

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// IssueDocument completes payload, a hand-written issue.created or
// issue.snapshot payload for issueUID, with each field every writer sends
// that payload lacks: uid, a short_id derived from issueUID, the title
// "Issue", an empty body, the author "worker", the status "open", empty
// metadata, and a fixed created_at. Fields payload already has keep their
// values and order; the missing ones are appended.
func IssueDocument(t testing.TB, issueUID string, payload jsontext.Value) jsontext.Value {
	t.Helper()
	var present map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(payload, &present), "issue document fixture must be a JSON object")
	defaults := []struct {
		field string
		value any
	}{
		{"uid", issueUID},
		{"short_id", strings.ToLower(issueUID[len(issueUID)-4:])},
		{"title", "Issue"},
		{"body", ""},
		{"author", "worker"},
		{"status", "open"},
		{"metadata", map[string]any{}},
		{"created_at", "2026-05-23T12:00:00.000Z"},
	}
	out := bytes.TrimSpace(bytes.Clone(payload))
	out = out[:len(out)-1]
	separator := len(present) > 0
	for _, field := range defaults {
		if _, ok := present[field.field]; ok {
			continue
		}
		value, err := json.Marshal(field.value)
		require.NoError(t, err)
		if separator {
			out = append(out, ',')
		}
		separator = true
		out = append(out, `"`+field.field+`":`...)
		out = append(out, value...)
	}
	return jsontext.Value(append(out, '}'))
}
