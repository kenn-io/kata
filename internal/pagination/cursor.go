// Package pagination provides opaque, filter-bound issue continuation markers.
// A cursor conveys a position, never authorization; callers recheck authority.
package pagination

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"time"
)

// Position orders rows by creation time and their database-local tie-breaker.
type Position struct {
	CreatedAt time.Time `json:"created_at"`
	ID        int64     `json:"id"`
}

type cursor struct {
	Version int `json:"v"`
	Position
	Filter string `json:"filter"`
}

// Fingerprint binds a cursor to a caller's JSON-serializable effective filters.
func Fingerprint(filters any) string {
	// Preserve stable key ordering and the existing nil-versus-empty scope
	// distinction while retaining the prior handling of query string bytes.
	data, err := json.Marshal(filters, json.Deterministic(true), json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true), jsontext.AllowInvalidUTF8(true))
	if err != nil {
		panic("pagination filters must be JSON serializable")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Encode returns the canonical opaque representation of a continuation.
func Encode(p Position, hash string) string {
	p.CreatedAt = p.CreatedAt.UTC()
	data, _ := json.Marshal(cursor{Version: 1, Position: p, Filter: hash})
	return base64.RawURLEncoding.EncodeToString(data)
}

// Decode rejects malformed markers and changes to the bound filter set.
// Empty means the first page. The size bound applies before decoding.
func Decode(raw, hash string) (*Position, error) {
	if raw == "" {
		return nil, nil
	}
	invalid := errors.New("invalid cursor or changed filters; restart without a cursor")
	if len(raw) > 4096 {
		return nil, invalid
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil {
		return nil, invalid
	}
	var c cursor
	if err := json.Unmarshal(data, &c, json.RejectUnknownMembers(true)); err != nil {
		return nil, invalid
	}
	if c.Version != 1 || c.ID <= 0 || c.CreatedAt.IsZero() || c.CreatedAt.Year() < 1 || c.CreatedAt.Year() > 9999 || c.Filter != hash || len(hash) != 64 || Encode(c.Position, c.Filter) != raw {
		return nil, invalid
	}
	return &c.Position, nil
}
