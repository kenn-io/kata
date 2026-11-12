package main

import (
	"context"

	"go.kenn.io/kata/internal/agentsview"
	"go.kenn.io/kata/internal/config"
)

// reloadAgentsView keeps the original destination and only re-resolves its
// credential; a changed destination never receives a newly selected secret.
func reloadAgentsView(ctx context.Context, home string, service *agentsview.Service, trustPrivate bool) (bool, error) {
	cfg, err := config.ReadDaemonConfigForHome(home)
	if err != nil {
		return false, err
	}
	target := cfg.AgentsView
	target.TokenFile = ""
	target.TokenEnv = ""
	resolved, err := target.Resolve(ctx, trustPrivate)
	if err != nil {
		return false, err
	}
	if !service.SameTarget(resolved) {
		return false, nil
	}
	resolved, err = cfg.AgentsView.Resolve(ctx, trustPrivate)
	if err != nil {
		return false, err
	}
	return service.Reload(resolved, trustPrivate)
}
