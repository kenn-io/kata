package sqlitestore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

// CreateFederationEnrollment inserts an active enrollment. When p.Token is
// empty, a fresh plaintext token is generated and returned without persisting.
func (d *Store) CreateFederationEnrollment(
	ctx context.Context,
	p db.CreateFederationEnrollmentParams,
) (db.CreatedFederationEnrollment, error) {
	if !katauid.Valid(p.SpokeInstanceUID) {
		return db.CreatedFederationEnrollment{}, fmt.Errorf("invalid spoke instance uid %q", p.SpokeInstanceUID)
	}
	capabilities, err := db.CanonicalFederationCapabilities(p.Capabilities)
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	token := p.Token
	if token == "" {
		token, err = generateFederationToken()
		if err != nil {
			return db.CreatedFederationEnrollment{}, err
		}
	}
	var projectID any
	if p.ProjectID != nil {
		projectID = *p.ProjectID
	}
	res, err := d.ExecContext(ctx, `
		INSERT INTO federation_enrollments(token_hash, spoke_instance_uid, project_id, capabilities)
		VALUES(?, ?, ?, ?)`,
		db.FederationTokenHash(token), p.SpokeInstanceUID, projectID, capabilities)
	if err != nil {
		return db.CreatedFederationEnrollment{}, fmt.Errorf("create federation enrollment: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return db.CreatedFederationEnrollment{}, fmt.Errorf("federation enrollment last id: %w", err)
	}
	enrollment, err := d.federationEnrollmentByID(ctx, id)
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	return db.CreatedFederationEnrollment{Enrollment: enrollment, Token: token}, nil
}

// ListFederationEnrollments returns every enrollment row ordered by id.
func (d *Store) ListFederationEnrollments(ctx context.Context) ([]db.FederationEnrollment, error) {
	rows, err := d.QueryContext(ctx, federationEnrollmentSelect+` ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list federation enrollments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []db.FederationEnrollment{}
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
func (d *Store) RevokeFederationEnrollment(ctx context.Context, id int64) error {
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
		return db.ErrNotFound
	}
	return nil
}

// AuthorizeFederationToken returns the active enrollment matching token,
// project scope, capability, and an enabled hub binding on the target project.
func (d *Store) AuthorizeFederationToken(
	ctx context.Context,
	token string,
	projectID int64,
	capability string,
) (db.FederationEnrollment, error) {
	if token == "" {
		return db.FederationEnrollment{}, db.ErrNotFound
	}
	capability = strings.TrimSpace(capability)
	if !db.IsSupportedFederationCapability(capability) {
		return db.FederationEnrollment{}, db.ErrNotFound
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
		db.FederationTokenHash(token), capability, projectID, projectID))
}

func generateFederationToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate federation token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func (d *Store) federationEnrollmentByID(ctx context.Context, id int64) (db.FederationEnrollment, error) {
	return scanFederationEnrollment(d.QueryRowContext(ctx,
		federationEnrollmentSelect+` WHERE id = ?`, id))
}

const federationEnrollmentSelect = `SELECT id, token_hash, spoke_instance_uid, project_id,
       capabilities, created_at, updated_at, revoked_at
  FROM federation_enrollments`

func scanFederationEnrollment(r rowScanner) (db.FederationEnrollment, error) {
	var (
		e         db.FederationEnrollment
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
		return db.FederationEnrollment{}, db.ErrNotFound
	}
	return db.FederationEnrollment{}, fmt.Errorf("scan federation enrollment: %w", err)
}
