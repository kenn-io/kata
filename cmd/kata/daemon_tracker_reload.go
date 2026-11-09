package main

import (
	"context"
	"errors"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
)

// Unlike hook reload's keep-previous policy, invalid account policy must suspend
// named sessions. Never log raw configuration/credential-resolution diagnostics.
func reloadTrackerConnections(ctx context.Context, r *daemon.TrackerRuntime, read func() (*config.DaemonConfig, error)) error {
	r.Suspend()
	cfg, err := read()
	if err != nil || cfg == nil {
		return errors.New("tracker configuration unavailable; named connections suspended")
	}
	if err := r.Reconfigure(ctx, cfg.TrackerConnections, cfg.TrackerAccess); err != nil {
		return safeTrackerPolicyError(err)
	}
	return nil
}

func safeTrackerPolicyError(err error) error {
	for _, safe := range []error{tracker.ErrInvalid, tracker.ErrGenerationChanged, tracker.ErrGenerationRetired, tracker.ErrHistoryFull} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return tracker.ErrUnavailable
}

func optionalTrackerRouter(routes []func(context.Context, db.IssueSyncBinding) (context.Context, error)) func(context.Context, db.IssueSyncBinding) (context.Context, error) {
	if len(routes) == 0 {
		return nil
	}
	return routes[0]
}
