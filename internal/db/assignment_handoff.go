package db

import "encoding/json/jsontext"

const expiredAssignmentMessage = "claim expired without hand-off"

// ExpiredAssignmentHandoffPatch marks abandoned work for human attention.
func ExpiredAssignmentHandoffPatch(status string, metadata JSONBlob) (map[string]jsontext.Value, bool) {
	attention, _ := StringValue(PayloadMap(jsontext.Value(metadata))["work.attention"])
	if status != "open" || attention != "ok" {
		return nil, false
	}
	return map[string]jsontext.Value{
		"work.attention":     jsontext.Value(`"needs-human"`),
		"work.attention_msg": jsontext.Value(`"` + expiredAssignmentMessage + `"`),
	}, true
}
