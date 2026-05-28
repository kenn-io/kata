package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	katauid "go.kenn.io/kata/internal/uid"
)

// CreateFederationEnrollmentParams carries the plaintext token at creation
// time only. The database stores only its SHA-256 hash.
type CreateFederationEnrollmentParams struct {
	Token            string
	SpokeInstanceUID string
	ProjectID        *int64
	Capabilities     string
}

// CreatedFederationEnrollment returns the created row plus the plaintext token
// so callers can display generated credentials exactly once.
type CreatedFederationEnrollment struct {
	Enrollment FederationEnrollment
	Token      string
}

// FederationTokenHash returns the SHA-256 hex digest used for enrollment token
// lookup. Plaintext federation tokens must never be persisted.
func FederationTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CanonicalFederationCapabilities validates and normalizes supported
// capabilities as sorted, de-duplicated comma-separated text.
func CanonicalFederationCapabilities(raw string) (string, error) {
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		capability := strings.TrimSpace(part)
		if capability == "" {
			return "", fmt.Errorf("empty federation capability")
		}
		if !isSupportedFederationCapability(capability) {
			return "", fmt.Errorf("unknown federation capability %q", capability)
		}
		seen[capability] = struct{}{}
	}
	if len(seen) == 0 {
		return "", fmt.Errorf("empty federation capability")
	}
	out := make([]string, 0, len(seen))
	for capability := range seen {
		out = append(out, capability)
	}
	sort.Strings(out)
	return strings.Join(out, ","), nil
}

// CreateFederationEnrollment inserts an active enrollment. When p.Token is
// empty, a fresh plaintext token is generated and returned without persisting.
func (d *DB) CreateFederationEnrollment(
	ctx context.Context,
	p CreateFederationEnrollmentParams,
) (CreatedFederationEnrollment, error) {
	if !katauid.Valid(p.SpokeInstanceUID) {
		return CreatedFederationEnrollment{}, fmt.Errorf("invalid spoke instance uid %q", p.SpokeInstanceUID)
	}
	capabilities, err := CanonicalFederationCapabilities(p.Capabilities)
	if err != nil {
		return CreatedFederationEnrollment{}, err
	}
	token := p.Token
	if token == "" {
		token, err = generateFederationToken()
		if err != nil {
			return CreatedFederationEnrollment{}, err
		}
	}
	var projectID any
	if p.ProjectID != nil {
		projectID = *p.ProjectID
	}
	res, err := d.ExecContext(ctx, `
		INSERT INTO federation_enrollments(token_hash, spoke_instance_uid, project_id, capabilities)
		VALUES(?, ?, ?, ?)`,
		FederationTokenHash(token), p.SpokeInstanceUID, projectID, capabilities)
	if err != nil {
		return CreatedFederationEnrollment{}, fmt.Errorf("create federation enrollment: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return CreatedFederationEnrollment{}, fmt.Errorf("federation enrollment last id: %w", err)
	}
	enrollment, err := d.federationEnrollmentByID(ctx, id)
	if err != nil {
		return CreatedFederationEnrollment{}, err
	}
	return CreatedFederationEnrollment{Enrollment: enrollment, Token: token}, nil
}

// ListFederationEnrollments returns every enrollment row ordered by id.
func (d *DB) ListFederationEnrollments(ctx context.Context) ([]FederationEnrollment, error) {
	rows, err := d.QueryContext(ctx, federationEnrollmentSelect+` ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list federation enrollments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []FederationEnrollment{}
	for rows.Next() {
		enrollment, err := scanFederationEnrollment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, enrollment)
	}
	return out, rows.Err()
}

// RevokeFederationEnrollment marks an enrollment inactive. Revocation is
// one-way; repeated calls leave the original revoked_at intact.
func (d *DB) RevokeFederationEnrollment(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `
		UPDATE federation_enrollments
		   SET revoked_at = COALESCE(revoked_at, strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("revoke federation enrollment: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke federation enrollment rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AuthorizeFederationToken returns the active enrollment matching token,
// project scope, capability, and an enabled hub binding on the target project.
func (d *DB) AuthorizeFederationToken(
	ctx context.Context,
	token string,
	projectID int64,
	capability string,
) (FederationEnrollment, error) {
	if token == "" {
		return FederationEnrollment{}, ErrNotFound
	}
	capability = strings.TrimSpace(capability)
	if !isSupportedFederationCapability(capability) {
		return FederationEnrollment{}, ErrNotFound
	}
	return scanFederationEnrollment(d.QueryRowContext(ctx, federationEnrollmentSelect+`
		 WHERE token_hash = ?
		   AND revoked_at IS NULL
		   AND instr(',' || capabilities || ',', ',' || ? || ',') > 0
		   AND (project_id = ? OR project_id IS NULL)
		   AND EXISTS (
		     SELECT 1
		       FROM federation_bindings
		       JOIN projects ON projects.id = federation_bindings.project_id
		      WHERE project_id = ?
		        AND projects.deleted_at IS NULL
		        AND role = 'hub'
		        AND enabled = 1
		   )`,
		FederationTokenHash(token), capability, projectID, projectID))
}

func isSupportedFederationCapability(capability string) bool {
	switch capability {
	case "claim", "pull", "push":
		return true
	default:
		return false
	}
}

func generateFederationToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate federation token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func (d *DB) federationEnrollmentByID(ctx context.Context, id int64) (FederationEnrollment, error) {
	return scanFederationEnrollment(d.QueryRowContext(ctx,
		federationEnrollmentSelect+` WHERE id = ?`, id))
}

const federationEnrollmentSelect = `SELECT id, token_hash, spoke_instance_uid, project_id,
       capabilities, created_at, updated_at, revoked_at
  FROM federation_enrollments`

func scanFederationEnrollment(r rowScanner) (FederationEnrollment, error) {
	var (
		e         FederationEnrollment
		projectID sql.NullInt64
		revokedAt sql.NullTime
	)
	err := r.Scan(&e.ID, &e.TokenHash, &e.SpokeInstanceUID, &projectID,
		&e.Capabilities, &e.CreatedAt, &e.UpdatedAt, &revokedAt)
	if err == nil {
		if projectID.Valid {
			v := projectID.Int64
			e.ProjectID = &v
		}
		if revokedAt.Valid {
			e.RevokedAt = &revokedAt.Time
		}
		return e, nil
	}
	if err == sql.ErrNoRows {
		return FederationEnrollment{}, ErrNotFound
	}
	return FederationEnrollment{}, fmt.Errorf("scan federation enrollment: %w", err)
}
