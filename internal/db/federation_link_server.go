package db

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/httpurl"
)

// CheckLinkSameServer refuses links involving a spoke unless both projects
// share a canonical hub base URL. It permits local and hub projects.
func CheckLinkSameServer(ctx context.Context, q moveQuery, fromProject, toProject int64) error {
	type project struct {
		name, hub string
		spoke     bool
	}
	read := func(id int64) (project, error) {
		var p project
		var role, hub sql.NullString
		err := q.QueryRowContext(ctx, `SELECT p.name,b.role,b.hub_url FROM projects p LEFT JOIN federation_bindings b ON b.project_id=p.id WHERE p.id=$1`, id).Scan(&p.name, &role, &hub)
		p.spoke = role.String == string(FederationRoleSpoke)
		p.hub = hub.String
		return p, err
	}
	from, err := read(fromProject)
	if err != nil {
		return err
	}
	to, err := read(toProject)
	if err != nil {
		return err
	}
	if !from.spoke && !to.spoke {
		return nil
	}
	if from.spoke && to.spoke {
		a, ae := httpurl.CanonicalHTTPBaseURL(from.hub)
		b, be := httpurl.CanonicalHTTPBaseURL(to.hub)
		if ae == nil && be == nil && a == b {
			return nil
		}
	}
	if !from.spoke && to.spoke {
		from, to = to, from
	}
	return &LinkServerMismatchError{From: from.name, To: to.name}
}

// CheckLinkProjectsAccess checks access to each project in the current transaction.
func CheckLinkProjectsAccess(ctx context.Context, tx Transaction, fromProject int64, toProjects []int64) error {
	seen := map[int64]bool{}
	for _, id := range append([]int64{fromProject}, toProjects...) {
		if seen[id] {
			continue
		}
		seen[id] = true
		var uid string
		err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, id).Scan(&uid)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := CheckProjectAccessTransaction(ctx, tx, uid); err != nil {
			return err
		}
	}
	return nil
}
