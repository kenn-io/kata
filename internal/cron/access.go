package cron

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// RequiresDefineGrant reports whether a job replacement needs execute-definition
// authority. The caller must additionally require it for renames and lifecycle
// changes: this helper sees only the documents, not the stored identity.
func RequiresDefineGrant(old, next *JobDefinition) bool {
	if (old == nil || old.Action.Kind != "execute") && (next == nil || next.Action.Kind != "execute") {
		return false
	}
	if old != nil && next != nil && old.Enabled && !next.Enabled {
		paused := *old
		paused.Enabled = false
		// Compare JSON object members without changing opaque numeric values.
		a, errA := json.Marshal(paused, json.Deterministic(true), jsontext.ReorderRawObjects(true))
		b, errB := json.Marshal(next, json.Deterministic(true), jsontext.ReorderRawObjects(true))
		if errA == nil && errB == nil && bytes.Equal(a, b) {
			return false
		}
	}
	return true
}
