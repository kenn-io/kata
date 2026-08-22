package hooks

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPrune_StartupSeed_TotalsBytesInDir(t *testing.T) {
	dir := t.TempDir()
	writeHookLogs(t, dir, 1, 0, 100, 50)
	mustWrite(t, filepath.Join(dir, "2.0.out"), 200)
	p := setupSeededPruner(t, dir, 1024)
	if got := p.Total(); got != 350 {
		t.Fatalf("seed total = %d, want 350", got)
	}
}

func TestPrune_MaybeSweep_OldestGroupFirst(t *testing.T) {
	dir := t.TempDir()
	writeHookLogs(t, dir, 10, 0, 100, 50)
	writeHookLogs(t, dir, 20, 0, 100, 50)
	writeHookLogs(t, dir, 30, 0, 100, 50)
	p := setupSeededPruner(t, dir, 250)
	p.MaybeSweep()
	// 450 -> cap 250 -> must delete oldest groups (10.0 and 20.0) leaving 150.
	assertPruned(t, filepath.Join(dir, "10.0.out"))
	assertRetained(t, filepath.Join(dir, "30.0.out"))
}

func TestPrune_AtomicGroup_DeletesOutAndErrTogether(t *testing.T) {
	dir := t.TempDir()
	writeHookLogs(t, dir, 10, 0, 100, 50)
	writeHookLogs(t, dir, 20, 0, 100, 50)
	p := setupSeededPruner(t, dir, 100)
	p.MaybeSweep()
	assertPruned(t, filepath.Join(dir, "10.0.out"))
	assertPruned(t, filepath.Join(dir, "10.0.err"))
}

func TestPrune_PartialGroup_NotFatal(t *testing.T) {
	dir := t.TempDir()
	// Only .out exists for this group; .err is missing.
	mustWrite(t, filepath.Join(dir, "10.0.out"), 100)
	writeHookLogs(t, dir, 20, 0, 100, 50)
	p := setupSeededPruner(t, dir, 100)
	p.MaybeSweep()
	assertPruned(t, filepath.Join(dir, "10.0.out"))
}

func TestPrune_AddAfterRun_TriggersSweep(t *testing.T) {
	dir := t.TempDir()
	p := setupSeededPruner(t, dir, 100)
	writeHookLogs(t, dir, 1, 0, 80, 0)
	p.AddRun(1, 0, 80, 0)
	writeHookLogs(t, dir, 2, 0, 80, 0)
	p.AddRun(2, 0, 80, 0)
	// Total now 160 over cap 100 -> after second AddRun, sweep should run
	// and delete oldest (1.0) leaving 80.
	assertPruned(t, filepath.Join(dir, "1.0.out"))
}

// TestPrune_SkipsActiveGroup pins the contract that MaybeSweep never
// unlinks an in-flight group's .out/.err. Marking 10.0 active blocks
// the oldest group from being deleted; the next-oldest is taken
// instead even though it lives newer in the (event_id, hook_index)
// ordering.
func TestPrune_SkipsActiveGroup(t *testing.T) {
	dir := t.TempDir()
	writeHookLogs(t, dir, 10, 0, 100, 50)
	writeHookLogs(t, dir, 20, 0, 100, 50)
	writeHookLogs(t, dir, 30, 0, 100, 50)
	p := setupSeededPruner(t, dir, 200)
	p.SetActiveCheck(func(k groupKey) bool {
		return k.eventID == 10
	})
	p.MaybeSweep()
	assertRetained(t, filepath.Join(dir, "10.0.out"))
	assertPruned(t, filepath.Join(dir, "20.0.out"))
}

// TestPrune_StaleScan_NoDoubleDecrement guards the
// removeStreamLocked accounting: when a file disappears between scan
// and remove (stat-locked path), p.total must not be decremented for
// it. Hand-rolled by faking a stale groupInfo whose file never existed.
func TestPrune_StaleScan_NoDoubleDecrement(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "1.0.out"), 100)
	p := setupSeededPruner(t, dir, 1024)
	startTotal := p.Total()
	stale := groupInfo{
		key:     groupKey{eventID: 999, hookIndex: 0},
		outPath: filepath.Join(dir, "999.0.out"),
		outSize: 1234,
	}
	p.mu.Lock()
	p.removeStreamLocked(stale.outPath, stale.outSize)
	p.mu.Unlock()
	if got := p.Total(); got != startTotal {
		t.Fatalf("stale missing-file delete decremented total: %d -> %d", startTotal, got)
	}
}

// TestPrune_ConcurrentSweep_TotalMatchesDisk pins the spec contract
// that two finishers calling MaybeSweep concurrently never corrupt the
// running total. The deletion phase is serialized under p.mu, so the
// post-condition is that p.total equals the sum of bytes still on disk.
func TestPrune_ConcurrentSweep_TotalMatchesDisk(t *testing.T) {
	dir := t.TempDir()
	const groups = 12
	const perStream = 100
	for i := 1; i <= groups; i++ {
		writeHookLogs(t, dir, i, 0, perStream, perStream)
	}
	p := setupSeededPruner(t, dir, 400)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { ; p.MaybeSweep() })
	}
	wg.Wait()

	var diskBytes int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		diskBytes += info.Size()
	}
	if got := p.Total(); got != diskBytes {
		t.Fatalf("p.Total()=%d, disk=%d (concurrent sweepers got out of sync)", got, diskBytes)
	}
	if diskBytes > 400 {
		t.Fatalf("disk bytes %d > cap 400 after sweeps", diskBytes)
	}
}

func mustWrite(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), n), 0o600); err != nil {
		t.Fatal(err)
	}
}

// setupSeededPruner builds a pruner with a discarded-log writer and
// runs Seed, failing the test if seeding errors.
func setupSeededPruner(t *testing.T, dir string, capBytes int64) *pruner {
	t.Helper()
	p := newPruner(dir, capBytes, log.New(&bytes.Buffer{}, "", 0))
	if err := p.Seed(); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeHookLogs writes the .out and .err pair for a (eventID, hookIndex)
// group with the given byte sizes.
func writeHookLogs(t *testing.T, dir string, eventID, hookIndex, outSize, errSize int) {
	t.Helper()
	mustWrite(t, filepath.Join(dir, fmt.Sprintf("%d.%d.out", eventID, hookIndex)), outSize)
	mustWrite(t, filepath.Join(dir, fmt.Sprintf("%d.%d.err", eventID, hookIndex)), errSize)
}

// assertPruned fails the test if path still exists on disk.
func assertPruned(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("expected %s to be pruned, but it still exists", path)
	}
}

// assertRetained fails the test if path does not exist on disk.
func assertRetained(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to be retained: %v", path, err)
	}
}
