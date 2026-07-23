package main

import (
	"context"
	"log"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

type federationConfigReconciler interface {
	Run(context.Context) error
	Health() federation.Health
}

var newFederationConfigReconciler = func(
	cfg federation.ReconcilerConfig,
) federationConfigReconciler {
	return federation.NewReconciler(cfg)
}

func startFederationConfigReconciler(
	ctx context.Context,
	daemonConfig *config.DaemonConfig,
	store db.Storage,
	federationWake func(),
	daemonLog *log.Logger,
) func() api.FederationConfigHealth {
	if len(daemonConfig.Federation.Projects) == 0 {
		return nil
	}

	targets := make([]federation.Target, 0, len(daemonConfig.Federation.Projects))
	for _, mapping := range daemonConfig.Federation.Projects {
		catalog, _ := daemonConfig.CatalogDaemon(mapping.Hub)
		targets = append(targets, federation.Target{
			Catalog: catalog,
			Mapping: mapping,
		})
	}
	reconciler := newFederationConfigReconciler(federation.ReconcilerConfig{
		Store:       store,
		Credentials: config.DefaultFederationCredentialStore(),
		Targets:     targets,
		HubFactory: func(
			ctx context.Context, catalog config.CatalogDaemonConfig,
		) (federation.Hub, error) {
			return federation.NewHubClient(ctx, catalog)
		},
		Wake:   federationWake,
		Logger: daemonLog,
	})
	go func() {
		_ = reconciler.Run(ctx)
	}()
	return func() api.FederationConfigHealth {
		health := reconciler.Health()
		return api.FederationConfigHealth{
			Configured:        health.Configured,
			Reconciled:        health.Reconciled,
			Pending:           health.Pending,
			Conflicted:        health.Conflicted,
			LastAttemptAt:     health.LastAttemptAt,
			LastSuccessAt:     health.LastSuccessAt,
			LastErrorCategory: health.LastErrorCategory,
			LastErrorStatus:   health.LastErrorStatus,
		}
	}
}
