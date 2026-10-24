package db

import (
	"bytes"
	"database/sql/driver"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// ScopedNullableBytes preserves SQL NULL versus an empty byte string on the
// owner-backup wire. json/v2 byte slices have base64 formats, not emitnull.
// This local codec changes no signature transcript or unrelated collection.
type ScopedNullableBytes []byte

func (b ScopedNullableBytes) MarshalJSON() ([]byte, error) {
	if b == nil {
		return []byte("null"), nil
	}
	return json.Marshal([]byte(b))
}
func (b *ScopedNullableBytes) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) {
		*b = nil
		return nil
	}
	if len(raw) == 0 || raw[0] != '"' {
		return errors.New("nullable scoped bytes require base64 string or null")
	}
	value := make([]byte, 0)
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*b = value
	return nil
}
func (b *ScopedNullableBytes) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*b = nil
	case []byte:
		*b = bytes.Clone(v)
	case string:
		*b = []byte(v)
	default:
		return fmt.Errorf("invalid SQL nullable scoped bytes %T", value)
	}
	return nil
}
func (b ScopedNullableBytes) Value() (driver.Value, error) {
	if b == nil {
		return nil, nil
	}
	return []byte(b), nil
}
