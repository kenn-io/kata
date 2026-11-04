package sqlitestore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

// CronAccessPolicy reads daemon-local cron authority.
func (s *Store) CronAccessPolicy(ctx context.Context, projectUID string) (db.ProjectCronAccessPolicy, error) {
	return db.ReadCronAccessPolicy(ctx, s.readQ, projectUID)
}

// SetCronAccessPolicy serializes CAS, epoch invalidation and attributed audit.
func (s *Store) SetCronAccessPolicy(ctx context.Context, policy db.ProjectCronAccessPolicy, actor string) (db.ProjectCronAccessPolicy, db.Event, error) {
	if err := validateProjectAccessAdmin(actor); err != nil {
		return db.ProjectCronAccessPolicy{}, db.Event{}, err
	}
	var result db.ProjectCronAccessPolicy
	var event db.Event
	err := s.withProjectAccessTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = db.SetCronAccessPolicyTx(ctx, tx, policy)
		if err != nil {
			return err
		}
		if err := bumpProjectAccess(ctx, tx); err != nil {
			return err
		}
		event, err = s.projectAccessAudit(ctx, tx, "project.cron_access_changed", actor, result)
		return err
	})
	if err != nil {
		return db.ProjectCronAccessPolicy{}, db.Event{}, err
	}
	return result, event, nil
}

// CronActorAccess reads a consistent policy and membership snapshot under the
// shared epoch lock. Callers separately intersect credential write authority.
func (s *Store) CronActorAccess(ctx context.Context, projectUID, actor string) (db.CronAccess, error) {
	var result db.CronAccess
	err := s.withCronAccessReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = db.ReadCronActorAccess(ctx, tx, projectUID, actor)
		return err
	})
	return result, err
}
func (s *Store) withCronAccessReadTx(ctx context.Context, op func(*sql.Tx) error) error {
	return s.withProjectAccessTx(ctx, op)
}
