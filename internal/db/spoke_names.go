package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	katauid "go.kenn.io/kata/internal/uid"
)

// FederationSpokeNamePrefix stores hub-local display labels in existing metadata.
const FederationSpokeNamePrefix = "federation_spoke_name."

// ErrFederationSpokeNameValidation identifies invalid or conflicting labels.
var ErrFederationSpokeNameValidation = errors.New("invalid federation spoke name")
var spokeNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidateFederationSpokeName is shared by live writes and backup replay.
func ValidateFederationSpokeName(uid, name string) error {
	if !katauid.Valid(uid) {
		return fmt.Errorf("%w: invalid instance UID %q", ErrFederationSpokeNameValidation, uid)
	}
	if !spokeNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q must match [a-z][a-z0-9-]{0,31}", ErrFederationSpokeNameValidation, name)
	}
	return nil
}

// SpokeNameReader is the common database/sql read surface used by both backends.
type SpokeNameReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// ReadFederationSpokeNames loads labels in one query for an entire response.
func ReadFederationSpokeNames(ctx context.Context, q SpokeNameReader) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT key,value FROM meta WHERE substr(key,1,$1)=$2`, len(FederationSpokeNamePrefix), FederationSpokeNamePrefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	names := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		names[strings.TrimPrefix(key, FederationSpokeNamePrefix)] = value
	}
	return names, rows.Err()
}

// WriteFederationSpokeName runs inside a backend-serialized naming transaction.
// An empty name clears the label; it never changes the enrollment itself.
func WriteFederationSpokeName(ctx context.Context, tx *sql.Tx, uid, name string) error {
	if name != "" {
		if err := ValidateFederationSpokeName(uid, name); err != nil {
			return err
		}
	} else if !katauid.Valid(uid) {
		return fmt.Errorf("%w: invalid instance UID %q", ErrFederationSpokeNameValidation, uid)
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_enrollments WHERE spoke_instance_uid=$1)`, uid).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	key := FederationSpokeNamePrefix + uid
	if name == "" {
		_, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key=$1`, key)
		return err
	}
	names, err := ReadFederationSpokeNames(ctx, tx)
	if err != nil {
		return err
	}
	for holder, taken := range names {
		if holder != uid && strings.EqualFold(taken, name) {
			return fmt.Errorf("%w: %q is held by spoke %s", ErrFederationSpokeNameValidation, name, holder)
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, name)
	return err
}

func validateSpokeNameReplay(records []ImportRecord) error {
	enrolled := make(map[string]bool)
	for _, record := range records {
		if e, ok := record.(*FederationEnrollmentExport); ok {
			enrolled[e.SpokeInstanceUID] = true
		}
	}
	names := make(map[string]string)
	seen := make(map[string]bool)
	for _, record := range records {
		m, ok := record.(*MetaKV)
		if !ok || !strings.HasPrefix(m.Key, FederationSpokeNamePrefix) {
			continue
		}
		uid := strings.TrimPrefix(m.Key, FederationSpokeNamePrefix)
		if err := ValidateFederationSpokeName(uid, m.Value); err != nil {
			return err
		}
		if !enrolled[uid] {
			return fmt.Errorf("%w: spoke %s has no enrollment", ErrFederationSpokeNameValidation, uid)
		}
		if seen[uid] {
			return fmt.Errorf("%w: repeated name for spoke %s", ErrFederationSpokeNameValidation, uid)
		}
		seen[uid] = true
		key := strings.ToLower(m.Value)
		if holder, ok := names[key]; ok && holder != uid {
			return fmt.Errorf("%w: %q is held by spoke %s", ErrFederationSpokeNameValidation, m.Value, holder)
		}
		names[key] = uid
	}
	return nil
}
