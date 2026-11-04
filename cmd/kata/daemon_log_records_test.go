package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/jsonlog"
)

// The reader contract is shared by hook history and dispatch history: only
// newline-terminated records advance the cursor, including at snapshot handoff.
func TestLogRecordsPartialHandoff(t *testing.T) {
	base := "dispatch.jsonl"
	dir := t.TempDir()
	path := filepath.Join(dir, base)
	require.NoError(t, os.WriteFile(path, []byte(`{"version":1,"job_uid":"JOB"`), 0o600))
	var out, warnings bytes.Buffer
	mark, err := runDaemonLogOnce(t.Context(), logSource{dir, base}, &out, &warnings, 10, &cronLogFilter{})
	require.NoError(t, err)
	require.Empty(t, out.String())
	require.Empty(t, warnings.String())
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304: test-controlled log under t.TempDir()
	require.NoError(t, err)
	_, err = file.WriteString("}\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	state := &followState{prevInfo: mark.info, prevSize: mark.size}
	t.Cleanup(state.close)
	require.NoError(t, tailTick(t.Context(), path, &out, &warnings, state, &cronLogFilter{}))
	require.Contains(t, out.String(), `"job_uid":"JOB"`)
	require.Empty(t, warnings.String())
}

func TestLogRecordsRotation(t *testing.T) {
	for _, rotations := range []int{1, 2} {
		t.Run(strconv.Itoa(rotations)+" rotations", func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "dispatch.jsonl")
			require.NoError(t, os.WriteFile(path, []byte("{\"n\":1}\n"), 0o600))
			var out, warnings bytes.Buffer
			mark, err := runDaemonLogOnce(t.Context(), logSource{dir, "dispatch.jsonl"}, &out, &warnings, 10, &cronLogFilter{})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, []byte("{\"n\":1}\n{\"n\":2}\n"), 0o600))
			require.NoError(t, os.Rename(path, path+".1"))
			if rotations == 2 {
				require.NoError(t, os.Rename(path+".1", path+".2"))
				require.NoError(t, os.WriteFile(path+".1", []byte("{\"n\":3}\n"), 0o600))
			}
			require.NoError(t, os.WriteFile(path, []byte("{\"n\":4}\n"), 0o600))
			state := &followState{prevInfo: mark.info, prevSize: mark.size}
			t.Cleanup(state.close)
			require.NoError(t, tailTick(t.Context(), path, &out, &warnings, state, &cronLogFilter{}))
			require.Contains(t, out.String(), `"n":4`)
			if rotations == 1 {
				require.Equal(t, "{\"n\":1}\n{\"n\":2}\n{\"n\":4}\n", out.String())
				require.Empty(t, warnings.String())
			} else {
				require.Equal(t, "{\"n\":1}\n{\"n\":2}\n{\"n\":3}\n{\"n\":4}\n", out.String())
				require.Empty(t, warnings.String())
			}
		})
	}
	t.Run("follow seeds snapshot mark", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "runs.jsonl")
			require.NoError(t, os.WriteFile(path, []byte("{\"n\":1}\n"), 0o600))
			var out, warnings bytes.Buffer
			mark, err := runDaemonLogOnce(t.Context(), logSource{dir, "runs.jsonl"}, &out, &warnings, 10, &hookLogFilter{hookIndex: -1})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, []byte("{\"n\":1}\n{\"n\":2}\n"), 0o600))
			require.NoError(t, os.Rename(path, path+".1"))
			require.NoError(t, os.WriteFile(path, []byte("{\"n\":3}\n"), 0o600))
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- followActive(ctx, &out, &warnings, path, mark, &hookLogFilter{hookIndex: -1}) }()
			time.Sleep(250 * time.Millisecond)
			cancel()
			require.NoError(t, <-done)
			require.Equal(t, "{\"n\":1}\n{\"n\":2}\n{\"n\":3}\n", out.String())
		})
	})
}

func TestLogRecordsVersionAndMixedSkip(t *testing.T) {
	for _, tc := range []struct {
		name, base, content   string
		filter                daemonLogFilter
		limit                 int
		want, absent, warning string
	}{
		{"mixed skip filter", "dispatch.jsonl", "{\"version\":2,\"reason\":\"skipped\"}\n{\"version\":1,\"reason\":\"delivered\",\"skipped\":{\"count\":2}}\n", &cronLogFilter{reason: "skipped"}, 1, `"reason":"delivered"`, `"version":2`, "unsupported version 2 record at line 1"},
		{"hook version", "runs.jsonl", "{\"kata_hook_runs_version\":2,\"event_id\":1}\n{\"kata_hook_runs_version\":1,\"event_id\":2}\n{\"event_id\":3}\n", &hookLogFilter{hookIndex: -1}, 10, "{\"kata_hook_runs_version\":1,\"event_id\":2}\n{\"event_id\":3}\n", `"event_id":1`, "unsupported version 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tc.base)
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			for _, tail := range []bool{false, true} {
				var out, warnings bytes.Buffer
				if tail {
					_, err := emitNewLines(t.Context(), path, 0, &out, &warnings, tc.filter)
					require.NoError(t, err)
				} else {
					_, err := runDaemonLogOnce(t.Context(), logSource{dir, tc.base}, &out, &warnings, tc.limit, tc.filter)
					require.NoError(t, err)
				}
				require.NotContains(t, out.String(), tc.absent)
				require.Contains(t, out.String(), tc.want)
				require.Contains(t, warnings.String(), tc.warning)
			}
		})
	}
}

// A log reader must allow the writer's rename while its read handle is open.
// Windows requires explicit delete sharing; Unix permits rename on an open file.
func TestLogRecordsReaderAllowsWriterRotation(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("delete-sharing regression requires Windows")
	}
	path := filepath.Join(t.TempDir(), "dispatch.jsonl")
	writer, err := jsonlog.Open(t.Context(), path, 1, 2)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	reader, err := openLog(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	require.NoError(t, writer.Append(t.Context(), map[string]int{"version": 1}))
	raw, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"version":1`)
	info, err := reader.Stat()
	require.NoError(t, err)
	rotated, err := openLog(t.Context(), path+".1")
	require.NoError(t, err)
	defer func() { require.NoError(t, rotated.Close()) }()
	rotatedInfo, err := rotated.Stat()
	require.NoError(t, err)
	require.True(t, os.SameFile(info, rotatedInfo))
}

// Retention can evict the cursor; all newer generations still retained must drain.
func TestLogRecordsCursorEviction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatch.jsonl")
	writer, err := jsonlog.Open(t.Context(), path, 1, 2)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	require.NoError(t, writer.Append(t.Context(), map[string]int{"n": 1}))
	var out, warnings bytes.Buffer
	mark, err := readDaemonLogSnapshot(t.Context(), logSource{dir, "dispatch.jsonl"}, &out, &warnings, 10, &cronLogFilter{}, true)
	require.NoError(t, err)
	state := &followState{prevInfo: mark.info, prevSize: mark.size, prevFile: mark.file}
	t.Cleanup(state.close)
	for n := 2; n <= 4; n++ {
		require.NoError(t, writer.Append(t.Context(), map[string]int{"n": n}))
	}
	require.NoError(t, tailTick(t.Context(), path, &out, &warnings, state, &cronLogFilter{}))
	require.Equal(t, "{\"n\":1}\n{\"n\":3}\n{\"n\":4}\n", out.String())
}

// Rotate after the oldest record is consumed, exactly between mutable names.
// Both selectors must retain the snapshot and hand off each later record once.
type rotatingSnapshotFilter struct{ rotate func() }

func (f *rotatingSnapshotFilter) accept(map[string]jsontext.Value) bool {
	if f.rotate != nil {
		rotate := f.rotate
		f.rotate = nil
		rotate()
	}
	return true
}

func TestLogSnapshotRotationRetainsHistory(t *testing.T) {
	for _, base := range []string{"dispatch.jsonl", "runs.jsonl"} {
		for _, pin := range []bool{false, true} {
			t.Run(base+"/pin="+strconv.FormatBool(pin), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, base)
				require.NoError(t, os.WriteFile(path+".1", []byte("{\"n\":1}\n"), 0o600))
				require.NoError(t, os.WriteFile(path, []byte("{\"n\":2}\n"), 0o600))
				f := &rotatingSnapshotFilter{rotate: func() {
					require.NoError(t, os.Rename(path+".1", path+".2"))
					require.NoError(t, os.Rename(path, path+".1"))
					require.NoError(t, os.WriteFile(path, []byte("{\"n\":3}\n"), 0o600))
				}}
				var out, warnings bytes.Buffer
				mark, err := readDaemonLogSnapshot(t.Context(), logSource{dir, base}, &out, &warnings, 10, f, pin)
				require.NoError(t, err)
				require.Equal(t, "{\"n\":1}\n{\"n\":2}\n", out.String(), "snapshot must retain the pinned generations")
				state := &followState{prevInfo: mark.info, prevSize: mark.size, prevFile: mark.file}
				t.Cleanup(state.close)
				require.NoError(t, tailTick(t.Context(), path, &out, &warnings, state, &cronLogFilter{}))
				require.NoError(t, os.WriteFile(path, []byte("{\"n\":3}\n{\"n\":4}\n"), 0o600))
				require.NoError(t, tailTick(t.Context(), path, &out, &warnings, state, &cronLogFilter{}))
				require.Equal(t, "{\"n\":1}\n{\"n\":2}\n{\"n\":3}\n{\"n\":4}\n", out.String())
				require.Empty(t, warnings.String())
			})
		}
	}
}

func TestLogTailFirstTickRetainsHistory(t *testing.T) {
	for _, rotatedSnapshot := range []bool{false, true} {
		t.Run("rotated snapshot="+strconv.FormatBool(rotatedSnapshot), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "dispatch.jsonl")
			if rotatedSnapshot {
				require.NoError(t, os.WriteFile(path+".1", []byte("{\"n\":0}\n"), 0o600))
			}
			var out, warnings bytes.Buffer
			mark, err := readDaemonLogSnapshot(t.Context(), logSource{dir, "dispatch.jsonl"}, &out, &warnings, 10, &cronLogFilter{}, true)
			require.NoError(t, err)
			state := &followState{prevInfo: mark.info, prevSize: mark.size, prevFile: mark.file}
			t.Cleanup(state.close)
			if rotatedSnapshot {
				require.NoError(t, os.Rename(path+".1", path+".2"))
			}
			require.NoError(t, os.WriteFile(path+".1", []byte("{\"n\":1}\n"), 0o600))
			require.NoError(t, os.WriteFile(path, nil, 0o600))
			require.NoError(t, tailTick(t.Context(), path, &out, &warnings, state, &cronLogFilter{}))
			want := "{\"n\":1}\n"
			if rotatedSnapshot {
				want = "{\"n\":0}\n" + want
			}
			require.Equal(t, want, out.String())
			require.NoError(t, tailTick(t.Context(), path, &out, &warnings, state, &cronLogFilter{}))
			require.Equal(t, want, out.String(), "next poll must not replay retained history")
		})
	}
}
