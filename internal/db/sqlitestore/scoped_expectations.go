package sqlitestore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// PrepareMutation resolves native command IDs in this transaction, then uses
// the same live private preparation core as the stable DTO entrypoint.
func (s *Store) PrepareMutation(ctx context.Context, command db.ScopedCommand) (db.MutationExpectation, error) {
	var result db.MutationExpectation
	if _, ok := db.ScopedSourceAccessFromContext(ctx); !ok {
		return result, db.ErrNotFound
	}
	err := s.relayTx(ctx, func(tx *sql.Tx) error {
		result = db.MutationExpectation{}
		preparation, err := db.ScopedCommandPreparationTx(ctx, tx, command)
		if err != nil {
			return err
		}
		result, err = s.prepareScopedMutationTx(ctx, tx, preparation, &command)
		return err
	})
	if err != nil {
		return db.MutationExpectation{}, scopedPreparationError(err)
	}
	return result, nil
}

// PrepareScopedMutation is the stable in-process DTO path used by native
// preparation adapters when native write parameters lack a conditional guard.
// It is not a wire route, and supplies no authority of its own.
func (s *Store) PrepareScopedMutation(ctx context.Context, preparation db.ScopedMutationPreparation) (db.MutationExpectation, error) {
	var result db.MutationExpectation
	if _, ok := db.ScopedSourceAccessFromContext(ctx); !ok {
		return result, db.ErrNotFound
	}
	err := s.relayTx(ctx, func(tx *sql.Tx) error {
		result = db.MutationExpectation{}
		var err error
		result, err = s.prepareScopedMutationTx(ctx, tx, preparation, nil)
		return err
	})
	if err != nil {
		return db.MutationExpectation{}, scopedPreparationError(err)
	}
	return result, nil
}

func scopedPreparationError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return db.ErrNotFound
	}
	return err
}

func (s *Store) prepareScopedMutationTx(ctx context.Context, tx *sql.Tx, preparation db.ScopedMutationPreparation, command *db.ScopedCommand) (db.MutationExpectation, error) {
	access, ok := db.ScopedSourceAccessFromContext(ctx)
	if !ok {
		return db.MutationExpectation{}, db.ErrNotFound
	}
	// The transport account must independently cover the full selected view.
	// Keep the original caller ceiling for the actual subjects in the core.
	view, err := s.fenceScopedAuthorityTx(db.WithAuthorizedProjects(ctx, nil), tx, access.View, "push")
	if err != nil {
		return db.MutationExpectation{}, err
	}
	if s.instanceUID != view.AuthorityUID || access.Principal.AuthorityUID != view.AuthorityUID || access.Principal.RouteDigest != view.RouteDigest {
		return db.MutationExpectation{}, db.ErrNotFound
	}
	hop, position, err := scopedHop(view, s.instanceUID)
	if err != nil || position != -1 || hop.BindingUID != access.Principal.BindingUID || hop.Epoch != access.Principal.Epoch {
		return db.MutationExpectation{}, db.ErrNotFound
	}
	grant, err := scanFederationEnrollment(tx.QueryRowContext(ctx, federationEnrollmentSelect+` WHERE relay_binding_uid=$1`, hop.BindingUID))
	if err != nil {
		return db.MutationExpectation{}, err
	}
	if grant.Actor != access.Principal.AccountableActor {
		return db.MutationExpectation{}, db.ErrNotFound
	}
	return db.PrepareScopedExpectationTx(ctx, tx, view, access.Principal, preparation, command, false)
}
