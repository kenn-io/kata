package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/telemetry"
	"go.kenn.io/kit/telemetry/posthog"
)

func TestSetupFinishedDeliveryRetry(t *testing.T) {
	for _, outcome := range []string{"delivery interrupted", "completion interrupted", "disabled result", "second daemon completes"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "kata.db")
				store := openKataTestDB(t, path)
				project, err := store.CreateProject(t.Context(), "example-project")
				require.NoError(t, err)
				_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "First issue", Author: "tester"})
				require.NoError(t, err)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var first posthog.Delivery
				reporter := &fakeTelemetryReporter{onConfirmed: func(ctx context.Context, delivery posthog.Delivery) (posthog.Status, error) {
					first = delivery
					switch outcome {
					case "completion interrupted":
						cancel()
						return posthog.StatusDelivered, nil
					case "second daemon completes":
						return "", context.DeadlineExceeded
					case "disabled result":
						return posthog.StatusDisabled, nil
					}
					<-ctx.Done()
					return "", ctx.Err()
				}}
				workers := newDaemonWorkerGroup()
				startSetupFinished(ctx, workers, store, reporter)
				synctest.Wait()
				require.NotEqual(t, uuid.Nil(), first.ID)
				if outcome == "second daemon completes" {
					otherWorkers := newDaemonWorkerGroup()
					startSetupFinished(t.Context(), otherWorkers, store, &fakeTelemetryReporter{})
					synctest.Wait()
					require.True(t, otherWorkers.Wait(t.Context()))
					time.Sleep(time.Minute)
					synctest.Wait()
					require.True(t, workers.Wait(t.Context()))
					require.Equal(t, 1, reporter.eventCount(), "completed delivery stops the first daemon's retries")
					require.NoError(t, store.Close())
					return
				}
				// Sending must leave the database available to issue writers.
				_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Another issue", Author: "tester"})
				require.NoError(t, err)
				cancel()
				require.True(t, workers.Wait(t.Context()))
				require.NoError(t, store.Close())
				time.Sleep(2 * time.Hour)
				store = openKataTestDB(t, path)
				defer func() { require.NoError(t, store.Close()) }()
				startSetupFinished(t.Context(), newDaemonWorkerGroup(), store, telemetry.DisabledReporter())
				second := &fakeTelemetryReporter{}
				workers = newDaemonWorkerGroup()
				startSetupFinished(t.Context(), workers, store, second)
				synctest.Wait()
				require.True(t, workers.Wait(t.Context()))
				require.Equal(t, 1, second.eventCount())
				require.Equal(t, "setup_finished", second.eventAt(0).event)
				require.Equal(t, first, second.eventAt(0).delivery)
			})
		})
	}
}

func TestSetupFinishedAfterEmptyStartup(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint("restart=", restart), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := openKataTestDB(t, filepath.Join(t.TempDir(), "kata.db"))
				defer func() { require.NoError(t, store.Close()) }()
				project, err := store.CreateProject(t.Context(), "example-project")
				require.NoError(t, err)
				reporter := &fakeTelemetryReporter{}
				workers := newDaemonWorkerGroup()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				startSetupFinished(ctx, workers, store, reporter)
				synctest.Wait()
				require.Zero(t, reporter.eventCount())
				_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "First issue", Author: "tester"})
				require.NoError(t, err)
				if restart {
					cancel()
					require.True(t, workers.Wait(t.Context()))
					require.Zero(t, reporter.eventCount())
					workers = newDaemonWorkerGroup()
					startSetupFinished(t.Context(), workers, store, reporter)
				} else {
					time.Sleep(time.Minute)
				}
				synctest.Wait()
				require.True(t, workers.Wait(t.Context()))
				require.Equal(t, 1, reporter.eventCount())
			})
		})
	}
}
