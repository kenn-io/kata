package db

import (
	"context"
	"time"
)

// ActorTokenTransactionFence keeps an admitted ordinary actor token live through
// the write transaction. Scoped credentials use their existing domain fence.
// An ordinary token may carry an expiry; a token's expiry never changes after
// issue, so the admitted value is the stored one; it is compared with the
// clock after the transaction holds the token row.
func ActorTokenTransactionFence(admitted APIToken) TransactionFence {
	return func(ctx context.Context, tx Transaction) error {
		if admitted.ID == 0 || admitted.Actor == "" || admitted.Scope != nil {
			return ErrNotFound
		}
		expiry := `expires_at IS NULL`
		if admitted.ExpiresAt != nil {
			expiry = `expires_at IS NOT NULL`
		}
		//nolint:gosec // expiry is one of two constant SQL fragments.
		result, err := tx.ExecContext(ctx, `UPDATE api_tokens SET actor = actor WHERE id = $1 AND actor = $2 AND revoked_at IS NULL AND scope_kind IS NULL AND `+expiry, admitted.ID, admitted.Actor)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrNotFound
		}
		// The UPDATE may have waited for another transaction's row lock, so
		// the expiry is compared only once this transaction holds the row.
		if admitted.ExpiresAt != nil && !time.Now().Before(*admitted.ExpiresAt) {
			return ErrNotFound
		}
		return nil
	}
}
