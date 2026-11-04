package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/jsonlog"
)

// The reader contract is shared by hook history and dispatch history: only
// newline-terminated records advance the cursor, including at snapshot handoff.
func TestLogRecordsPartialHandoff(t *testing.T) {
	for _, base := range []string{"dispatch.jsonl", "runs.jsonl"} {
		t.Run(base, func(t *testing.T) {
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
		})
	}
}

func TestLogRecordsRotation(t *testing.T) {
	for _, rotations := range []int{1, 2} {
		t.Run(time.Duration(rotations).String(), func(t *testing.T) {
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
}

func TestLogRecordsRotationBeforeFirstPoll(t *testing.T) {
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
}

func TestLogRecordsVersionAndMixedSkip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatch.jsonl")
	content := "{\"version\":2,\"reason\":\"skipped\"}\n{\"version\":1,\"reason\":\"delivered\",\"skipped\":{\"count\":2}}\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	for _, tail := range []bool{false, true} {
		var out, warnings bytes.Buffer
		filter := &cronLogFilter{reason: "skipped"}
		if tail {
			_, err := emitNewLines(t.Context(), path, 0, &out, &warnings, filter)
			require.NoError(t, err)
		} else {
			_, err := runDaemonLogOnce(t.Context(), logSource{dir, "dispatch.jsonl"}, &out, &warnings, 1, filter)
			require.NoError(t, err)
		}
		require.NotContains(t, out.String(), `"version":2`)
		require.Contains(t, out.String(), `"reason":"delivered"`)
		require.Contains(t, warnings.String(), "unsupported version 2 record at line 1")
	}
}

// A log reader must allow the writer's rename while its read handle is open.
// Linux and Windows exercise the same contract with their native file sharing.
func TestLogRecordsReaderAllowsWriterRotation(t *testing.T) {
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

func TestLogRecordsHookFutureVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs.jsonl")
	writeHookLog(t, dir, "runs.jsonl", map[string]any{"kata_hook_runs_version": 2, "event_id": 1}, map[string]any{"kata_hook_runs_version": 1, "event_id": 2}, map[string]any{"event_id": 3})
	for _, tail := range []bool{false, true} {
		var out, warnings bytes.Buffer
		filter := &hookLogFilter{hookIndex: -1}
		if tail {
			_, err := emitNewLines(t.Context(), path, 0, &out, &warnings, filter)
			require.NoError(t, err)
		} else {
			_, err := runDaemonLogOnce(t.Context(), logSource{dir, "runs.jsonl"}, &out, &warnings, 10, filter)
			require.NoError(t, err)
		}
		require.NotContains(t, out.String(), `"event_id":1`)
		require.Contains(t, out.String(), `"event_id":2`)
		require.Contains(t, out.String(), `"event_id":3`)
		require.Contains(t, warnings.String(), "unsupported version 2")
	}
}
