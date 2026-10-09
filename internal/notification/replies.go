package notification

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

// Identity names one exact actor inbox, optionally narrowed to a teammate.
type Identity struct{ Actor, Teammate string }

// Address returns the exact recipient used by inbox and notify.
func Address(actor, teammate string) string {
	if teammate != "" {
		return actor + "/" + teammate
	}
	return actor
}

// LinkInput is captured by the writing daemon inside the comment transaction.
// PriorLinkers are newest first. PendingTargets maps recipients to the target
// of the comment currently referenced in their slot.
type LinkInput struct {
	Sender, Target                                             Identity
	ReplyUID, ReplyHandle, TargetUID, Kind, Owner, ParentOwner string
	PriorLinkers                                               []Identity
	Current                                                    map[string]jsontext.Value
	PendingTargets                                             map[string]string
}

// LinkPatch computes one explicit metadata patch. Replay never calls it.
func LinkPatch(in LinkInput) (map[string]jsontext.Value, error) {
	patch := map[string]jsontext.Value{}
	sender := Address(in.Sender.Actor, in.Sender.Teammate)
	if in.Kind == "reply" {
		var pending Value
		if json.Unmarshal(in.Current[MetadataKey(sender)], &pending) == nil && pending.Re != "" && pending.Re == in.TargetUID {
			patch[MetadataKey(sender)] = jsontext.Value("null")
		}
	}
	if in.Sender == in.Target {
		return patch, nil
	}
	handle := in.ReplyHandle
	if handle == "" {
		handle = "c:" + commentSuffix(in.ReplyUID)
	}
	value, err := json.Marshal(Value{From: in.Sender.Actor, Teammate: in.Sender.Teammate, Message: fmt.Sprintf("latest: %s %s by %s", in.Kind, handle, sender), Re: in.ReplyUID, Kind: in.Kind})
	if err != nil {
		return nil, err
	}
	recipients := []string{Address(in.Target.Actor, in.Target.Teammate)}
	if in.Kind == "confirm" {
		recipients = append(recipients, in.Owner, in.ParentOwner)
		seen := map[string]bool{}
		for _, identity := range in.PriorLinkers {
			recipient := Address(identity.Actor, identity.Teammate)
			if recipient == "" || recipient == sender || seen[recipient] {
				continue
			}
			seen[recipient] = true
			recipients = append(recipients, recipient)
			if len(seen) == 8 {
				break
			}
		}
	}
	for _, recipient := range recipients {
		if recipient == "" || recipient == sender {
			continue
		}
		if _, err := NormalizeRecipient(recipient); err != nil {
			continue
		}
		key := MetadataKey(recipient)
		if raw, ok := in.Current[key]; ok && string(raw) != "null" {
			var pending Value
			// Malformed values are also preserved: do not silently destroy requests.
			if json.Unmarshal(raw, &pending) != nil || pending.Re == "" {
				continue
			}
		}
		if in.Kind == "confirm" && in.PendingTargets[recipient] == in.TargetUID {
			continue
		}
		patch[key] = value
	}
	return patch, nil
}

func commentSuffix(uid string) string {
	if len(uid) > 6 {
		uid = uid[len(uid)-6:]
	}
	return uid
}
