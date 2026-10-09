package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"time"
	"unicode/utf8"
)

// ValidateCommentReply checks the persisted link shape without resolving its
// target. Federation and restores may deliver the target later.
func ValidateCommentReply(uid, target, kind string) error {
	if (target == "") != (kind == "") {
		return fmt.Errorf("reply_to_uid and reply_kind must be paired")
	}
	if target == "" {
		return nil
	}
	if utf8.RuneCountInString(target) != 26 || target == uid {
		return fmt.Errorf("reply_to_uid must name another 26-character comment UID")
	}
	if !ValidCommentReplyKind(kind) {
		return fmt.Errorf("invalid reply_kind %q", kind)
	}
	return nil
}

// ValidCommentReplyKind reports whether kind is an accepted persisted reply
// kind. Projections use the same contract to avoid exposing stale or invalid
// reply metadata.
func ValidCommentReplyKind(kind string) bool {
	switch kind {
	case "reply", "confirm", "refute", "supersede":
		return true
	default:
		return false
	}
}

func validateCommentEntry(entry map[string]jsontext.Value) error {
	values := map[string]string{}
	present := map[string]bool{}
	for _, field := range []string{"comment_uid", "reply_to_uid", "reply_kind", "edited_at"} {
		raw := entry[field]
		if len(raw) == 0 {
			continue
		}
		var value *string
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("%s must be a string", field)
		}
		if value != nil {
			values[field] = *value
			present[field] = true
		}
	}
	if present["reply_to_uid"] != present["reply_kind"] {
		return fmt.Errorf("reply_to_uid and reply_kind must be paired")
	}
	if present["reply_to_uid"] && (values["reply_to_uid"] == "" || values["reply_kind"] == "") {
		return fmt.Errorf("reply fields must be nonempty when present")
	}
	if err := ValidateCommentReply(values["comment_uid"], values["reply_to_uid"], values["reply_kind"]); err != nil {
		return err
	}
	if present["edited_at"] {
		if _, err := time.Parse(time.RFC3339Nano, values["edited_at"]); err != nil {
			return fmt.Errorf("invalid edited_at: %w", err)
		}
	}
	return nil
}
