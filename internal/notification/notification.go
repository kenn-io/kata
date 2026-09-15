// Package notification owns the metadata contract shared by kata notify,
// kata inbox, and daemon-generated due-date attention requests.
package notification

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/kata/internal/metadata"
)

const (
	// KeyPrefix identifies metadata entries consumed by kata inbox.
	KeyPrefix = "notify."
	// RecipientMaxBytes limits the UTF-8 byte length of an inbox recipient.
	RecipientMaxBytes = 128
	// MessageMaxBytes limits the UTF-8 byte length of an inbox message.
	MessageMaxBytes = 1024
)

// Kind identifies the planning date that generated an automatic notification.
type Kind string

const (
	// Scheduled identifies a notification generated from scheduled_on.
	Scheduled Kind = "scheduled_on"
	// Deadline identifies a notification generated from deadline_on.
	Deadline Kind = "deadline_on"
)

// Value is the JSON payload stored in a notify.* metadata entry.
type Value struct {
	From     string `json:"from"`
	Teammate string `json:"teammate,omitempty"`
	Message  string `json:"message"`
}

// Automatic describes a system-generated notification recognized from metadata.
type Automatic struct {
	Recipient   string
	Kind        Kind
	SourceValue string
}

// NormalizeRecipient validates and canonicalizes an inbox recipient.
func NormalizeRecipient(raw string) (string, error) {
	recipient := strings.TrimSpace(raw)
	if recipient == "" {
		return "", errors.New("recipient must not be blank")
	}
	if !utf8.ValidString(recipient) || len(recipient) > RecipientMaxBytes {
		return "", errors.New("recipient must be valid UTF-8 and at most 128 bytes")
	}
	if strings.ContainsFunc(recipient, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}) {
		return "", errors.New("recipient must not contain control characters")
	}
	return recipient, nil
}

// MetadataKey returns the notify.* metadata key for a normalized recipient.
func MetadataKey(recipient string) string {
	return KeyPrefix + base64.RawURLEncoding.EncodeToString([]byte(recipient))
}

// MarshalSystem builds the exact payload for a reached planning date.
func MarshalSystem(kind Kind, sourceValue string) (json.RawMessage, error) {
	prefix, ok := messagePrefix(kind)
	if !ok || !validSourceValue(kind, sourceValue) {
		return nil, errors.New("invalid automatic notification source")
	}
	encoded, err := json.Marshal(Value{From: "system", Message: prefix + sourceValue})
	return json.RawMessage(encoded), err
}

// ParseAutomatic recognizes only exact system-generated notification payloads.
func ParseAutomatic(key string, raw json.RawMessage) (Automatic, bool) {
	recipient, ok := recipientFromKey(key)
	if !ok {
		return Automatic{}, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 2 {
		return Automatic{}, false
	}
	if _, ok := fields["from"]; !ok {
		return Automatic{}, false
	}
	if _, ok := fields["message"]; !ok {
		return Automatic{}, false
	}
	var value Value
	if json.Unmarshal(raw, &value) != nil || value.From != "system" || value.Teammate != "" {
		return Automatic{}, false
	}
	for _, kind := range []Kind{Scheduled, Deadline} {
		prefix, _ := messagePrefix(kind)
		sourceValue, found := strings.CutPrefix(value.Message, prefix)
		if found && validSourceValue(kind, sourceValue) {
			return Automatic{Recipient: recipient, Kind: kind, SourceValue: sourceValue}, true
		}
	}
	return Automatic{}, false
}

func recipientFromKey(key string) (string, bool) {
	encoded, ok := strings.CutPrefix(key, KeyPrefix)
	if !ok || encoded == "" {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	recipient, err := NormalizeRecipient(string(raw))
	return recipient, err == nil && MetadataKey(recipient) == key
}

func validSourceValue(kind Kind, value string) bool {
	encoded, err := json.Marshal(value)
	if err != nil {
		return false
	}
	return metadata.Validate(metadata.IssueRegistry, string(kind), encoded) == nil
}

func messagePrefix(kind Kind) (string, bool) {
	switch kind {
	case Scheduled:
		return "Scheduled date reached: ", true
	case Deadline:
		return "Deadline reached: ", true
	default:
		return "", false
	}
}
