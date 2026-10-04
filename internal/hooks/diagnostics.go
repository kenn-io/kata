package hooks

import (
	"bufio"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"go.kenn.io/kata/internal/diagnostics"
	"go.kenn.io/kit/safefileio"
)

// Diagnostics reads the current snapshot using the same daemon PATH lookup as
// exec.Command. It neither executes hooks nor exposes their payload or secrets.
func (d *Dispatcher) Diagnostics() diagnostics.Hooks {
	out := diagnostics.Hooks{Available: true, Hooks: []diagnostics.Hook{}, QueueLength: len(d.queue), QueueCapacity: cap(d.queue), InFlight: d.inflight.Load(), Dropped: d.dropped.Load()}
	if snap := d.snapshot.Load(); snap != nil {
		for _, hook := range snap.Hooks {
			_, err := exec.LookPath(hook.Command)
			out.Hooks = append(out.Hooks, diagnostics.Hook{Index: hook.Index, Command: hook.Command, ExecutableAvailable: err == nil, WorkingDirectoryAvailable: workingDirectoryAvailable(hook.WorkingDir)})
		}
	}
	d.readRecentRuns(&out)
	return out
}

func (noopSink) Diagnostics() diagnostics.Hooks {
	return diagnostics.Hooks{Available: true, Hooks: []diagnostics.Hook{}}
}

// Synchronize with append/rotation so a record cannot be counted twice while
// its file shifts. Bound content reads; inspect older retained files only for
// size. Configuration caps retention at 100 rotated files.
func (d *Dispatcher) readRecentRuns(out *diagnostics.Hooks) {
	if d.appender == nil {
		out.HistoryIncomplete = true
		return
	}
	d.appender.mu.Lock()
	defer d.appender.mu.Unlock()
	budget := int64(8 << 20)
	now := d.deps.Now()
	for i := 0; i <= d.appender.keep; i++ {
		path := d.appender.path
		if i > 0 {
			path += fmt.Sprintf(".%d", i)
		}
		file, err := safefileio.OpenCurrentUserFile(path)
		if err != nil {
			if i == 0 || !errors.Is(err, os.ErrNotExist) {
				out.HistoryIncomplete = true
			}
			continue
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			out.HistoryIncomplete = true
			continue
		}
		if budget == 0 || i > 64 {
			// Reaching the budget is complete when no retained records
			// remain. Inspect metadata without reading more content.
			if info.Size() > 0 {
				out.HistoryTruncated = true
			}
			_ = file.Close()
			continue
		}
		limit := info.Size()
		if limit > budget {
			limit = budget
			out.HistoryTruncated = true
		}
		budget -= limit
		// Runs are appended at the end. When capped, prioritize the newest
		// records rather than filling the budget with old, out-of-window runs.
		start := info.Size() - limit
		skipPartial := false
		if start > 0 {
			var previous [1]byte
			if _, err := file.ReadAt(previous[:], start-1); err != nil {
				_ = file.Close()
				out.HistoryIncomplete = true
				continue
			}
			skipPartial = previous[0] != '\n'
		}
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			_ = file.Close()
			out.HistoryIncomplete = true
			continue
		}
		scanner := bufio.NewScanner(io.LimitReader(file, limit))
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			if skipPartial {
				skipPartial = false
				continue
			}
			countDoctorRun(scanner.Bytes(), now, out)
		}
		if scanner.Err() != nil {
			out.HistoryIncomplete = true
		}
		_ = file.Close()
	}
}

func countDoctorRun(line []byte, now time.Time, out *diagnostics.Hooks) {
	var record struct {
		Version  int    `json:"kata_hook_runs_version"`
		EndedAt  string `json:"ended_at"`
		Result   string `json:"result"`
		ExitCode *int   `json:"exit_code"`
	}
	if json.Unmarshal(line, &record) != nil || record.Version != 1 || record.ExitCode == nil {
		out.HistoryIncomplete = true
		return
	}
	end, err := time.Parse(time.RFC3339Nano, record.EndedAt)
	if err != nil || record.Result == "" {
		out.HistoryIncomplete = true
		return
	}
	switch record.Result {
	case "ok", "spawn_failed", "working_dir_missing", "timed_out", "daemon_shutdown":
	default:
		out.HistoryIncomplete = true
		return
	}
	if end.Before(now.Add(-7*24*time.Hour)) || end.After(now) {
		return
	}
	out.RecentRuns++
	if record.Result != "ok" || *record.ExitCode != 0 {
		out.RecentFailures++
	}
}
