package client

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kata/pkg/client/generated"
)

// TelemetryGate shares disabled receipts within a process and serializes receipt
// checks across processes. Its zero value is ready to use.
type TelemetryGate struct {
	mu       sync.Mutex
	disabled map[string]string
}

// Report rechecks daily markers under the target's gate and records a valid
// receipt before releasing it. A day already settled by those markers returns
// without waiting for the gate. Agent claims end only on full or disabled;
// other daily claims end on any accepted receipt. A true result ends this day's
// reporting for this claim. An empty claim keeps enabled per-run reports distinct.
func (g *TelemetryGate) Report(ctx context.Context, target, claim, day string, agent bool, send func(context.Context) (*generated.CaptureTelemetryEventResponse, error)) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if g.settled(target, claim, day) {
		return true, nil
	}
	unlock, err := lockTelemetryGate(ctx, target)
	if err != nil {
		return false, err
	}
	defer unlock()
	if g.settled(target, claim, day) {
		return true, nil
	}
	receipt, err := send(ctx)
	if err != nil {
		return false, err
	}
	if receipt == nil {
		return false, fmt.Errorf("telemetry returned no receipt")
	}
	if err := receipt.Validate(); err != nil {
		return false, err
	}
	quiet := receipt.Status == generated.Disabled
	if quiet {
		RecordTelemetryDayMarker(TelemetryDisabledMarkerPath(target), day)
		g.markQuiet(target, day)
	}
	done := !agent || quiet || receipt.Status == generated.Full
	if done && claim != "" {
		RecordTelemetryDayMarker(claim, day)
	}
	return quiet || (done && claim != ""), nil
}

// settled reports whether a disabled receipt or this claim already ends the day.
func (g *TelemetryGate) settled(target, claim, day string) bool {
	if g.quiet(target, day) {
		return true
	}
	if ReadTelemetryDayMarker(TelemetryDisabledMarkerPath(target)) == day {
		g.markQuiet(target, day)
		return true
	}
	return claim != "" && ReadTelemetryDayMarker(claim) == day
}

// lockTelemetryGate serializes receipt checks for target across processes. When
// the Kata home cannot hold the gate file, no caller can persist a receipt either,
// so the report proceeds unserialized instead of being dropped.
func lockTelemetryGate(ctx context.Context, target string) (func(), error) {
	path := TelemetryDayMarkerPathForKey(target, "gate-")
	if path == "" || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return func() {}, nil
	}
	lock := flock.New(path, flock.SetPermissions(0o600))
	// An unlocked result without a context error means the gate file cannot be opened.
	if locked, _ := lock.TryLockContext(ctx, 10*time.Millisecond); locked {
		return func() { _ = lock.Unlock() }, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return func() {}, nil
}

func (g *TelemetryGate) quiet(target, day string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.disabled[target] == day
}

func (g *TelemetryGate) markQuiet(target, day string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.disabled == nil {
		g.disabled = make(map[string]string)
	}
	g.disabled[target] = day
}
