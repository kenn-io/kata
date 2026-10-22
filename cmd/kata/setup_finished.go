package main

import (
	"context"
	"log/slog"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/telemetry"
	"go.kenn.io/kit/telemetry/posthog"
)

func startSetupFinished(ctx context.Context, workers *daemonWorkerGroup, store db.Storage, reporter telemetry.Client) {
	if reporter == nil || !reporter.Enabled() {
		return
	}
	workers.Go(func() {
		for ctx.Err() == nil {
			if captureSetupFinished(ctx, store, reporter) {
				return
			}
			timer := time.NewTimer(time.Minute)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	})
}

func captureSetupFinished(ctx context.Context, store db.Storage, reporter telemetry.Client) bool {
	pending, complete, err := store.PrepareSetupFinished(ctx, time.Now())
	if err == nil && pending != nil && reporter.Enabled() {
		sendCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		var status posthog.Status
		status, err = reporter.CaptureConfirmed(sendCtx, "setup_finished", nil, posthog.Delivery{ID: pending.ID, Time: pending.Time})
		if err == nil && status == posthog.StatusDelivered {
			err = store.CompleteSetupFinished(ctx, *pending)
			complete = err == nil
		}
	}
	if err != nil {
		slog.Warn("telemetry setup capture", "err", err)
	}
	return complete
}
