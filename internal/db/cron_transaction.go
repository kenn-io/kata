package db

import (
	"context"
	"database/sql"
)

// Cron writes take the project lock before ordinary credential or
// host fences. PostgreSQL enrollment fences hold SHARE on project/binding;
// admitting two of those before the project UPDATE creates an upgrade deadlock.
// Preserve existing fence order after the project lock and leave legacy callers
// unchanged. The ordinary unfenced path locks in its domain transaction.
func (a CronSQL) transactProject(ctx context.Context, projectID int64, run func(*sql.Tx) error) error {
	if !HasTransactionFence(ctx) {
		return a.Transact(ctx, run)
	}
	original := ctx
	ctx = WithTransactionFence(ctx, func(ctx context.Context, tx Transaction) error {
		if err := LockCronProject(ctx, tx, projectID); err != nil {
			return err
		}
		return ApplyTransactionFence(original, tx)
	})
	return a.Transact(ctx, run)
}
