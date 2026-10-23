package main

import (
	"context"
	"errors"
	"log"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/mcpevents"
)

// mcpEventsStartup carries what the daemon hands the Events service.
type mcpEventsStartup struct {
	Config      config.DaemonConfig
	KeyPath     string
	Store       db.Storage
	Authority   mcpevents.Authority
	Broadcaster mcpevents.Subscriber
	Log         *log.Logger
}

// startMCPEvents builds the daemon-owned Events service and runs its delivery
// worker under the daemon's worker group, which the shutdown coordinator
// cancels and joins before the store closes.
//
// Events failures never stop the daemon. A missing, corrupt or mismatched key
// leaves the service built but disabled; a construction failure leaves it nil.
// Either way the returned reason is what the status route reports, and every
// other daemon function keeps working. A nil service with an empty reason
// means Events is not enabled in configuration.
func startMCPEvents(ctx context.Context, workers *daemonWorkerGroup, in mcpEventsStartup) (*mcpevents.Service, string) {
	if !in.Config.MCPEvents.Enabled {
		return nil, ""
	}
	maxTTL, err := in.Config.MCPEvents.MaxTTLDuration()
	if err != nil {
		in.Log.Printf("MCP Events disabled: %v", err)
		return nil, mcpevents.ReasonStartFailed
	}
	service, err := mcpevents.NewService(mcpevents.ServiceConfig{
		Store:       in.Store,
		Keys:        mcpevents.NewFileKeyProvider(in.KeyPath),
		Authority:   in.Authority,
		Webhook:     mcpevents.NewWebhookClient(),
		MaxTTL:      maxTTL,
		Broadcaster: in.Broadcaster,
		InstanceUID: in.Store.InstanceUID(),
	})
	if err != nil {
		in.Log.Printf("MCP Events disabled: %v", err)
		return nil, mcpevents.ReasonStartFailed
	}
	if availability := service.Availability(); !availability.Enabled {
		in.Log.Printf("MCP Events disabled: %s", availability.Reason)
	}
	workers.Go(func() {
		if err := service.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			in.Log.Printf("MCP Events worker: %v", err)
		}
	})
	return service, ""
}

// mcpEventsKeyPath is the owner-only key file for this daemon's storage.
func mcpEventsKeyPath(storageID string) (string, error) {
	home, err := config.KataHome()
	if err != nil {
		return "", err
	}
	return config.MCPEventsKeyPath(home, storageID), nil
}

// startDaemonMCPEvents wires the daemon's own authority, wake source and key
// file into startMCPEvents.
func startDaemonMCPEvents(
	ctx context.Context,
	workers *daemonWorkerGroup,
	dcfg *config.DaemonConfig,
	storageID string,
	store db.Storage,
	broadcaster *daemon.EventBroadcaster,
	daemonLog *log.Logger,
) (*mcpevents.Service, string) {
	if !dcfg.MCPEvents.Enabled {
		return nil, ""
	}
	keyPath, err := mcpEventsKeyPath(storageID)
	if err != nil {
		daemonLog.Printf("MCP Events disabled: %v", err)
		return nil, mcpevents.ReasonStartFailed
	}
	return startMCPEvents(ctx, workers, mcpEventsStartup{
		Config:      *dcfg,
		KeyPath:     keyPath,
		Store:       store,
		Authority:   daemon.NewMCPEventsAuthority(daemon.ServerConfig{DB: store, Auth: dcfg.Auth}),
		Broadcaster: daemon.NewMCPEventsSubscriber(broadcaster),
		Log:         daemonLog,
	})
}
