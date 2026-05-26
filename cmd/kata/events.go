package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newEventsCmd() *cobra.Command {
	var (
		tail         bool
		projectIDArg int64
		allProjects  bool
		afterID      int64
		lastEventID  int64
		limit        int
	)
	cmd := &cobra.Command{
		Use:   "events",
		Short: "list or stream events",
		Long: `kata events lists recent events. With --tail, it streams them live over SSE.

Without --tail, prints up to --limit events ordered by id ASC and exits.
With --tail, opens an SSE connection and emits one NDJSON envelope per
line. The stream reconnects with exponential backoff on disconnect and
runs until SIGINT/SIGTERM.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if allProjects && projectIDArg != 0 {
				return &cliError{Message: "--all-projects and --project-id are mutually exclusive", Kind: kindUsage, ExitCode: ExitUsage}
			}
			if strings.TrimSpace(flags.Project) != "" && (allProjects || projectIDArg != 0) {
				return &cliError{Message: "--project cannot be combined with --all-projects or --project-id", Kind: kindUsage, ExitCode: ExitUsage}
			}
			if tail {
				// One-shot-only flags must reject under --tail so users
				// don't think `--tail --limit 1` will stream just one
				// event (it streams indefinitely; --limit is poll-only).
				// hammer-test finding #6.
				if cmd.Flags().Changed("limit") {
					return &cliError{Message: "--limit applies only to one-shot mode (drop --tail to use it)", Kind: kindUsage, ExitCode: ExitUsage}
				}
				if cmd.Flags().Changed("after") {
					return &cliError{Message: "--after applies only to one-shot mode (use --last-event-id with --tail)", Kind: kindUsage, ExitCode: ExitUsage}
				}
				if lastEventID < 0 {
					return &cliError{Message: "--last-event-id must be a non-negative integer", Kind: kindUsage, ExitCode: ExitUsage}
				}
				return runEventsTail(cmd, eventsTailOptions{
					ProjectIDArg: projectIDArg,
					AllProjects:  allProjects,
					LastEventID:  lastEventID,
				})
			}
			if cmd.Flags().Changed("last-event-id") {
				return &cliError{Message: "--last-event-id applies only to --tail mode (use --after for one-shot)", Kind: kindUsage, ExitCode: ExitUsage}
			}
			if afterID < 0 {
				return &cliError{Message: "--after must be a non-negative integer", Kind: kindUsage, ExitCode: ExitUsage}
			}
			if limit <= 0 {
				return &cliError{Message: "--limit must be a positive integer", Kind: kindValidation, ExitCode: ExitValidation}
			}
			return runEventsPoll(cmd, eventsPollOptions{
				ProjectIDArg: projectIDArg,
				AllProjects:  allProjects,
				AfterID:      afterID,
				Limit:        limit,
			})
		},
	}
	cmd.Flags().BoolVar(&tail, "tail", false, "stream events live over SSE")
	cmd.Flags().Int64Var(&projectIDArg, "project-id", 0, "scope to a specific project id")
	cmd.Flags().BoolVar(&allProjects, "all-projects", false, "use the cross-project endpoint")
	cmd.Flags().Int64Var(&afterID, "after", 0, "polling cursor (one-shot mode)")
	cmd.Flags().Int64Var(&lastEventID, "last-event-id", 0, "resume cursor (--tail mode)")
	cmd.Flags().IntVar(&limit, "limit", 100, "max rows in one-shot mode")
	return cmd
}

type eventsPollOptions struct {
	ProjectIDArg int64
	AllProjects  bool
	AfterID      int64
	Limit        int
}

func runEventsPoll(cmd *cobra.Command, opts eventsPollOptions) error {
	ctx := cmd.Context()
	baseURL, err := ensureDaemon(ctx)
	if err != nil {
		return err
	}
	client, err := httpClientFor(ctx, baseURL)
	if err != nil {
		return err
	}
	url, err := pollURL(ctx, baseURL, opts)
	if err != nil {
		return err
	}
	status, bs, err := httpDoJSON(ctx, client, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if status >= 400 {
		return apiErrFromBody(status, bs)
	}
	mode := currentOutputMode()
	if mode == outputJSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	if mode == outputAgent {
		return printEventsAgent(cmd, bs)
	}
	return printEventsHuman(cmd, bs)
}

func pollURL(ctx context.Context, baseURL string, opts eventsPollOptions) (string, error) {
	switch {
	case opts.AllProjects:
		return fmt.Sprintf("%s/api/v1/events?after_id=%d&limit=%d", baseURL, opts.AfterID, opts.Limit), nil
	case opts.ProjectIDArg != 0:
		return fmt.Sprintf("%s/api/v1/projects/%d/events?after_id=%d&limit=%d",
			baseURL, opts.ProjectIDArg, opts.AfterID, opts.Limit), nil
	default:
		start, err := resolveStartPath(flags.Workspace)
		if err != nil {
			return "", err
		}
		pid, err := resolveProjectID(ctx, baseURL, start)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s/api/v1/projects/%d/events?after_id=%d&limit=%d",
			baseURL, pid, opts.AfterID, opts.Limit), nil
	}
}

func printEventsHuman(cmd *cobra.Command, bs []byte) error {
	var b struct {
		ResetRequired bool  `json:"reset_required"`
		ResetAfterID  int64 `json:"reset_after_id"`
		Events        []struct {
			EventID      int64   `json:"event_id"`
			Type         string  `json:"type"`
			ProjectID    int64   `json:"project_id"`
			IssueShortID *string `json:"issue_short_id"`
			Actor        string  `json:"actor"`
			CreatedAt    string  `json:"created_at"`
		} `json:"events"`
		NextAfterID int64 `json:"next_after_id"`
	}
	if err := json.Unmarshal(bs, &b); err != nil {
		return err
	}
	if b.ResetRequired {
		_, err := fmt.Fprintf(cmd.OutOrStdout(),
			"reset_required: refetch state and resume from %d\n", b.ResetAfterID)
		return err
	}
	for _, e := range b.Events {
		issueStr := "-"
		if e.IssueShortID != nil && *e.IssueShortID != "" {
			issueStr = *e.IssueShortID
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(),
			"%-6d  %-22s  proj=%-3d  %-8s  by %s  %s\n",
			e.EventID, e.Type, e.ProjectID, issueStr, e.Actor, e.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

func printEventsAgent(cmd *cobra.Command, bs []byte) error {
	var b struct {
		ResetRequired bool  `json:"reset_required"`
		ResetAfterID  int64 `json:"reset_after_id"`
		Events        []struct {
			EventID      int64   `json:"event_id"`
			Type         string  `json:"type"`
			ProjectName  string  `json:"project_name"`
			IssueShortID *string `json:"issue_short_id"`
			Actor        string  `json:"actor"`
		} `json:"events"`
		NextAfterID int64 `json:"next_after_id"`
	}
	if err := json.Unmarshal(bs, &b); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if b.ResetRequired {
		_, err := fmt.Fprintf(out, "OK events reset_required=true reset_after_id=%d\n", b.ResetAfterID)
		return err
	}
	if _, err := fmt.Fprintf(out, "OK events count=%d next_after_id=%d\n", len(b.Events), b.NextAfterID); err != nil {
		return err
	}
	for _, e := range b.Events {
		id := fmt.Sprint(e.EventID)
		project := e.ProjectName
		if err := writeAgentKVRow(out,
			agentRowField("id", id),
			agentRowField("type", e.Type),
			agentOptionalRowField("project", &project),
			agentOptionalRowField("issue", e.IssueShortID),
			agentRowField("actor", e.Actor),
		); err != nil {
			return err
		}
	}
	return nil
}

type eventsTailOptions struct {
	ProjectIDArg int64
	AllProjects  bool
	LastEventID  int64
}

const (
	tailBackoffStart = 1 * time.Second
	tailBackoffMax   = 30 * time.Second
)

// streamResult is the typed return shape from streamOnce. Exactly one of
// {Reset, Progress} is set on a successful return.
type streamResult struct {
	Reset    *streamResetSignal
	Progress streamProgress
}

type streamResetSignal struct{ newCursor int64 }
type streamProgress struct{ lastID int64 }

// resetEnvelope is the NDJSON shape emitted on sync.reset_required so
// downstream tooling can match on `reset_required:true`.
type resetEnvelope struct {
	ResetRequired bool  `json:"reset_required"`
	ResetAfterID  int64 `json:"reset_after_id"`
}

// errTerminalHTTP wraps a server-validated rejection (HTTP 4xx) so the tail
// reconnect loop bails out instead of spinning. See spec §7.2 retryable-vs-
// terminal classification.
var errTerminalHTTP = errors.New("terminal HTTP status")

func runEventsTail(cmd *cobra.Command, opts eventsTailOptions) error {
	ctx := cmd.Context()
	baseURL, err := ensureDaemon(ctx)
	if err != nil {
		return err
	}
	// SSE bodies are long-lived; httpClientFor's 5s overall timeout would
	// terminate a healthy stream on every quiet period. Use a streaming
	// client and rely on ctx cancellation.
	client, err := streamingClientFor(ctx, baseURL)
	if err != nil {
		return err
	}
	url, err := tailURL(ctx, baseURL, opts)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	mode := currentOutputMode()
	cursor := opts.LastEventID
	backoff := tailBackoffStart
	for {
		if ctx.Err() != nil {
			return nil
		}
		res, sErr := streamOnce(ctx, client, url, cursor, out, mode)
		if errors.Is(sErr, errTerminalHTTP) {
			return sErr
		}
		if sErr != nil && !flags.Quiet {
			fmt.Fprintln(os.Stderr, "kata: stream error:", sErr,
				"(reconnecting in", backoff.Round(time.Second), ")")
		}
		next, reset := applyAttemptResult(res, cursor, backoff)
		cursor = next.cursor
		if reset {
			backoff = next.backoff
			continue
		}
		if waitErr := waitBackoff(ctx, next.backoff); waitErr != nil {
			return nil
		}
		backoff = nextBackoff(next.backoff)
	}
}

type tailState struct {
	cursor  int64
	backoff time.Duration
}

// applyAttemptResult folds streamOnce's typed result into (next cursor,
// next backoff). The bool return is true iff the stream signalled a reset
// (caller should skip the backoff wait and retry immediately).
func applyAttemptResult(res streamResult, cursor int64, backoff time.Duration) (tailState, bool) {
	if res.Reset != nil {
		return tailState{cursor: res.Reset.newCursor, backoff: tailBackoffStart}, true
	}
	if res.Progress.lastID > cursor {
		return tailState{cursor: res.Progress.lastID, backoff: tailBackoffStart}, false
	}
	return tailState{cursor: cursor, backoff: backoff}, false
}

func waitBackoff(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func nextBackoff(cur time.Duration) time.Duration {
	if cur >= tailBackoffMax {
		return tailBackoffMax
	}
	doubled := cur * 2
	if doubled > tailBackoffMax {
		return tailBackoffMax
	}
	return doubled
}

// frameState accumulates the lines of a single SSE frame and drains them
// into NDJSON output when the frame terminator (blank line) arrives.
type frameState struct {
	id    string
	event string
	data  string
}

func (f *frameState) reset() { f.id, f.event, f.data = "", "", "" }
func (f *frameState) empty() bool {
	return f.id == "" && f.event == "" && f.data == ""
}

// flush writes the frame to out and returns the typed result. Callers
// must call reset() afterward (deferred by the caller, not here, so the
// return path is single-purpose). JSON/human tail output stays NDJSON;
// agent tail output uses compact one-line records.
func (f *frameState) flush(out io.Writer, mode outputMode) (streamResult, error) {
	if f.event == "sync.reset_required" {
		var r struct {
			ResetAfterID int64 `json:"reset_after_id"`
		}
		if err := json.Unmarshal([]byte(f.data), &r); err != nil {
			return streamResult{}, fmt.Errorf("parse reset frame: %w", err)
		}
		if mode == outputAgent {
			if _, err := fmt.Fprintf(out, "OK events reset_required=true reset_after_id=%d\n", r.ResetAfterID); err != nil {
				return streamResult{}, err
			}
		} else {
			env := resetEnvelope{ResetRequired: true, ResetAfterID: r.ResetAfterID}
			body, err := json.Marshal(env)
			if err != nil {
				return streamResult{}, fmt.Errorf("encode reset envelope: %w", err)
			}
			if _, err := fmt.Fprintln(out, string(body)); err != nil {
				return streamResult{}, err
			}
		}
		return streamResult{Reset: &streamResetSignal{newCursor: r.ResetAfterID}}, nil
	}
	if mode == outputAgent {
		return f.flushAgentEvent(out)
	}
	if _, err := fmt.Fprintln(out, f.data); err != nil {
		return streamResult{}, err
	}
	n, _ := strconv.ParseInt(f.id, 10, 64)
	return streamResult{Progress: streamProgress{lastID: n}}, nil
}

func (f *frameState) flushAgentEvent(out io.Writer) (streamResult, error) {
	var e struct {
		EventID      int64   `json:"event_id"`
		Type         string  `json:"type"`
		ProjectName  string  `json:"project_name"`
		IssueShortID *string `json:"issue_short_id"`
		Actor        string  `json:"actor"`
	}
	if err := json.Unmarshal([]byte(f.data), &e); err != nil {
		return streamResult{}, fmt.Errorf("parse event frame: %w", err)
	}
	id := e.EventID
	if id == 0 {
		id, _ = strconv.ParseInt(f.id, 10, 64)
	}
	progressID, _ := strconv.ParseInt(f.id, 10, 64)
	if _, err := fmt.Fprintf(out, "OK event id=%d type=%s", id, agentValue(e.Type)); err != nil {
		return streamResult{}, err
	}
	if e.IssueShortID != nil && *e.IssueShortID != "" {
		if _, err := fmt.Fprintf(out, " issue=%s", agentValue(*e.IssueShortID)); err != nil {
			return streamResult{}, err
		}
	}
	if e.ProjectName != "" {
		if _, err := fmt.Fprintf(out, " project=%s", agentValue(e.ProjectName)); err != nil {
			return streamResult{}, err
		}
	}
	if e.Actor != "" {
		if _, err := fmt.Fprintf(out, " actor=%s", agentValue(e.Actor)); err != nil {
			return streamResult{}, err
		}
	}
	if _, err := fmt.Fprintln(out); err != nil {
		return streamResult{}, err
	}
	return streamResult{Progress: streamProgress{lastID: progressID}}, nil
}

func streamOnce(ctx context.Context, client *http.Client, baseURL string, cursor int64, out io.Writer, mode outputMode) (streamResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if err != nil {
		return streamResult{Progress: streamProgress{lastID: cursor}}, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if cursor > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(cursor, 10))
	}
	resp, err := client.Do(req) //nolint:gosec // baseURL comes from daemon discovery
	if err != nil {
		return streamResult{Progress: streamProgress{lastID: cursor}}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		bs, _ := io.ReadAll(resp.Body)
		// 4xx responses are terminal: a malformed cursor, missing project,
		// or method/Accept negotiation failure will not be cured by a
		// reconnect. Wrap with errTerminalHTTP so the caller bails out.
		// 5xx responses are transient; let the caller back off and retry.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return streamResult{Progress: streamProgress{lastID: cursor}},
				fmt.Errorf("%w: http %d: %s", errTerminalHTTP, resp.StatusCode, string(bs))
		}
		return streamResult{Progress: streamProgress{lastID: cursor}},
			fmt.Errorf("http %d: %s", resp.StatusCode, string(bs))
	}
	return parseSSEStream(bufio.NewReader(resp.Body), cursor, out, mode)
}

// parseSSEStream pulls SSE frames off rd until EOF or a reset frame and
// emits each event in the caller-selected output mode.
func parseSSEStream(rd *bufio.Reader, cursor int64, out io.Writer, mode outputMode) (streamResult, error) {
	var f frameState
	progress := streamProgress{lastID: cursor}
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return streamResult{Progress: progress}, nil
			}
			return streamResult{Progress: progress}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if f.empty() {
				continue
			}
			res, ferr := f.flush(out, mode)
			f.reset()
			if ferr != nil {
				return streamResult{Progress: progress}, ferr
			}
			if res.Reset != nil {
				return res, nil
			}
			if res.Progress.lastID > 0 {
				progress = res.Progress
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, ":"):
			// comment / heartbeat — ignore
		case strings.HasPrefix(line, "id: "):
			f.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			f.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			f.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func tailURL(ctx context.Context, baseURL string, opts eventsTailOptions) (string, error) {
	switch {
	case opts.AllProjects:
		return baseURL + "/api/v1/events/stream", nil
	case opts.ProjectIDArg != 0:
		return fmt.Sprintf("%s/api/v1/events/stream?project_id=%d", baseURL, opts.ProjectIDArg), nil
	default:
		start, err := resolveStartPath(flags.Workspace)
		if err != nil {
			return "", err
		}
		pid, err := resolveProjectID(ctx, baseURL, start)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s/api/v1/events/stream?project_id=%d", baseURL, pid), nil
	}
}
