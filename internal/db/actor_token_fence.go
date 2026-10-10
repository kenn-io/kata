package db

import "context"

// ActorTokenTransactionFence keeps an admitted ordinary actor token live through
// the write transaction. Scoped credentials use their existing domain fence.
func ActorTokenTransactionFence(admitted APIToken) TransactionFence {
	return func(ctx context.Context, tx Transaction) error {
		if admitted.ID == 0 || admitted.Actor == "" || admitted.Scope != nil || admitted.ExpiresAt != nil {
			return ErrNotFound
		}
		result, err := tx.ExecContext(ctx, `UPDATE api_tokens SET actor = actor WHERE id = $1 AND actor = $2 AND revoked_at IS NULL AND scope_kind IS NULL AND expires_at IS NULL`, admitted.ID, admitted.Actor)
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
		return nil
	}
}
