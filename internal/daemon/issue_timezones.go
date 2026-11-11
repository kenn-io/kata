package daemon

import "encoding/json/jsontext"

// issueTimezoneWriteRelevant excludes unrelated agent metadata from admission.
func issueTimezoneWriteRelevant(patch map[string]jsontext.Value) bool {
	for _, key := range []string{"timezone", "scheduled_on", "deadline_on"} {
		if _, ok := patch[key]; ok {
			return true
		}
	}
	return false
}
