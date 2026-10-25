package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"go.kenn.io/kata/internal/db"
)

func (s *Store) scopedSourceAuthorityTx(ctx context.Context, tx *sql.Tx, principal db.CustodyPrincipal, submission db.SourceSubmission) (db.ScopedViewDescriptor, *db.CustodyPrincipal, error) {
	return s.scopedMemberAuthorityTx(ctx, tx, principal, submission, "push")
}

func (s *Store) scopedMemberAuthorityTx(ctx context.Context, tx *sql.Tx, principal db.CustodyPrincipal, submission db.SourceSubmission, capability string) (db.ScopedViewDescriptor, *db.CustodyPrincipal, error) {
	if capability != "push" && capability != "claim" {
		return db.ScopedViewDescriptor{}, nil, db.ErrNotFound
	}
	if err := principal.Validate(); err != nil {
		return db.ScopedViewDescriptor{}, nil, db.ErrNotFound
	}
	rows, err := tx.QueryContext(ctx, `SELECT g.view_uid,g.generation FROM federation_scoped_generations g JOIN federation_scoped_views v ON v.view_uid=g.view_uid WHERE v.authority_uid=$1 AND v.route_digest=$2 AND g.state IN ('active','prepared') AND v.state IN ('active','prepared') ORDER BY g.view_uid,g.generation DESC`, principal.AuthorityUID, principal.RouteDigest)
	if err != nil {
		return db.ScopedViewDescriptor{}, nil, err
	}
	var keys []db.ScopedViewKey
	for rows.Next() {
		var key db.ScopedViewKey
		if err := rows.Scan(&key.ViewUID, &key.Generation); err != nil {
			_ = rows.Close()
			return db.ScopedViewDescriptor{}, nil, err
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return db.ScopedViewDescriptor{}, nil, err
	}
	if capability == "push" {
		if access, explicit := db.ScopedSourceAccessFromContext(ctx); explicit {
			if access.Principal != principal {
				return db.ScopedViewDescriptor{}, nil, db.ErrNotFound
			}
			found := false
			for _, key := range keys {
				found = found || key == access.View
			}
			if !found {
				return db.ScopedViewDescriptor{}, nil, db.ErrNotFound
			}
			keys = []db.ScopedViewKey{access.View}
		} else if len(keys) > 1 {
			// A route/member subset cannot choose among sibling views. The
			// explicit hint supplies no authority; all native fences still run.
			return db.ScopedViewDescriptor{}, nil, db.ErrScopedSourcePending
		}
	}
	for _, key := range keys {
		view, _, err := s.scopedDescriptorTx(ctx, tx, key)
		if err != nil {
			continue
		}
		hop, position, err := scopedHop(view, s.instanceUID)
		if err != nil || position+1 == len(view.Route) || hop.BindingUID != principal.BindingUID || hop.Epoch != principal.Epoch {
			continue
		}
		selected := map[string]bool{}
		for _, p := range view.Projects {
			selected[p.ProjectUID] = true
		}
		matches := true
		if submission.Operation.Version == 3 {
			intent := submission.Operation.ImportIntent
			matches = intent != nil && selected[intent.ProjectUID]
		}
		for _, member := range submission.Operation.Members {
			matches = matches && selected[member.ProjectUID]
		}
		if !matches {
			continue
		}
		view, err = s.fenceScopedAuthorityTx(ctx, tx, key, capability)
		if err != nil {
			return db.ScopedViewDescriptor{}, nil, err
		}
		grant, err := scanFederationEnrollment(tx.QueryRowContext(ctx, federationEnrollmentSelect+` WHERE relay_binding_uid=$1`, principal.BindingUID))
		if err != nil || grant.Actor != principal.AccountableActor {
			return db.ScopedViewDescriptor{}, nil, db.ErrNotFound
		}
		// Onward custody has the actual upstream account, which may differ from
		// both the receiving account and the immutable origin principal.
		var onward *db.CustodyPrincipal
		if position >= 0 {
			var actor string
			if err := tx.QueryRowContext(ctx, `SELECT b.bound_actor FROM federation_bindings b JOIN projects p ON p.id=b.project_id WHERE p.uid=$1`, view.Projects[0].ProjectUID).Scan(&actor); err != nil {
				return db.ScopedViewDescriptor{}, nil, err
			}
			incoming := view.Route[position]
			value := db.CustodyPrincipal{AuthorityUID: view.AuthorityUID, BindingUID: incoming.BindingUID, Epoch: incoming.Epoch, AccountableActor: actor, RouteDigest: view.RouteDigest}
			if err := value.Validate(); err != nil {
				return db.ScopedViewDescriptor{}, nil, db.ErrNotFound
			}
			onward = &value
		}
		return view, onward, nil
	}
	return db.ScopedViewDescriptor{}, nil, db.ErrNotFound
}

func (s *Store) RetainSourceOperation(ctx context.Context, principal db.CustodyPrincipal, submission db.SourceSubmission) (db.SourceStatus, error) {
	var result db.SourceStatus
	err := s.relayTx(ctx, func(tx *sql.Tx) error {
		result = db.SourceStatus{}
		view, onward, err := s.scopedSourceAuthorityTx(ctx, tx, principal, submission)
		if err != nil {
			return err
		}
		// Proof DATA is captured here, but alias IO is lazy: the shared ONE
		// retain engine verifies every proof/creator relation before invoking
		// its reference callback. No-alias/newborn commands perform no read.
		endpoints := make(map[string]db.ScopedEndpoint, len(submission.Operation.Membership))
		for _, proof := range submission.Operation.Membership {
			endpoints[proof.Endpoint.IssueUID] = proof.Endpoint
		}
		aliases, err := scopedSourceReferenceAliasesTx(ctx, tx, endpoints, nil, nil)
		if err != nil {
			return err
		}
		result, err = db.RetainScopedSourceTx(ctx, tx, view, principal, onward, submission, aliases)
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = db.ErrNotFound
		}
		return db.SourceStatus{}, err
	}
	return result, nil
}
