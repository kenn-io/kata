package mcpevents

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// ErrInvalidCursor marks a malformed or unauthenticated resume cursor.
var ErrInvalidCursor = errors.New("invalid MCP Events cursor")

// Cursor is a resume position within one subscription.
type Cursor struct {
	Incarnation          string
	ContinuityGeneration int64
	EventID              int64
}

const cursorMACBytes = 16

// EncodeCursor renders c1.<incarnation>.<continuity_generation>.<event_id>.<mac>.
// The MAC covers the subscription id and every serialized field.
func EncodeCursor(key []byte, subscriptionID string, c Cursor) string {
	cg := strconv.FormatInt(c.ContinuityGeneration, 10)
	id := strconv.FormatInt(c.EventID, 10)
	return "c1." + c.Incarnation + "." + cg + "." + id + "." + cursorMAC(key, subscriptionID, c.Incarnation, cg, id)
}

// DecodeCursor parses and authenticates raw. The MAC is verified for every
// incarnation, including ones other than the subscription's current one;
// deciding whether an authenticated incarnation is stale is the caller's job.
func DecodeCursor(key []byte, subscriptionID, raw string) (Cursor, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 5 || parts[0] != "c1" || !validIncarnation(parts[1]) {
		return Cursor{}, ErrInvalidCursor
	}
	cg, ok := canonicalDecimal(parts[2])
	if !ok {
		return Cursor{}, ErrInvalidCursor
	}
	id, ok := canonicalDecimal(parts[3])
	if !ok {
		return Cursor{}, ErrInvalidCursor
	}
	got, err := base64.RawURLEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(got) != cursorMACBytes {
		return Cursor{}, ErrInvalidCursor
	}
	want, err := base64.RawURLEncoding.Strict().DecodeString(cursorMAC(key, subscriptionID, parts[1], parts[2], parts[3]))
	if err != nil || !hmac.Equal(got, want) {
		return Cursor{}, ErrInvalidCursor
	}
	return Cursor{Incarnation: parts[1], ContinuityGeneration: cg, EventID: id}, nil
}

func cursorMAC(key []byte, subscriptionID, incarnation, cg, id string) string {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte("c1." + subscriptionID + "." + incarnation + "." + cg + "." + id))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:cursorMACBytes])
}

// canonicalDecimal accepts only digits with no sign and no leading zero,
// except "0" itself, within int64.
func canonicalDecimal(s string) (int64, bool) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// validIncarnation limits incarnations to a dot-free URL-safe alphabet.
func validIncarnation(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}
