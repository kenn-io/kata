package metadata

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// NormalizeJSON removes whitespace and sorts object keys while preserving
// numeric literals exactly, including integers beyond float64 precision.
// Invalid input is returned unchanged.
func NormalizeJSON(raw jsontext.Value) []byte {
	var value any
	if err := json.Unmarshal(raw, &value, json.WithUnmarshalers(json.UnmarshalFromFunc(func(dec *jsontext.Decoder, value *any) error {
		if dec.PeekKind() != '0' {
			return errors.ErrUnsupported
		}
		number, err := dec.ReadValue()
		*value = number.Clone()
		return err
	}))); err != nil {
		return raw
	}
	out, err := json.Marshal(value, json.Deterministic(true), jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
	if err != nil {
		return raw
	}
	return out
}

// KeyDiff records the before/after JSON values for one metadata key.
// From is nil when the key was absent (or null) in the old blob.
// To is nil when the key was removed (or set to null) in the new blob.
type KeyDiff struct {
	From jsontext.Value // nil means "was absent/null"
	To   jsontext.Value // nil means "now absent/null"
}

// Diff computes the per-key changes between oldBlob and newBlob, both of
// which must be JSON objects (or null/empty, treated as {}).
// Only keys that actually changed are included in the returned map.
// A key that goes from absent/null to absent/null is suppressed (no-op).
func Diff(oldBlob, newBlob jsontext.Value) (map[string]KeyDiff, error) {
	oldMap, err := parseMetaBlob(oldBlob)
	if err != nil {
		return nil, fmt.Errorf("parsing old metadata: %w", err)
	}
	newMap, err := parseMetaBlob(newBlob)
	if err != nil {
		return nil, fmt.Errorf("parsing new metadata: %w", err)
	}

	result := make(map[string]KeyDiff)

	// Keys present in old.
	for k, oldVal := range oldMap {
		newVal, inNew := newMap[k]
		if !inNew || isNull(newVal) {
			if isNull(oldVal) {
				continue // null → null: no-op
			}
			result[k] = KeyDiff{From: oldVal, To: nil}
			continue
		}
		if !bytes.Equal(NormalizeJSON(oldVal), NormalizeJSON(newVal)) {
			// Normalize null From to nil: contract says From==nil means
			// "was absent or null", so raw `null` bytes must not leak out.
			var from jsontext.Value
			if !isNull(oldVal) {
				from = oldVal
			}
			result[k] = KeyDiff{From: from, To: newVal}
		}
	}

	// Keys present only in new.
	for k, newVal := range newMap {
		if _, inOld := oldMap[k]; inOld {
			continue // already handled above
		}
		if isNull(newVal) {
			continue // absent → null: no-op
		}
		result[k] = KeyDiff{From: nil, To: newVal}
	}

	return result, nil
}

// parseMetaBlob decodes a JSON object blob into a raw-value map.
// Null and empty input are treated as an empty object.
func parseMetaBlob(blob jsontext.Value) (map[string]jsontext.Value, error) {
	if len(blob) == 0 || string(blob) == "null" {
		return map[string]jsontext.Value{}, nil
	}
	var m map[string]jsontext.Value
	if err := json.Unmarshal(blob, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return map[string]jsontext.Value{}, nil
	}
	return m, nil
}

// isNull reports whether raw is a JSON null literal.
func isNull(raw jsontext.Value) bool {
	return string(raw) == "null"
}
