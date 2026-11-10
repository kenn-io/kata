package db_test

import (
	"testing"

	"go.kenn.io/kata/internal/db"
)

// Contract: selection identity and an active boolean from an older process or
// restored database never authorize provider mutations in the current process.
func TestTrackerActivationRequiresCurrentProcessAndExactBinding(t *testing.T) {
	f := trackerFenceFixture()
	a := db.TrackerActivation{Version: 1, RootInstanceUID: f.RootInstanceUID, ProjectUID: f.ProjectUID, Connection: f.Connection, BindingID: 1, SourceKey: f.SourceKey, ProcessEpoch: f.ProcessEpoch, Active: true, EventFloor: 10}
	if !a.Allows(f.ProcessEpoch, 1, f.Connection, f.SourceKey, 11) {
		t.Fatal("fresh exact activation denied")
	}
	if a.Allows("01ARZ3NDEKTSV4RRFFQ69G5FB1", 1, f.Connection, f.SourceKey, 11) || a.Allows(f.ProcessEpoch, 1, f.Connection, f.SourceKey, 10) || a.Allows(f.ProcessEpoch, 2, f.Connection, f.SourceKey, 11) {
		t.Fatal("stale activation broadened")
	}
	changed := f.Connection
	changed.Generation = "changed"
	if a.Allows(f.ProcessEpoch, 1, changed, f.SourceKey, 11) {
		t.Fatal("selection changed")
	}
	a.RestoredHistoryGap = true
	if a.Allows(f.ProcessEpoch, 1, f.Connection, f.SourceKey, 11) {
		t.Fatal("known missing restore history authorized a new dispatch")
	}
	a.RestoredHistoryGap = false
	a.Active = false
	if a.Allows(f.ProcessEpoch, 1, f.Connection, f.SourceKey, 11) {
		t.Fatal("restored barrier ignored")
	}
}
