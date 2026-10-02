package db

import (
	"fmt"
	"time"
)

// MetaKeyInstanceCreatedAt is the meta key holding when meta.instance_uid was
// first generated. Databases whose instance_uid predates it have no row.
const MetaKeyInstanceCreatedAt = "instance_created_at"

// FormatInstanceCreatedAt renders an instance creation time for meta.
func FormatInstanceCreatedAt(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// ParseInstanceCreatedAt reads a meta.instance_created_at value.
func ParseInstanceCreatedAt(value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %s: %w", MetaKeyInstanceCreatedAt, err)
	}
	return t, nil
}
