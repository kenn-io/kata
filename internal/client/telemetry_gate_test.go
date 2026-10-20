package client

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestTelemetryGateWaitsAndRechecks(t *testing.T) {
	for _, receipt := range []string{"queued", "disabled", "deadline"} {
		t.Run(receipt, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			synctest.Test(t, func(t *testing.T) {
				const target = "http://daemon.example"
				day := time.Now().UTC().Format(time.DateOnly)
				path := TelemetryDayMarkerPathForKey(target, "gate-")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				owner := flock.New(path)
				require.NoError(t, owner.Lock())
				defer func() { _ = owner.Unlock() }()
				var gate TelemetryGate
				var posts atomic.Int32
				send := func(context.Context) (*generated.CaptureTelemetryEventResponse, error) {
					posts.Add(1)
					return &generated.CaptureTelemetryEventResponse{Status: generated.Queued}, nil
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				var wg sync.WaitGroup
				var stopped bool
				var err error
				wg.Go(func() { stopped, err = gate.Report(ctx, target, "", day, true, send) })
				synctest.Wait()
				require.Zero(t, posts.Load(), "the waiter must not send while another caller owns the gate")
				if receipt == "deadline" {
					time.Sleep(time.Second)
					wg.Wait()
					require.ErrorIs(t, err, context.DeadlineExceeded)
					require.Zero(t, posts.Load())
					return
				}
				if receipt == "disabled" {
					RecordTelemetryDayMarker(TelemetryDisabledMarkerPath(target), day)
				}
				require.NoError(t, owner.Unlock())
				time.Sleep(10 * time.Millisecond)
				wg.Wait()
				require.NoError(t, err)
				if receipt == "disabled" {
					require.True(t, stopped)
					require.Zero(t, posts.Load(), "the waiter must recheck the owner's disabled receipt")
				} else {
					require.False(t, stopped)
					require.EqualValues(t, 1, posts.Load(), "an enabled report waits instead of being dropped")
				}
			})
		})
	}
}

func TestTelemetryGateSettledDayDoesNotWait(t *testing.T) {
	for _, settled := range []string{"claimed", "disabled"} {
		t.Run(settled, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			synctest.Test(t, func(t *testing.T) {
				const target = "http://daemon.example"
				day := time.Now().UTC().Format(time.DateOnly)
				claim := TelemetryDayMarkerPathForKey(target, "cli-opened-")
				if settled == "claimed" {
					RecordTelemetryDayMarker(claim, day)
				} else {
					RecordTelemetryDayMarker(TelemetryDisabledMarkerPath(target), day)
				}
				path := TelemetryDayMarkerPathForKey(target, "gate-")
				owner := flock.New(path)
				require.NoError(t, owner.Lock())
				defer func() { _ = owner.Unlock() }()
				var gate TelemetryGate
				var posts atomic.Int32
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				started := time.Now()
				stopped, err := gate.Report(ctx, target, claim, day, false, func(context.Context) (*generated.CaptureTelemetryEventResponse, error) {
					posts.Add(1)
					return &generated.CaptureTelemetryEventResponse{Status: generated.Queued}, nil
				})
				require.NoError(t, err)
				require.True(t, stopped)
				require.Zero(t, posts.Load())
				require.Zero(t, time.Since(started), "a settled day must not wait for another caller's report")
			})
		})
	}
}

func TestTelemetryGateUnwritableHomeStillReports(t *testing.T) {
	for _, home := range []string{"file", "read-only"} {
		t.Run(home, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("KATA_HOME", dir)
			telemetryDir := filepath.Join(dir, "telemetry")
			if home == "file" {
				require.NoError(t, os.WriteFile(telemetryDir, nil, 0o600))
			} else {
				if runtime.GOOS == "windows" || os.Geteuid() == 0 {
					t.Skip("directory permissions do not block file creation here")
				}
				require.NoError(t, os.Mkdir(telemetryDir, 0o500))
			}
			var gate TelemetryGate
			var posts atomic.Int32
			_, err := gate.Report(t.Context(), "http://daemon.example", "", time.Now().UTC().Format(time.DateOnly), false, func(context.Context) (*generated.CaptureTelemetryEventResponse, error) {
				posts.Add(1)
				return &generated.CaptureTelemetryEventResponse{Status: generated.Queued}, nil
			})
			require.NoError(t, err)
			require.EqualValues(t, 1, posts.Load(), "an unwritable home loses daily markers, not the report")
		})
	}
}
