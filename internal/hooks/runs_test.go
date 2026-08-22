package hooks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestRunsAppender(t *testing.T, threshold int64, keep int) (*runsAppender, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	app, err := newRunsAppender(path, threshold, keep)
	if err != nil {
		t.Fatalf("newRunsAppender: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app, path
}

func countValidRunRecords(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled path under t.TempDir()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	count := 0
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		var r runRecord
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			t.Fatalf("%s line %d not JSON: %v", path, count, err)
		}
		count++
	}
	return count
}

func TestRunsAppender_OneLinePerRun(t *testing.T) {
	app, path := newTestRunsAppender(t, 1<<20, 5)
	app.Append(runRecord{Version: 1, EventID: 1, Result: "ok"})
	app.Append(runRecord{Version: 1, EventID: 2, Result: "ok"})
	app.Append(runRecord{Version: 1, EventID: 3, Result: "ok"})
	if count := countValidRunRecords(t, path); count != 3 {
		t.Fatalf("got %d lines, want 3", count)
	}
}

func TestRunsAppender_RotatesAtThreshold(t *testing.T) {
	// 1KB threshold; each runRecord is well over 100B, so a few writes rotate.
	app, path := newTestRunsAppender(t, 1024, 3)
	for i := range 50 {
		app.Append(runRecord{Version: 1, EventID: int64(i), Result: "ok",
			HookCommand: "/usr/local/bin/something/longer"})
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("active file missing: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated .1 missing: %v", err)
	}
}

func TestRunsAppender_KeepsAtMostKeepFiles(t *testing.T) {
	app, path := newTestRunsAppender(t, 256, 2) // keep .1 and .2
	for i := range 200 {
		app.Append(runRecord{Version: 1, EventID: int64(i), Result: "ok",
			HookCommand: "/usr/local/bin/notify"})
	}
	for _, n := range []string{".1", ".2"} {
		st, err := os.Stat(path + n)
		if err != nil {
			t.Fatalf("expected %s to exist: %v", path+n, err)
		}
		if st.Size() == 0 {
			t.Fatalf("%s is empty; rotation produced an empty file", path+n)
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Fatal("runs.jsonl.3 should have been dropped")
	}
}

func TestRunsAppender_ConcurrentAppends_NoInterleave(t *testing.T) {
	// Sized so the workload produces at most `keep` rotations, so every
	// file the appender created is still on disk for validation. With
	// each runRecord ~250B serialized, 8 writers * 25 records = ~50KB
	// of writes against a 16KB threshold yields ~3 rotations < keep=5.
	const totalRecords = 25
	app, path := newTestRunsAppender(t, 16*1024, 5)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range totalRecords {
				app.Append(runRecord{Version: 1, EventID: int64(id*1000 + i), Result: "ok",
					HookCommand: "/usr/local/bin/something/longer"})
			}
		}(w)
	}
	wg.Wait()
	files := []string{path}
	for i := 1; i <= 5; i++ {
		if _, err := os.Stat(fmt.Sprintf("%s.%d", path, i)); err == nil {
			files = append(files, fmt.Sprintf("%s.%d", path, i))
		}
	}
	if len(files) < 2 {
		t.Fatalf("expected rotation to produce at least one .N file, only saw active")
	}
	// Every produced file (active + every rotation that survived) must
	// contain only well-formed JSON lines. Combined with keep=5 and a
	// volume capped under 5 rotation windows, this means *no* file the
	// appender wrote was evicted before assertion.
	totalLines := 0
	for _, f := range files {
		totalLines += countValidRunRecords(t, f)
	}
	if want := 8 * totalRecords; totalLines != want {
		t.Fatalf("validated %d lines across %d files, want %d (no rotated file should have been evicted)",
			totalLines, len(files), want)
	}
}
