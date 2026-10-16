package notification

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
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
	Sender, Target                                Identity
	ReplyUID, TargetUID, Kind, Owner, ParentOwner string
	PriorLinkers                                  []Identity
	Current                                       map[string]jsontext.Value
	PendingTargets                                map[string]string
}

// RecipientOf returns the normalized inbox address of an identity: the form
// inbox, wait, and notify --to use. Actors are stored as written, so every
// slot key derived from a stored identity goes through here. It returns ""
// for an identity no inbox can address.
func RecipientOf(id Identity) string {
	recipient, err := NormalizeRecipient(Address(strings.TrimSpace(id.Actor), strings.TrimSpace(id.Teammate)))
	if err != nil {
		return ""
	}
	return recipient
}

// LinkValue is the slot value that reports one typed reply. The message names
// the reply by its shortest possible handle; inbox resolves the current handle
// from Re when it lists the slot.
func LinkValue(sender Identity, replyUID, kind string) (jsontext.Value, error) {
	handle := strings.ToLower(replyUID)
	if len(handle) > 6 {
		handle = handle[len(handle)-6:]
	}
	return json.Marshal(Value{
		From: sender.Actor, Teammate: sender.Teammate, Re: replyUID, Kind: kind,
		Message: fmt.Sprintf("latest: %s c:%s by %s", kind, handle, Address(sender.Actor, sender.Teammate)),
	})
}

// LinkPatch computes one explicit metadata patch. Replay never calls it.
func LinkPatch(in LinkInput) (map[string]jsontext.Value, error) {
	patch := map[string]jsontext.Value{}
	sender := RecipientOf(in.Sender)
	// Unaddressable identities all normalize to "", so only a real address
	// can match the target's.
	if in.Sender == in.Target || (sender != "" && sender == RecipientOf(in.Target)) {
		return patch, nil
	}
	value, err := LinkValue(in.Sender, in.ReplyUID, in.Kind)
	if err != nil {
		return nil, err
	}
	recipients := []string{RecipientOf(in.Target)}
	if in.Kind == "confirm" {
		recipients = append(recipients, RecipientOf(Identity{Actor: in.Owner}), RecipientOf(Identity{Actor: in.ParentOwner}))
		seen := map[string]bool{}
		for _, identity := range in.PriorLinkers {
			recipient := RecipientOf(identity)
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
		key := MetadataKey(recipient)
		if !linkSlotWritable(in.Current, key) {
			continue
		}
		if in.Kind == "confirm" && in.PendingTargets[recipient] == in.TargetUID {
			continue
		}
		patch[key] = value
	}
	return patch, nil
}

// SettlePatch answers the sender's request in one issue's slots. Any typed
// reply to the comment a request names settles that request. The requester
// then learns of the answer on the issue where they asked, which is where
// kata wait --until reply watches. Replay never calls it.
func SettlePatch(slots map[string]jsontext.Value, sender Identity, answeredUID string, link jsontext.Value) map[string]jsontext.Value {
	patch := map[string]jsontext.Value{}
	senderAddress := RecipientOf(sender)
	if senderAddress == "" {
		return patch
	}
	key := MetadataKey(senderAddress)
	var request Value
	if json.Unmarshal(slots[key], &request) != nil || request.Re == "" || request.Re != answeredUID {
		return patch
	}
	patch[key] = jsontext.Value("null")
	// A slot with a kind reports an earlier reply; nobody is waiting on it.
	if request.Kind != "" || request.From == "" {
		return patch
	}
	requester := RecipientOf(Identity{Actor: request.From, Teammate: request.Teammate})
	if requester == "" || requester == senderAddress {
		return patch
	}
	if requesterKey := MetadataKey(requester); linkSlotWritable(slots, requesterKey) {
		patch[requesterKey] = link
	}
	return patch
}

// linkSlotWritable reports whether a reply notice may replace a slot. Only an
// earlier reply notice, which always has a kind, is replaced. Requests, with
// or without a comment reference, and malformed values are preserved: do not
// silently destroy them.
func linkSlotWritable(slots map[string]jsontext.Value, key string) bool {
	raw, ok := slots[key]
	if !ok || string(raw) == "null" {
		return true
	}
	var pending Value
	return json.Unmarshal(raw, &pending) == nil && pending.Kind != ""
}
