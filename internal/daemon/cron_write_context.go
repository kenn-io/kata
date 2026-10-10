package daemon

import (
	"context"
	"errors"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// cronWriteContext applies ordinary actor attribution and credential
// revalidation. It adds no executor authority or scheduling permission.
func cronWriteContext(ctx context.Context, requested string) (context.Context, string, error) {
	actor, err := attributedActor(ctx, requested)
	if err != nil {
		return ctx, "", err
	}
	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.Kind != PrincipalDBToken {
		return ctx, actor, nil
	}
	fence := db.ActorTokenTransactionFence(db.APIToken{ID: principal.TokenID, Actor: principal.Actor, Scope: principal.Scope, ExpiresAt: principal.ExpiresAt})
	ctx = db.WithAdditionalTransactionFence(ctx, func(ctx context.Context, tx db.Transaction) error {
		if err := fence(ctx, tx); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return api.NewError(401, "unauthorized", "authentication required", "", nil)
			}
			return err
		}
		return nil
	})
	return ctx, actor, nil
}
