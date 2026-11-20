package db

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/httpurl"
)

// CheckLinkSameServer refuses links involving a spoke unless both projects can
// push to the same canonical hub base URL. It permits local and hub projects.
func CheckLinkSameServer(ctx context.Context, q moveQuery, fromProject, toProject int64) error {
	type project struct {
		name, hub string
		spoke     bool
		active    bool
	}
	read := func(id int64) (project, error) {
		var p project
		var role, hub sql.NullString
		var enabled, pushEnabled int
		err := q.QueryRowContext(ctx, `SELECT p.name,b.role,b.hub_url,COALESCE(b.enabled,0),COALESCE(b.push_enabled,0) FROM projects p LEFT JOIN federation_bindings b ON b.project_id=p.id WHERE p.id=$1`, id).Scan(&p.name, &role, &hub, &enabled, &pushEnabled)
		p.spoke = role.String == string(FederationRoleSpoke)
		p.hub = hub.String
		p.active = enabled == 1 && pushEnabled == 1
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
				return &LinkServerMismatchError{To: from.name, Paused: true}
			}
			if !to.active {
				return &LinkServerMismatchError{To: to.name, Paused: true}
			}
			return nil
		}
	}
	if !from.spoke && to.spoke {
		from, to = to, from
	}
	return &LinkServerMismatchError{From: from.name, To: to.name}
}
