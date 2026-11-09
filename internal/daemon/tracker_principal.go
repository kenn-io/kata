package daemon

import (
	"context"
	"errors"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
)

var errTrackerPrincipalUnsupported = errors.New("tracker principal is unsupported for this client authentication mode")

// trackerRequestPrincipal consumes middleware authentication facts. Attribution
// labels and enrollment transport are deliberately not inputs. The caller must
// still revalidate token/project scope and apply root mapping and owner grants.
func trackerRequestPrincipal(ctx context.Context, instanceUID string) (tracker.Principal, error) {
	if insecureReadonlyRequest(ctx) || unauthenticatedPrivateNetworkRequest(ctx) || webSessionAuthenticated(ctx) {
		return tracker.Principal{}, errTrackerPrincipalUnsupported
	}
	p, ok := PrincipalFromContext(ctx)
	if mcp, _ := ctx.Value(trackerMCPClientKey{}).(bool); mcp && (!ok || p.Kind != PrincipalDBToken) {
		return tracker.Principal{}, errTrackerPrincipalUnsupported
	}
	result := tracker.Principal{AuthorityInstanceUID: instanceUID}
	if !ok {
		if !projectOwnerAuthority(ctx) {
			return tracker.Principal{}, errTrackerPrincipalUnsupported
		}
		result.Kind = tracker.SubjectOwnerLocal
		result.Subject = instanceUID
	} else {
		switch p.Kind {
		case PrincipalDBToken:
			// Issue-subtree tokens cannot control a project-wide provider binding.
			// Relay-parent tokens are transport only, not member account authority.
			if p.TokenID <= 0 || p.Scope != nil || db.ValidateTokenActor(p.Actor) != nil {
				return tracker.Principal{}, errTrackerPrincipalUnsupported
			}
			result.Kind = tracker.SubjectAccount
			result.Subject = p.Actor
		case PrincipalStaticToken:
			result.Kind = tracker.SubjectStaticOwner
			result.Subject = instanceUID
		default:
			return tracker.Principal{}, errTrackerPrincipalUnsupported
		}
	}
	if result.Validate() != nil {
		return tracker.Principal{}, errTrackerPrincipalUnsupported
	}
	return result, nil
}

type trackerMCPClientKey struct{}

// This caller marker only reduces authority; it never supplies an identity.
func trackerClientContext(ctx context.Context, client string) (context.Context, error) {
	switch client {
	case "":
		return ctx, nil
	case "mcp":
		return context.WithValue(ctx, trackerMCPClientKey{}, true), nil
	default:
		return ctx, errTrackerPrincipalUnsupported
	}
}
