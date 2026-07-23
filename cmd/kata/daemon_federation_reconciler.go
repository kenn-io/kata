package main

import (
	"context"
	"log"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationconfig"
)

type federationConfigReconciler interface {
	Run(context.Context) error
	Health() federationconfig.Health
}

var newFederationConfigReconciler = func(
	cfg federationconfig.ReconcilerConfig,
) federationConfigReconciler {
	return federationconfig.NewReconciler(cfg)
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

	targets := make([]federationconfig.Target, 0, len(daemonConfig.Federation.Projects))
	for _, mapping := range daemonConfig.Federation.Projects {
		catalog, _ := daemonConfig.CatalogDaemon(mapping.Hub)
		targets = append(targets, federationconfig.Target{
			Catalog: catalog,
			Mapping: mapping,
		})
	}
	reconciler := newFederationConfigReconciler(federationconfig.ReconcilerConfig{
		Store:       store,
		Credentials: config.DefaultFederationCredentialStore(),
		Targets:     targets,
		HubFactory: func(
			ctx context.Context, catalog config.CatalogDaemonConfig,
		) (federationconfig.Hub, error) {
			return federationconfig.NewHubClient(ctx, catalog)
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
