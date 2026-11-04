package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
)

// daemonLogsCmd selects local hook or native recurring dispatch history.
func daemonLogsCmd() *cobra.Command {
	var (
		hooks          bool
		cron           bool
		jobUID, reason string
		tail           bool
		limit          int
		failedOnly     bool
		eventType      string
		hookIndex      int
	)
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "read daemon logs (hook runs or cron decisions)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireCurrentHomeDaemonCommand(); err != nil {
				return err
			}
			if hooks == cron {
				return &cliError{Kind: kindUsage, ExitCode: ExitUsage, Message: "select exactly one of --hooks and --cron"}
			}
			if cron && (cmd.Flags().Changed("failed-only") || cmd.Flags().Changed("event-type") || cmd.Flags().Changed("hook-index")) || hooks && (cmd.Flags().Changed("job-uid") || cmd.Flags().Changed("reason")) {
				return &cliError{Kind: kindUsage, ExitCode: ExitUsage, Message: "log filters must match the selected stream"}
			}

			// hammer-test finding #7: --limit was silently treated as
			// "no limit" for 0 / -1, contradicting the help text. Reject
			// non-positive values with a clear validation error. Same
			// for --hook-index: -1 means "all" (per help), so anything
			// below -1 is meaningless and should fail loudly.
			if limit <= 0 {
				return &cliError{Message: "--limit must be a positive integer", Kind: kindValidation, ExitCode: ExitValidation}
			}
			if hookIndex < -1 {
				return &cliError{Message: "--hook-index must be -1 (all) or a non-negative integer", Kind: kindValidation, ExitCode: ExitValidation}
			}
			source, err := daemonLogSource(cmd.Context(), cron)
			if err != nil {
				return err
			}
			var f daemonLogFilter = &hookLogFilter{failedOnly: failedOnly, eventType: eventType, hookIndex: hookIndex}
			if cron {
				f = &cronLogFilter{jobUID: strings.ToUpper(strings.TrimSpace(jobUID)), reason: reason}
			}
			if tail {
				return runDaemonLogTail(cmd.Context(), source, cmd.OutOrStdout(), cmd.ErrOrStderr(), limit, f)
			}
			_, err = runDaemonLogOnce(cmd.Context(), source, cmd.OutOrStdout(), cmd.ErrOrStderr(), limit, f)
			return err
		},
	}
	cmd.Flags().BoolVar(&hooks, "hooks", false, "show hook run logs")
	cmd.Flags().BoolVar(&cron, "cron", false, "show native recurring dispatch logs")
	cmd.Flags().StringVar(&jobUID, "job-uid", "", "filter cron decisions by job UID")
	cmd.Flags().StringVar(&reason, "reason", "", "filter cron decisions by reason")
	cmd.Flags().BoolVar(&tail, "tail", false, "follow the active log")
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum matching records (default 100)")
	cmd.Flags().BoolVar(&failedOnly, "failed-only", false, "show only failed runs (result != ok || exit_code != 0)")
	cmd.Flags().StringVar(&eventType, "event-type", "", "filter by event type")
	cmd.Flags().IntVar(&hookIndex, "hook-index", -1, "filter by hook index (-1 = all)")
	return cmd
}

type logSource struct{ root, base string }
type daemonLogFilter interface {
	accept(map[string]jsontext.Value) bool
}
type cronLogFilter struct{ jobUID, reason string }

func (f *cronLogFilter) accept(rec map[string]jsontext.Value) bool {
	if f.jobUID != "" && jsonString(rec, "job_uid") != f.jobUID {
		return false
	}
	if f.reason == "skipped" {
		var skipped struct {
			Count int `json:"count"`
		}
		_ = json.Unmarshal(rec["skipped"], &skipped)
		return jsonString(rec, "reason") == "skipped" || skipped.Count > 0
	}
	return f.reason == "" || jsonString(rec, "reason") == f.reason
}

type hookLogFilter struct {
	failedOnly bool
	eventType  string
	hookIndex  int
}

func (f *hookLogFilter) accept(rec map[string]jsontext.Value) bool {
	return (!f.failedOnly || !isOK(rec)) &&
		(f.eventType == "" || jsonString(rec, "event_type") == f.eventType) &&
		(f.hookIndex < 0 || jsonInt(rec, "hook_index") == f.hookIndex)
}

// isOK returns true when result == "ok" and exit_code == 0.
func isOK(rec map[string]jsontext.Value) bool {
	return jsonString(rec, "result") == "ok" && jsonInt(rec, "exit_code") == 0
}

func jsonString(rec map[string]jsontext.Value, key string) string {
	var s string
	_ = json.Unmarshal(rec[key], &s)
	return s
}

func jsonInt(rec map[string]jsontext.Value, key string) int {
	var n int
	_ = json.Unmarshal(rec[key], &n)
	return n
}

// activeMark retains the read handle's identity and last complete newline.
// Snapshot fragments belong to the next poll, even across a rotation.
type activeMark struct {
	set  bool
	info os.FileInfo
	size int64
	file *os.File
}

// runDaemonLogOnce retains only the last limit matches across rotated files.
func runDaemonLogOnce(ctx context.Context, source logSource, stdout, stderr io.Writer, limit int, f daemonLogFilter) (activeMark, error) {
	return readDaemonLogSnapshot(ctx, source, stdout, stderr, limit, f, false)
}

func readDaemonLogSnapshot(ctx context.Context, source logSource, stdout, stderr io.Writer, limit int, f daemonLogFilter, pin bool) (activeMark, error) {
	var files []*os.File
	for {
		var err error
		files, err = pinLogFiles(ctx, source)
		if !errors.Is(err, errLogRotation) {
			if err != nil {
				return activeMark{}, err
			}
			break
		}
		select {
		case <-ctx.Done():
			return activeMark{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	var matches []string
	var mark activeMark
	defer func() {
		for _, file := range files {
			if file != mark.file {
				_ = file.Close()
			}
		}
	}()
	head := 0
	accept := func(line []byte) {
		if len(matches) < limit {
			matches = append(matches, string(line))
		} else if limit > 0 {
			matches[head] = string(line)
			head = (head + 1) % limit
		}
	}
	for _, file := range files {
		current, err := readMatchesFromLog(ctx, file, stderr, f, accept)
		if err != nil {
			return activeMark{}, err
		}
		mark = current
	}
	if pin && len(files) > 0 {
		mark.file = files[len(files)-1]
	}
	for i := range matches {
		writeHookLogRecord(stdout, []byte(matches[(head+i)%len(matches)]))
	}
	return mark, nil
}

// writeLine emits one log record + newline as raw bytes. Routed
// through io.Writer.Write rather than fmt.Fprintln so gosec's XSS
// taint analyzer (G705) doesn't flag file-tainted bytes traveling
// through fmt formatting verbs.
func writeLine(w io.Writer, s string) {
	_, _ = w.Write([]byte(s))
	_, _ = w.Write([]byte{'\n'})
}

func writeHookLogRecord(w io.Writer, raw []byte) {
	if currentOutputMode() != outputAgent {
		writeLine(w, string(raw))
		return
	}
	var rec map[string]jsontext.Value
	if err := json.Unmarshal(raw, &rec); err != nil {
		return
	}
	writeLine(w, formatAgentHookLogRecord(rec))
}

func formatAgentHookLogRecord(rec map[string]jsontext.Value) string {
	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("OK daemon_log")
	for _, k := range keys {
		b.WriteByte(' ')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(agentLogValue(rec[k]))
	}
	return b.String()
}

func agentLogValue(raw jsontext.Value) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return agentValue(s)
	}
	buf := raw.Clone()
	if err := buf.Compact(); err != nil {
		return agentValue(string(raw))
	}
	return agentValue(buf.String())
}

func readMatchesFromLog(ctx context.Context, file *os.File, stderr io.Writer, f daemonLogFilter, accept func([]byte)) (activeMark, error) {
	info, err := file.Stat()
	if err != nil {
		return activeMark{}, err
	}
	n, err := readLogRecords(ctx, io.LimitReader(file, info.Size()), file.Name(), stderr, f, accept)
	return activeMark{set: true, info: info, size: n}, err
}

// runDaemonLogTail prints the last `limit` matches from existing files,
// then follows the active runs.jsonl. The snapshot mark is
// passed through so followActive resumes at the exact offset the
// one-shot pass stopped consuming — rather than the file's current
// size, which can have grown between one-shot and follow.
func runDaemonLogTail(ctx context.Context, source logSource, stdout, stderr io.Writer, limit int, f daemonLogFilter) error {
	mark, err := readDaemonLogSnapshot(ctx, source, stdout, stderr, limit, f, true)
	if err != nil {
		return err
	}
	// Keep the cursor inode alive until followActive assumes ownership.
	defer func() {
		if mark.file != nil {
			_ = mark.file.Close()
		}
	}()
	active, err := awaitActiveFile(ctx, source)
	if err != nil || active == "" {
		return err
	}
	cursor := mark
	mark.file = nil
	return followActive(ctx, stdout, stderr, active, cursor, f)
}

// awaitActiveFile blocks until runs.jsonl exists (or ctx is done).
// Watches runs.jsonl specifically rather than "any file in the
// directory" — a rotated-only directory (runs.jsonl.N present, active
// missing) must NOT short-circuit, otherwise --tail would follow a
// rotated file that never gets new content.
func awaitActiveFile(ctx context.Context, source logSource) (string, error) {
	active := filepath.Join(source.root, source.base)
	for {
		switch _, statErr := os.Stat(active); {
		case statErr == nil:
			return active, nil
		case !errors.Is(statErr, fs.ErrNotExist):
			return "", statErr
		}
		select {
		case <-ctx.Done():
			return "", nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// followState carries the polling state across followActive ticks.
type followState struct {
	prevSize int64
	prevInfo os.FileInfo
	prevFile *os.File
}

func (st *followState) close() {
	if st.prevFile != nil {
		_ = st.prevFile.Close()
		st.prevFile = nil
	}
}

// followActive starts with the snapshot identity, so a rotation before the
// first poll is handled exactly like a rotation between later polls.
func followActive(ctx context.Context, stdout, stderr io.Writer, active string, mark activeMark, f daemonLogFilter) error {
	st := &followState{prevFile: mark.file}
	defer st.close()
	if mark.set {
		st.prevInfo, st.prevSize = mark.info, mark.size
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
		if err := tailTick(ctx, active, stdout, stderr, st, f); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

// tailTick stats the handle it reads, including Windows file identity.
func tailTick(ctx context.Context, active string, stdout, stderr io.Writer, st *followState, f daemonLogFilter) error {
	file, err := openLog(ctx, active)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() {
		if file != nil {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if st.prevInfo == nil || !os.SameFile(st.prevInfo, info) {
		if err := drainRotatedLog(ctx, active, stdout, stderr, st, info, f); err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errLogRotation) {
				return nil
			}
			return err
		}
		st.prevSize = 0
	} else if info.Size() < st.prevSize {
		st.prevSize = 0
	}
	st.prevInfo = info
	n, err := emitLogLines(ctx, file, active, st.prevSize, stdout, stderr, f)
	st.prevSize += n
	// Retain one handle between polls so eviction cannot recycle the cursor inode.
	st.close()
	st.prevFile, file = file, nil
	return err
}

// Drain every retained generation after the previous cursor before reading the
// pinned active file. An evicted cursor resumes from the oldest retained file.
func drainRotatedLog(ctx context.Context, active string, stdout, stderr io.Writer, st *followState, stop os.FileInfo, f daemonLogFilter) error {
	files, err := pinRotatedLogs(ctx, active, stop)
	if err != nil {
		return err
	}
	defer closePinnedLogs(files)
	start, from := 0, int64(0)
	for i, file := range files {
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if st.prevInfo != nil && os.SameFile(st.prevInfo, info) {
			start, from = i, st.prevSize
			break
		}
	}
	for i := start; i < len(files); i++ {
		if i != start {
			from = 0
		}
		if _, err := emitLogLines(ctx, files[i], files[i].Name(), from, stdout, stderr, f); err != nil {
			return err
		}
	}
	return nil
}

var errLogRotation = errors.New("log rotated during collection")

// Both snapshot and tail acquire a complete generation set before reading.
// Validation in generation order detects renames during collection; pinned
// handles remain readable even when the writer subsequently rotates them.
func pinLogFiles(ctx context.Context, source logSource) (files []*os.File, err error) {
	defer func() {
		if err != nil {
			closePinnedLogs(files)
			files = nil
		}
	}()
	paths, err := scanRunsFiles(source)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return files, err
		}
		file, err := openLog(ctx, path)
		if errors.Is(err, fs.ErrNotExist) {
			return files, errLogRotation
		}
		if err != nil {
			return files, err
		}
		files = append(files, file)
	}
	after, err := scanRunsFiles(source)
	if err != nil {
		return files, err
	}
	if !slices.Equal(paths, after) {
		return files, errLogRotation
	}
	for _, file := range files {
		pinned, err := file.Stat()
		if err != nil {
			return files, err
		}
		current, err := os.Stat(file.Name())
		if errors.Is(err, fs.ErrNotExist) {
			return files, errLogRotation
		}
		if err != nil {
			return files, err
		}
		if !os.SameFile(pinned, current) {
			return files, errLogRotation
		}
	}
	return files, nil
}

// A tail tick must drain only generations preceding its pinned active handle.
// If that handle rotated during collection, leave the cursor for the next poll.
func pinRotatedLogs(ctx context.Context, active string, stop os.FileInfo) ([]*os.File, error) {
	files, err := pinLogFiles(ctx, logSource{filepath.Dir(active), filepath.Base(active)})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errLogRotation
	}
	newest := files[len(files)-1]
	info, err := newest.Stat()
	if err != nil || !os.SameFile(info, stop) {
		closePinnedLogs(files)
		if err != nil {
			return nil, err
		}
		return nil, errLogRotation
	}
	_ = newest.Close()
	return files[:len(files)-1], nil
}

func closePinnedLogs(files []*os.File) {
	for _, file := range files {
		_ = file.Close()
	}
}

func emitNewLines(ctx context.Context, path string, from int64, stdout, stderr io.Writer, f daemonLogFilter) (int64, error) {
	file, err := openLog(ctx, path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()
	return emitLogLines(ctx, file, path, from, stdout, stderr, f)
}

func emitLogLines(ctx context.Context, file *os.File, path string, from int64, stdout, stderr io.Writer, f daemonLogFilter) (int64, error) {
	if _, err := file.Seek(from, io.SeekStart); err != nil {
		return 0, err
	}
	return readLogRecords(ctx, file, path, stderr, f, func(line []byte) { writeHookLogRecord(stdout, line) })
}

// readLogRecords is the complete-record and version boundary for both modes.
func readLogRecords(ctx context.Context, input io.Reader, path string, stderr io.Writer, f daemonLogFilter, accept func([]byte)) (int64, error) {
	reader := bufio.NewReaderSize(input, 64*1024)
	var consumed int64
	lineNo := 0
	for {
		if err := ctx.Err(); err != nil {
			return consumed, err
		}
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			consumed += int64(len(line))
			lineNo++
			content := line[:len(line)-1]
			if len(bytes.TrimSpace(content)) > 0 {
				var rec map[string]jsontext.Value
				if json.Unmarshal(content, &rec) != nil || rec == nil {
					_, _ = fmt.Fprintf(stderr, "kata: skipping malformed line %d in %s\n", lineNo, path)
				} else if version := max(jsonInt(rec, "version"), jsonInt(rec, "kata_hook_runs_version")); version > 1 {
					_, _ = fmt.Fprintf(stderr, "kata: skipping unsupported version %d record at line %d in %s\n", version, lineNo, path)
				} else if f.accept(rec) {
					accept(content)
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return consumed, nil
		}
		if err != nil {
			return consumed, err
		}
	}
}

// daemonLogSource resolves the selected local storage target once per command.
func daemonLogSource(ctx context.Context, cron bool) (logSource, error) {
	dsn, err := config.KataDSN(ctx)
	if err != nil {
		return logSource{}, err
	}
	pg, err := config.KataPostgresStorageConfig(ctx)
	if err != nil {
		return logSource{}, err
	}
	hash := config.StorageHash(dsn, pg.Schema)
	if cron {
		path, err := config.CronDispatchLogPath(hash)
		return logSource{filepath.Dir(path), filepath.Base(path)}, err
	}
	root, err := config.HookRootDir(hash)
	return logSource{root, "runs.jsonl"}, err
}

// rotatedRun is a parsed runs.jsonl.N entry: idx is N, path is the
// absolute filesystem path.
type rotatedRun struct {
	path string
	idx  int
}

func scanRunsFiles(source logSource) ([]string, error) {
	entries, err := os.ReadDir(source.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	rotateds, hasActive := classifyRunsEntries(source, entries)
	sort.Slice(rotateds, func(i, j int) bool { return rotateds[i].idx > rotateds[j].idx })
	out := make([]string, 0, len(rotateds)+1)
	for _, r := range rotateds {
		out = append(out, r.path)
	}
	if hasActive {
		out = append(out, filepath.Join(source.root, source.base))
	}
	return out, nil
}

// classifyRunsEntries splits a directory listing into rotated entries
// (runs.jsonl.N) and a hasActive flag for runs.jsonl. Anything else is
// ignored.
func classifyRunsEntries(source logSource, entries []os.DirEntry) ([]rotatedRun, bool) {
	var rotateds []rotatedRun
	var hasActive bool
	for _, e := range entries {
		name := e.Name()
		if name == source.base {
			hasActive = true
			continue
		}
		idx, ok := parseRotatedIndex(name, source.base)
		if !ok {
			continue
		}
		rotateds = append(rotateds, rotatedRun{path: filepath.Join(source.root, name), idx: idx})
	}
	return rotateds, hasActive
}

// parseRotatedIndex returns the numeric suffix of "runs.jsonl.N",
// or false if the name doesn't match.
func parseRotatedIndex(name, base string) (int, bool) {
	if !strings.HasPrefix(name, base+".") {
		return 0, false
	}
	idx, err := strconv.Atoi(strings.TrimPrefix(name, base+"."))
	if err != nil {
		return 0, false
	}
	return idx, true
}
