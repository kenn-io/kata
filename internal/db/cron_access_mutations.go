package db

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/cron"
)

// CronPermissionError names the missing grant for clients while preserving the
// stable ErrCronPermissionDenied classification.
type CronPermissionError struct{ Grant string }

func (e *CronPermissionError) Error() string { return "cron permission denied: " + e.Grant }
func (e *CronPermissionError) Unwrap() error { return ErrCronPermissionDenied }

// RequireCronGrant evaluates ordinary authority within the caller's transaction.
// The caller holds the shared access epoch lock until commit.
func RequireCronGrant(ctx context.Context, tx Transaction, projectUID, actor, grant string) error {
	access, err := ReadCronActorAccess(ctx, tx, projectUID, actor)
	if err != nil {
		return err
	}
	allowed := grant == "define" && access.Define || grant == "observe" && access.Observe
	if !allowed {
		return &CronPermissionError{Grant: grant}
	}
	return nil
}

func (a CronSQL) checkCronGrant(ctx context.Context, tx *sql.Tx, projectUID string, check CronAccessCheck, grant string) error {
	if !check.Enforce {
		return nil
	}
	if a.LockAccess == nil {
		return errors.New("cron access lock unavailable")
	}
	if err := a.LockAccess(ctx, tx); err != nil {
		return err
	}
	return RequireCronGrant(ctx, tx, projectUID, check.Actor, grant)
}

func (a CronSQL) checkDefinitionGrant(ctx context.Context, tx *sql.Tx, projectUID string, req cronPutRequest, current cronRow, fresh bool) error {
	if !req.access.Enforce {
		return nil
	}
	required := true
	if req.job {
		next := req.definition.(cron.JobDefinition)
		var old *cron.JobDefinition
		if !fresh {
			previous, err := cron.DecodeJob(current.document)
			if err != nil {
				return err
			}
			old = &previous
		}
		required = cron.RequiresDefineGrant(old, &next)
		// Document comparison cannot exempt a rename, archive, or restoration.
		if old != nil && (old.Action.Kind == "execute" || next.Action.Kind == "execute") && (req.name != current.Name || req.deleted || current.DeletedAt != nil) {
			required = true
		}
	}
	if !required {
		return nil
	}
	return a.checkCronGrant(ctx, tx, projectUID, req.access, "define")
}
