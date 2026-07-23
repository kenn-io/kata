package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// CreateFederationEnrollment inserts an active enrollment. When p.Token is
// empty, a fresh plaintext token is generated and returned without persisting.
func (d *Store) CreateFederationEnrollment(
	ctx context.Context,
	p db.CreateFederationEnrollmentParams,
) (db.CreatedFederationEnrollment, error) {
	prepared, err := db.PrepareFederationEnrollmentParams(p)
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	return retryWrite1(ctx, d, func() (db.CreatedFederationEnrollment, error) {
		return d.createFederationEnrollment(ctx, prepared)
	})
}

// CreateProjectFederationEnrollment enables one hub project and creates its
// scoped enrollment in the same transaction.
func (d *Store) CreateProjectFederationEnrollment(
	ctx context.Context,
	p db.CreateFederationEnrollmentParams,
) (db.CreatedFederationEnrollment, error) {
	prepared, err := db.PrepareFederationEnrollmentParams(p)
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	if prepared.Params.ProjectID == nil || *prepared.Params.ProjectID <= 0 {
		return db.CreatedFederationEnrollment{}, fmt.Errorf("project-scoped federation enrollment requires project id")
	}
	return retryWrite1(ctx, d, func() (db.CreatedFederationEnrollment, error) {
		return d.createProjectFederationEnrollment(ctx, prepared)
	})
}

// RotateFederationEnrollment revokes active grants for one spoke and project
// before inserting the caller-supplied replacement in the same transaction.
func (d *Store) RotateFederationEnrollment(
	ctx context.Context,
	p db.CreateFederationEnrollmentParams,
) (db.CreatedFederationEnrollment, error) {
	if p.Token == "" {
		return db.CreatedFederationEnrollment{}, fmt.Errorf("federation enrollment rotation requires replacement token")
	}
	prepared, err := db.PrepareFederationEnrollmentParams(p)
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	if prepared.Params.ProjectID == nil || *prepared.Params.ProjectID <= 0 {
		return db.CreatedFederationEnrollment{}, fmt.Errorf(
			"federation enrollment rotation requires project id",
		)
	}
	return retryWrite1(ctx, d, func() (db.CreatedFederationEnrollment, error) {
		return d.rotateFederationEnrollment(ctx, prepared)
	})
}

func (d *Store) createFederationEnrollment(
	ctx context.Context,
	prepared db.PreparedFederationEnrollment,
) (db.CreatedFederationEnrollment, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	defer func() { _ = tx.Rollback() }()
	created, err := createFederationEnrollmentTx(ctx, tx, prepared)
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	if err := tx.Commit(); err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	return created, nil
}

func (d *Store) rotateFederationEnrollment(
	ctx context.Context,
	prepared db.PreparedFederationEnrollment,
) (db.CreatedFederationEnrollment, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	defer func() { _ = tx.Rollback() }()

	p := prepared.Params
	existing, err := scanFederationEnrollment(tx.QueryRowContext(ctx,
		federationEnrollmentSelect+` WHERE token_hash = ?`, db.FederationTokenHash(p.Token)))
	matched := err == nil
	if matched {
		if !db.FederationEnrollmentMatchesCreate(existing, p) {
			return db.CreatedFederationEnrollment{}, db.ErrFederationEnrollmentTokenConflict
		}
	} else if !errors.Is(err, db.ErrNotFound) {
		return db.CreatedFederationEnrollment{}, err
	} else if d.rotationStage != nil {
		if err := d.rotationStage(ctx); err != nil {
			return db.CreatedFederationEnrollment{}, err
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE federation_enrollments
		   SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'),
		       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE revoked_at IS NULL
		   AND spoke_instance_uid = ?
		   AND project_id = ?
		   AND token_hash <> ?`,
		p.SpokeInstanceUID, *p.ProjectID, db.FederationTokenHash(p.Token)); err != nil {
		return db.CreatedFederationEnrollment{}, fmt.Errorf("revoke federation enrollments for rotation: %w", err)
	}
	if matched {
		if err := tx.Commit(); err != nil {
			return db.CreatedFederationEnrollment{}, err
		}
		return db.CreatedFederationEnrollment{Enrollment: existing, Token: p.Token}, nil
	}
	created, err := createFederationEnrollmentTx(ctx, tx, prepared)
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	if err := tx.Commit(); err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	return created, nil
}

func (d *Store) createProjectFederationEnrollment(
	ctx context.Context,
	prepared db.PreparedFederationEnrollment,
) (db.CreatedFederationEnrollment, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	defer func() { _ = tx.Rollback() }()
	p := prepared.Params
	if _, err := d.enableProjectFederationTx(ctx, tx, *p.ProjectID, p.Actor); err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	created, err := createFederationEnrollmentTx(ctx, tx, prepared)
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	if err := tx.Commit(); err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	return created, nil
}

func createFederationEnrollmentTx(
	ctx context.Context,
	tx *sql.Tx,
	prepared db.PreparedFederationEnrollment,
) (db.CreatedFederationEnrollment, error) {
	p := prepared.Params
	var projectID any
	if p.ProjectID != nil {
		projectID = *p.ProjectID
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO federation_enrollments(
		  token_hash, spoke_instance_uid, project_id, capabilities, bound_actor,
		  allow_adoption_snapshot_authors
		)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(token_hash) DO NOTHING`,
		db.FederationTokenHash(p.Token), p.SpokeInstanceUID, projectID, p.Capabilities, p.Actor,
		p.AllowAdoptionSnapshotAuthors)
	if err != nil {
		return db.CreatedFederationEnrollment{}, fmt.Errorf("create federation enrollment: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return db.CreatedFederationEnrollment{}, fmt.Errorf("federation enrollment rows affected: %w", err)
	}
	if inserted == 0 {
		if !prepared.ExplicitToken {
			return db.CreatedFederationEnrollment{}, fmt.Errorf("create federation enrollment: generated token collision")
		}
		enrollment, selectErr := scanFederationEnrollment(tx.QueryRowContext(ctx,
			federationEnrollmentSelect+` WHERE token_hash = ?`, db.FederationTokenHash(p.Token)))
		if selectErr != nil {
			return db.CreatedFederationEnrollment{}, selectErr
		}
		if !db.FederationEnrollmentMatchesCreate(enrollment, p) {
			return db.CreatedFederationEnrollment{}, db.ErrFederationEnrollmentTokenConflict
		}
		return db.CreatedFederationEnrollment{Enrollment: enrollment, Token: p.Token}, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return db.CreatedFederationEnrollment{}, fmt.Errorf("federation enrollment last id: %w", err)
	}
	enrollment, err := federationEnrollmentByIDTx(ctx, tx, id)
	if err != nil {
		return db.CreatedFederationEnrollment{}, err
	}
	return db.CreatedFederationEnrollment{Enrollment: enrollment, Token: p.Token}, nil
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

// FindActiveFederationEnrollment returns the newest active enrollment whose
// public correlation fields exactly match the request.
func (d *Store) FindActiveFederationEnrollment(
	ctx context.Context,
	p db.ActiveFederationEnrollmentParams,
) (db.FederationEnrollment, error) {
	return scanFederationEnrollment(d.QueryRowContext(ctx, federationEnrollmentSelect+`
		 WHERE project_id = ?
		   AND spoke_instance_uid = ?
		   AND capabilities = ?
		   AND bound_actor = ?
		   AND allow_adoption_snapshot_authors = ?
		   AND revoked_at IS NULL
		 ORDER BY id DESC
		 LIMIT 1`,
		p.ProjectID, p.SpokeInstanceUID, p.Capabilities, p.Actor,
		p.AllowAdoptionSnapshotAuthors))
}

// RevokeFederationEnrollment marks an enrollment inactive. Revocation is
// one-way; repeated calls leave the original revoked_at intact.
func (d *Store) RevokeFederationEnrollment(ctx context.Context, id int64) error {
	return d.RetryTransient(ctx, func() error {
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
	})
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
	enrollment, err := scanFederationEnrollment(d.QueryRowContext(ctx, federationEnrollmentSelect+`
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
	if err != nil {
		return db.FederationEnrollment{}, err
	}
	if enrollment.ProjectID == nil {
		enrollment.AllowAdoptionSnapshotAuthors = false
		enrollment.AdoptionBaselineOpen = false
		enrollment.AdoptionBaselineNextSourceEventID = 0
		enrollment.AdoptionBaselineEndSourceEventID = 0
	}
	return enrollment, nil
}

// FederationEnrollmentTransactionFence rechecks native credential authority
// in the same transaction as the protected mutation.
func (d *Store) FederationEnrollmentTransactionFence(
	admitted db.FederationEnrollment,
	projectID int64,
	capability string,
) db.TransactionFence {
	return func(ctx context.Context, transaction db.Transaction) error {
		current, err := scanFederationEnrollment(transaction.QueryRowContext(ctx,
			federationEnrollmentSelect+` WHERE id = ?`, admitted.ID))
		if err != nil {
			return err
		}
		if !db.FederationEnrollmentAuthorizationMatches(current, admitted, projectID, capability) {
			return db.ErrNotFound
		}
		var active int
		err = transaction.QueryRowContext(ctx, `
			SELECT binding.enabled
			FROM federation_bindings AS binding
			JOIN projects AS project ON project.id = binding.project_id
			WHERE binding.project_id = ? AND binding.role = 'hub'
			  AND binding.enabled = 1 AND project.deleted_at IS NULL`, projectID).Scan(&active)
		if errors.Is(err, sql.ErrNoRows) {
			return db.ErrNotFound
		}
		if err != nil {
			return err
		}
		if active != 1 {
			return db.ErrNotFound
		}
		return nil
	}
}

func federationEnrollmentByIDTx(ctx context.Context, tx *sql.Tx, id int64) (db.FederationEnrollment, error) {
	return scanFederationEnrollment(tx.QueryRowContext(ctx,
		federationEnrollmentSelect+` WHERE id = ?`, id))
}

const federationEnrollmentSelect = `SELECT id, token_hash, spoke_instance_uid, project_id,
       capabilities, bound_actor, allow_adoption_snapshot_authors,
       adoption_baseline_open, adoption_baseline_next_source_event_id,
       adoption_baseline_end_source_event_id,
       created_at, updated_at, revoked_at
  FROM federation_enrollments`

func scanFederationEnrollment(r rowScanner) (db.FederationEnrollment, error) {
	var (
		e         db.FederationEnrollment
		projectID sql.NullInt64
		allow     int
		open      int
		revokedAt sql.NullTime
	)
	err := r.Scan(&e.ID, &e.TokenHash, &e.SpokeInstanceUID, &projectID,
		&e.Capabilities, &e.Actor, &allow, &open,
		&e.AdoptionBaselineNextSourceEventID, &e.AdoptionBaselineEndSourceEventID,
		&e.CreatedAt, &e.UpdatedAt, &revokedAt)
	if err == nil {
		if projectID.Valid {
			v := projectID.Int64
			e.ProjectID = &v
		}
		e.AllowAdoptionSnapshotAuthors = allow != 0
		e.AdoptionBaselineOpen = open != 0
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
