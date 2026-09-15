package db

import (
	"encoding/json"

	"go.kenn.io/kata/internal/teammate"
)

// IssueTeammate returns the valid, non-empty teammate attribution in issue metadata.
func IssueTeammate(raw JSONBlob) (string, bool) {
	var metadata struct {
		Teammate json.RawMessage `json:"teammate"`
	}
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil || len(metadata.Teammate) == 0 {
		return "", false
	}
	var handle string
	if err := json.Unmarshal(metadata.Teammate, &handle); err != nil || handle == "" {
		return "", false
	}
	if err := teammate.Validate(handle); err != nil {
		return "", false
	}
	return handle, true
}
