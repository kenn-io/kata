package daemon

import (
	"context"

	"go.kenn.io/kata/internal/tracker"
)

// memberConnection admits a real authenticated member only through the root's
// explicit mapping and the selected owner's exact grant. No caller actor or
// credential selector is accepted. Provider preflight reads consume bind grants;
// scheduled execution still needs an independent service mapping and grant.
func (r *TrackerRuntime) memberConnection(ctx context.Context, project string, principal tracker.Principal, selection tracker.Selection, action tracker.Action, resource string) (context.Context, trackerLocalConnection, error) {
	if selection.Validate() != nil || !tracker.ValidIdentityText(resource, 2048) {
		return ctx, trackerLocalConnection{}, tracker.ErrInvalid
	}
	if r != nil && selection.OwnerInstanceUID != r.store.InstanceUID() {
		return r.remoteMemberConnection(ctx, project, principal, selection, action, resource)
	}
	snapshot, err := r.current()
	if err != nil {
		return ctx, trackerLocalConnection{}, err
	}
	c, ok := snapshot.connections[selection.ConnectionUID]
	if !ok || c.descriptor.Selection != selection {
		return ctx, trackerLocalConnection{}, tracker.ErrUnavailable
	}
	ctx, err = r.mappedContext(ctx, snapshot, project, principal)
	if err != nil {
		return ctx, trackerLocalConnection{}, err
	}
	if !c.descriptor.Allows(r.store.InstanceUID(), project, principal, action, resource) {
		return ctx, trackerLocalConnection{}, tracker.ErrDenied
	}
	authority := trackerRunAuthority{root: r.store.InstanceUID(), project: project, principal: principal, source: resource, action: action, incarnation: snapshot.incarnation}
	ctx = context.WithValue(ctx, trackerRunAuthorityKey{}, authority)
	return tracker.WithRunSelection(ctx, selection), c, nil
}

// Owner-local configuration is copied only inside this daemon. Transport DTOs
// carry public origins instead; credential selectors never leave the owner.
func trackerPreflightConfig(cfg ServerConfig, c trackerLocalConnection) ServerConfig {
	switch c.config.Provider {
	case "github":
		cfg.GitHubSyncConfig = *c.config.GitHub
	case "notion":
		cfg.NotionSyncConfig = *c.config.Notion
	case "linear":
		cfg.LinearSyncConfig = *c.config.Linear
	case "plane":
		cfg.PlaneSyncConfig = *c.config.Plane
	case "ticktick":
		cfg.TickTickSyncConfig = *c.config.TickTick
	case "todoist":
		cfg.TodoistSyncConfig = *c.config.Todoist
	case "twenty":
		cfg.TwentySyncConfig = *c.config.Twenty
	}
	return cfg
}
