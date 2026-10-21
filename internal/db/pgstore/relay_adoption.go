package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"slices"

	"go.kenn.io/kata/internal/db"
)

func (d *Store) adoptionSQL(tx db.Transaction) db.FederationAdoptionSQL {
	return db.FederationAdoptionSQL{Tx: tx, InstanceUID: d.InstanceUID()}
}
func (d *Store) adoptionTx(ctx context.Context, id int64, run func(*sql.Tx) error) error {
	original := ctx
	ctx = db.WithTransactionFence(ctx, func(c context.Context, tx db.Transaction) error {
		if err := lockProjectAccess(c, tx); err != nil {
			return err
		}
		if err := lockAdoptionProjectTx(c, tx, id); err != nil {
			return err
		}
		return db.ApplyTransactionFence(original, tx)
	})
	return d.relayTx(ctx, run)
}

func lockAdoptionProjectTx(ctx context.Context, tx db.Transaction, id int64) error {
	// Reuse the native project coordination write. An UPDATE also invalidates
	// older MVCC snapshots; a SELECT FOR UPDATE alone does not do that.
	return db.LockCronProject(ctx, tx, id)
}
func ensureAdoptionWritableTx(ctx context.Context, tx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) error {
	var found int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE id=$1 FOR SHARE`, id).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.ErrNotFound
		}
		return err
	}

	var marker string
	err := tx.QueryRowContext(ctx, `SELECT m.value FROM meta m JOIN projects p ON m.key='federation.adoption.v1/' || p.uid WHERE p.id=$1`, id).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return db.ErrFederationAdoptionPaused
}

// BeginFederationAdoption installs a durable source pause after draining native writers.
func (d *Store) BeginFederationAdoption(ctx context.Context, op db.FederationAdoptionOperation) (db.FederationAdoptionOperation, error) {
	var out db.FederationAdoptionOperation
	err := d.adoptionTx(ctx, op.ProjectID, func(tx *sql.Tx) error {
		var err error
		out, err = d.adoptionSQL(tx).Begin(ctx, op)
		return err
	})
	return out, err
}

// FederationAdoption reads the durable source operation.
func (d *Store) FederationAdoption(ctx context.Context, id int64) (db.FederationAdoptionOperation, error) {
	return d.adoptionSQL(d).Read(ctx, id)
}

// AdvanceFederationAdoption conditionally advances one exact operation.
func (d *Store) AdvanceFederationAdoption(ctx context.Context, expected, next db.FederationAdoptionOperation) error {
	return d.adoptionTx(ctx, expected.ProjectID, func(tx *sql.Tx) error {
		if next.Phase == "attached" || expected.BindingUID != "" && next.ResetEpoch != expected.ResetEpoch {
			binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, expected.ProjectID))
			if err != nil {
				return err
			}
			if !db.FederationAdoptionBindingMatches(next, binding) || binding.RelayConfig == nil {
				return db.ErrFederationAdoptionConflict
			}
			if expected.BindingUID != "" && next.ResetEpoch != expected.ResetEpoch {
				if err := d.adoptionSQL(tx).ValidateCredentialProof(ctx, next, binding); err != nil {
					return err
				}
			}
		}
		return d.adoptionSQL(tx).Advance(ctx, expected, next)
	})
}

// CancelUnusedFederationAdoption releases an exact unused source operation.
func (d *Store) CancelUnusedFederationAdoption(ctx context.Context, op db.FederationAdoptionOperation) error {
	return d.adoptionTx(ctx, op.ProjectID, func(tx *sql.Tx) error { return d.adoptionSQL(tx).Cancel(ctx, op) })
}

// FinalizeFederationAdoption releases a fully drained, credential-proven pause.
func (d *Store) FinalizeFederationAdoption(ctx context.Context, op db.FederationAdoptionOperation, expected db.FederationBinding) (bool, error) {
	released := false
	err := d.adoptionTx(ctx, op.ProjectID, func(tx *sql.Tx) error {
		released = false
		native := d.adoptionSQL(tx)
		current, err := native.Read(ctx, op.ProjectID)
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if current != op {
			return db.ErrFederationAdoptionConflict
		}
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, op.ProjectID))
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(binding, expected) {
			return db.ErrFederationAdoptionConflict
		}
		if err := native.ValidateFinalization(ctx, op, binding); err != nil {
			return err
		}
		protocol := d.relayReset(tx)
		if err := protocol.ValidateUpstreamWork(ctx, op.ProjectID, op.ProjectUID, binding.RelayConfig, nil); err != nil {
			return err
		}
		open, err := protocol.Backend.RelayQuarantineOpen(ctx, op.ProjectID)
		if err != nil {
			return err
		}
		if open {
			return db.ErrFederationResetBlockedByQuarantine
		}
		if err := native.Release(ctx, op); err != nil {
			return err
		}
		released = true
		return nil
	})
	return released, err
}

// All endpoint projects are checked in ascending order before relationship writes.
func ensureAdoptionProjectsTx(ctx context.Context, tx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, ids ...int64) error {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	for _, id := range slices.Compact(ids) {
		if err := ensureAdoptionWritableTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}
func ensureAdoptionIssuesTx(ctx context.Context, tx *sql.Tx, ids ...int64) error {
	projects := make([]int64, 0, len(ids))
	for _, id := range ids {
		var project int64
		err := tx.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE id=$1`, id).Scan(&project)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		projects = append(projects, project)
	}
	return ensureAdoptionProjectsTx(ctx, tx, projects...)
}
func ensureAdoptionAuthorRewriteTx(ctx context.Context, tx *sql.Tx, id int64, author string) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT i.project_id FROM links l JOIN issues i ON i.id=l.to_issue_id JOIN issues f ON f.id=l.from_issue_id WHERE f.project_id=$1 AND l.author=$2 ORDER BY i.project_id`, id, author)
	if err != nil {
		return err
	}
	ids := []int64{id}
	for rows.Next() {
		var peer int64
		if err := rows.Scan(&peer); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, peer)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, project := range ids {
		if err := db.LockCronProject(ctx, tx, project); err != nil {
			return err
		}
	}
	return ensureAdoptionProjectsTx(ctx, tx, ids...)
}

func ensureAdoptionBackgroundTx(ctx context.Context, tx *sql.Tx, table string, id int64) error {
	if table != "issue_sync_bindings" && table != "external_root_bindings" {
		return db.ErrFederationAdoptionConflict
	}
	var project int64
	// #nosec G202 -- table is restricted to the two native binding tables above.
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM `+table+` WHERE id=$1`, id).Scan(&project); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.ErrNotFound
		}
		return err
	}
	return ensureAdoptionWritableTx(ctx, tx, project)
}

// Dedicated ingress/materialization may proceed only against the current hop.
func (d *Store) checkAdoptionSyncTx(ctx context.Context, tx *sql.Tx, id int64) error {
	var found int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE id=$1 FOR SHARE`, id).Scan(&found); err != nil {
		return err
	}

	native := d.adoptionSQL(tx)
	op, err := native.Read(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, id))
	if err != nil {
		return db.ErrFederationAdoptionPaused
	}
	if binding.RelayConfig == nil {
		return native.CheckAttachment(ctx, id, &binding)
	}
	return native.ValidateSyncBinding(ctx, op, binding, d.relayReset(tx))
}

type adoptionQueryer interface {
	db.Transaction
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func lockAdoptionProjectsTx(ctx context.Context, tx db.Transaction, ids ...int64) error {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, id := range ids {
		if err := db.LockCronProject(ctx, tx, id); err != nil {
			return err
		}
	}
	return ensureAdoptionProjectsTx(ctx, tx, ids...)
}

// Lifecycle changes can hide or delete a frozen project's relationship peer.
func lockAdoptionLifecycleTx(ctx context.Context, tx adoptionQueryer, ids ...int64) error {
	affected := slices.Clone(ids)
	for _, id := range ids {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT f.project_id,t.project_id FROM links l JOIN issues f ON f.id=l.from_issue_id JOIN issues t ON t.id=l.to_issue_id WHERE f.project_id=$1 OR t.project_id=$1`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var from, to int64
			if err := rows.Scan(&from, &to); err != nil {
				_ = rows.Close()
				return err
			}
			affected = append(affected, from, to)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
	}
	return lockAdoptionProjectsTx(ctx, tx, affected...)
}
func lockAdoptionMoveTx(ctx context.Context, tx *sql.Tx, from, to, issue int64) error {
	affected := []int64{from, to}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT f.project_id,t.project_id FROM links l JOIN issues f ON f.id=l.from_issue_id JOIN issues t ON t.id=l.to_issue_id WHERE f.id=$1 OR t.id=$1`, issue)
	if err != nil {
		return err
	}
	for rows.Next() {
		var from, to int64
		if err := rows.Scan(&from, &to); err != nil {
			_ = rows.Close()
			return err
		}
		affected = append(affected, from, to)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	return lockAdoptionProjectsTx(ctx, tx, affected...)
}
