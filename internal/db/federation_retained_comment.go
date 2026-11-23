package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// RetainedCommentPayload makes a retained comment publishable under the current actor.
func RetainedCommentPayload(payload string, eventType, commentUID, actor, body string) (string, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(payload), &fields); err != nil {
		return "", err
	}
	if eventType != "issue.commented" {
		var comments []map[string]jsontext.Value
		if err := json.Unmarshal(fields["comments"], &comments); err != nil {
			return "", err
		}
		fields = nil
		for _, comment := range comments {
			var uid string
			if err := json.Unmarshal(comment["comment_uid"], &uid); err != nil {
				return "", err
			}
			if uid == commentUID {
				fields = comment
				break
			}
		}
	}
	if fields == nil {
		return "", ErrNotFound
	}
	author, err := json.Marshal(actor)
	if err != nil {
		return "", err
	}
	fields["author"] = author
	encodedBody, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	fields["body"] = encodedBody
	delete(fields, "idempotency_key")
	delete(fields, "idempotency_fingerprint")
	result, err := json.Marshal(fields)
	return string(result), err
}
