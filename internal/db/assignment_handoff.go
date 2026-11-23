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

// ReclaimedAssignmentHandoffPatch clears the automatic expiry hand-off.
func ReclaimedAssignmentHandoffPatch(status string, metadata JSONBlob) (map[string]jsontext.Value, bool) {
	values := PayloadMap(jsontext.Value(metadata))
	attention, _ := StringValue(values["work.attention"])
	message, _ := StringValue(values["work.attention_msg"])
	if status != "open" || (attention != "needs-human" && attention != "ok") || message != expiredAssignmentMessage {
		return nil, false
	}
	return map[string]jsontext.Value{
		"work.attention":     jsontext.Value(`"ok"`),
		"work.attention_msg": jsontext.Value(`null`),
	}, true
}
