package pgstore

import (
	"database/sql/driver"
	"fmt"
	"time"
)

// storedTime adapts a NOT NULL TEXT timestamp column in both directions.
// Reads accept every layout parseStoredTime accepts; writes always emit
// storedTimeFormat, so read and write can no longer drift apart the way two
// free functions could. The ORDER BY created_at indexes depend on that.
type storedTime time.Time

func (t *storedTime) Scan(src any) error {
	text, err := storedText(src)
	if err != nil {
		return err
	}
	parsed, err := parseStoredTime(text)
	if err != nil {
		return err
	}
	*t = storedTime(parsed)
	return nil
}

func (t storedTime) Value() (driver.Value, error) {
	return formatStoredTime(time.Time(t)), nil
}

// storedNullTime adapts a nullable TEXT timestamp column in both directions.
// Scanning SQL NULL leaves Time nil rather than allocating a zero time.Time:
// the "deleted_at IS NULL" and "revoked_at IS NULL" semantics throughout
// federation and claims are expressed as a nil pointer downstream.
//
// This is a struct rather than a conversion over *time.Time because Go
// forbids methods on a type whose underlying type is a pointer.
type storedNullTime struct{ Time *time.Time }

func (t *storedNullTime) Scan(src any) error {
	if src == nil {
		t.Time = nil
		return nil
	}
	text, err := storedText(src)
	if err != nil {
		return err
	}
	parsed, err := parseStoredTime(text)
	if err != nil {
		return err
	}
	t.Time = &parsed
	return nil
}

func (t storedNullTime) Value() (driver.Value, error) {
	if t.Time == nil {
		return nil, nil
	}
	return formatStoredTime(*t.Time), nil
}

// storedBool adapts an INTEGER CHECK(value IN (0,1)) column in both
// directions. Anything outside that domain is a corrupt row, not a truthy
// value.
type storedBool bool

func (b *storedBool) Scan(src any) error {
	var number int64
	switch value := src.(type) {
	case int64:
		number = value
	case int32:
		number = int64(value)
	case int:
		number = int64(value)
	default:
		return fmt.Errorf("invalid stored boolean %T", src)
	}
	switch number {
	case 0:
		*b = false
	case 1:
		*b = true
	default:
		return fmt.Errorf("invalid stored boolean %d", number)
	}
	return nil
}

func (b storedBool) Value() (driver.Value, error) {
	if b {
		return int64(1), nil
	}
	return int64(0), nil
}

func storedText(src any) (string, error) {
	switch value := src.(type) {
	case string:
		return value, nil
	case []byte:
		return string(value), nil
	default:
		return "", fmt.Errorf("invalid stored timestamp source %T", src)
	}
}
