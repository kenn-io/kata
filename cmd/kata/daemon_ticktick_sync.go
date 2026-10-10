package main

import (
	"context"
	"log"
	"log/slog"
	"time"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/tickticksync"
)

var newTickTickSyncDaemonRunner = func(c tickticksync.RunnerConfig) issueSyncDaemonRunner { return tickticksync.NewRunner(c) }

func newConfiguredTickTickSyncFetcher(c config.TickTickSyncConfig) tickticksync.Fetcher {
	return tickticksync.NewClient(tickticksync.ClientConfig{TokenEnv: c.TokenEnv})
}
func startTickTickSyncRunner(
	ctx context.Context,
	workers *daemonWorkerGroup,
	drainAdmission activity.WaitableAdmission,
	store db.Storage,
	fetcher tickticksync.Fetcher,
	publisher daemon.EventPublisher,
	daemonLog *log.Logger,
	progress *issuesync.ProgressTracker,
) func() {
	if fetcher == nil {
		fetcher = newConfiguredTickTickSyncFetcher(config.TickTickSyncConfig{})
	}
	return startIssueSyncRunner(ctx, workers, daemonLog, "ticktick", func(wake <-chan struct{}, logger *slog.Logger) issueSyncDaemonRunner {
		return newTickTickSyncDaemonRunner(tickticksync.RunnerConfig{
			Store:          store,
			Fetcher:        fetcher,
			Progress:       progress,
			Logger:         logger,
			Interval:       30 * time.Second,
			Wake:           wake,
			DrainAdmission: drainAdmission,
			EventSinkFrom: func(_ context.Context, projectID int64, events []db.Event, fork activity.Admission) error {
				publisher.EventsFrom(projectID, events, fork)
				return nil
			},
		})
	})
}
