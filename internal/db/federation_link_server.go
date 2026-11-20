package db

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/httpurl"
)

// CheckLinkSameServer requires linked spokes to share a canonical hub base URL
// and have sync and push enabled. It permits local and hub projects.
func CheckLinkSameServer(ctx context.Context, q moveQuery, fromProject, toProject int64) error {
	type project struct {
		name, hub     string
		spoke, active bool
	}
	read := func(id int64) (project, error) {
		var p project
		var role, hub sql.NullString
		var enabled, push int
		err := q.QueryRowContext(ctx, `SELECT p.name,b.role,b.hub_url,COALESCE(b.enabled,0),COALESCE(b.push_enabled,0) FROM projects p LEFT JOIN federation_bindings b ON b.project_id=p.id WHERE p.id=$1`, id).Scan(&p.name, &role, &hub, &enabled, &push)
		p.spoke = role.String == string(FederationRoleSpoke)
		p.hub = hub.String
		p.active = enabled == 1 && push == 1
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
			if !from.active {
				return &LinkServerMismatchError{Paused: from.name}
			}
			if !to.active {
				return &LinkServerMismatchError{Paused: to.name}
			}
			return nil
		}
	}
	if !from.spoke && to.spoke {
		from, to = to, from
	}
	return &LinkServerMismatchError{From: from.name, To: to.name}
}
