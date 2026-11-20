package main

import (
	"bufio"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http/httptrace"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/shortid"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

const (
	moveBatchMaxItems     = 5000
	moveBatchMaxFileBytes = 8 * 1024 * 1024
	moveBatchWorkers      = 6
)

type moveBatchRow struct{ Ref, Target string }
type moveBatchResult struct {
	Index      int    `json:"index"`
	Ref        string `json:"ref"`
	UID        string `json:"uid,omitempty"`
	Target     string `json:"target,omitempty"`
	Result     string `json:"result"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message,omitempty"`
	Hint       string `json:"hint,omitempty"`
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
	NewShortID string `json:"new_short_id,omitempty"`
	EventID    int64  `json:"event_id,omitempty,omitzero"`
	source     string
	sourceID   int64
	targetID   int64
}

func moveBatchUsage(format string, args ...any) error {
	return &cliError{Message: fmt.Sprintf(format, args...), Kind: kindUsage, ExitCode: ExitUsage}
}

// parseRefsFrom reads one `ref` or `ref<TAB>target` row per line.
func parseRefsFrom(r io.Reader, sharedTarget string) ([]moveBatchRow, error) {
	limited := &io.LimitedReader{R: r, N: moveBatchMaxFileBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), moveBatchMaxFileBytes+2)
	var rows []moveBatchRow
	for line := 1; len(rows) <= moveBatchMaxItems && scanner.Scan(); line++ {
		text := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.TrimSpace(text) == "" {
			continue
		}
		fields := strings.Split(text, "\t")
		row := moveBatchRow{Ref: strings.TrimSpace(fields[0]), Target: sharedTarget}
		if len(fields) == 2 {
			row.Target = strings.TrimSpace(fields[1])
		}
		if len(fields) > 2 || row.Ref == "" || row.Target == "" {
			return nil, moveBatchUsage("refs file line %d: expected ref or ref<TAB>project", line)
		}
		rows = append(rows, row)
	}
	if limited.N == 0 {
		return nil, moveBatchUsage("refs file exceeds %d bytes; split the input into separate runs", moveBatchMaxFileBytes)
	}
	if scanner.Err() != nil {
		return nil, moveBatchUsage("read refs file: %v", scanner.Err())
	}
	return validateMoveBatchRows(rows)
}

func readMoveBatchRows(args []string, refsFrom string) ([]moveBatchRow, error) {
	shared := ""
	if len(args) > 0 {
		shared = args[len(args)-1]
	}
	if refsFrom != "" {
		if len(args) > 1 {
			return nil, moveBatchUsage("--refs-from accepts only a target project argument")
		}
		//nolint:gosec // G304: --refs-from names the local file to read.
		file, err := os.Open(refsFrom)
		if err != nil {
			return nil, moveBatchUsage("%v", err)
		}
		defer func() { _ = file.Close() }()
		return parseRefsFrom(file, shared)
	}
	if len(args) < 2 {
		return nil, moveBatchUsage("move requires issue refs and a target project")
	}
	var rows []moveBatchRow
	for _, ref := range args[:len(args)-1] {
		rows = append(rows, moveBatchRow{Ref: ref, Target: shared})
	}
	return validateMoveBatchRows(rows)
}

func validateMoveBatchRows(rows []moveBatchRow) ([]moveBatchRow, error) {
	if len(rows) > moveBatchMaxItems {
		return nil, moveBatchUsage("batch move accepts at most %d items; split the input into separate runs", moveBatchMaxItems)
	}
	return rows, nil
}

func runMoveBatch(cmd *cobra.Command, args []string, refsFrom string) error {
	rows, err := readMoveBatchRows(args, refsFrom)
	if err != nil {
		return err
	}
	results := make([]moveBatchResult, len(rows))
	for i, row := range rows {
		results[i] = moveBatchResult{Index: i, Ref: row.Ref, Result: "unattempted", Hint: "not sent; safe to rerun"}
	}
	if len(rows) == 0 {
		return printMoveBatch(cmd, results)
	}
	comment, handle, err := prepareFollowupComment(cmd)
	if err != nil {
		return errors.Join(err, printMoveBatch(cmd, results))
	}
	a, err := dialDaemon(cmd.Context())
	if err != nil {
		return errors.Join(err, printMoveBatch(cmd, results))
	}
	projects, err := loadProjectRefs(a)
	if err != nil {
		return errors.Join(err, printMoveBatch(cmd, results))
	}
	client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
	if err != nil {
		return errors.Join(err, printMoveBatch(cmd, results))
	}
	subtreeSupported := true
	if err := requireDaemonAPIVersion(a.ctx, a.client, a.baseURL, apiVersionMoveSubtree, "subtree move"); err != nil {
		compatibility, old := errors.AsType[*cliError](err)
		if !old || compatibility.Code != "daemon_api_too_old" {
			return errors.Join(err, printMoveBatch(cmd, results))
		}
		subtreeSupported = false
	}
	actor, _ := resolveActor(a.ctx, flags.As, nil)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range moveBatchWorkers {
		wg.Go(func() {
			for i := range jobs {
				moveBatchOne(a, client, projects, rows[i], &results[i], actor, subtreeSupported)
			}
		})
	}
dispatch:
	for i := range rows {
		if a.ctx.Err() != nil {
			break
		}
		select {
		case jobs <- i:
		case <-a.ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()
	var commentErrs []error
	for i := range results {
		r := &results[i]
		if r.Result != "moved" || comment == "" {
			continue
		}
		var e error
		if a.ctx.Err() != nil {
			e = a.ctx.Err()
		} else {
			e = postFollowupComment(a.ctx, a.client, a.baseURL, r.targetID, r.UID, actor, comment, handle)
		}
		if e != nil {
			retry := fmt.Sprintf("kata comment %s --project %s --body ...", r.UID, shellQuote(r.Target))
			if a.ctx.Err() != nil {
				commentErrs = append(commentErrs, fmt.Errorf("comment for %s failed: %w; retry with: %s", r.UID, e, retry))
			} else {
				commentErrs = append(commentErrs, errors.New(strings.Replace(e.Error(), "kata comment "+r.UID+" --body ...", retry, 1)))
			}
		}
	}
	printErr := printMoveBatch(cmd, results)
	var unknown, unattempted, refused int
	for _, r := range results {
		switch r.Result {
		case "unknown":
			unknown++
		case "unattempted":
			unattempted++
		case "refused":
			refused++
		}
	}
	var outcome error
	if a.ctx.Err() != nil {
		msg := fmt.Sprintf("batch move cancelled; %d rows not attempted", unattempted)
		if unknown > 0 {
			msg = "batch move cancelled; check unknown rows before retrying"
		}
		outcome = &cliError{Message: msg, Kind: kindDaemonUnavail, ExitCode: ExitDaemonUnavail}
	} else if unknown > 0 {
		outcome = &cliError{Message: "check unknown rows before retrying", Kind: kindDaemonUnavail, ExitCode: ExitDaemonUnavail}
	} else if unattempted > 0 {
		outcome = &cliError{Message: fmt.Sprintf("%d rows not attempted", unattempted), Kind: kindDaemonUnavail, ExitCode: ExitDaemonUnavail}
	} else if refused > 0 {
		outcome = moveBatchRefusalError()
	}
	return errors.Join(outcome, printErr, errors.Join(commentErrs...))
}

func moveBatchOne(a daemonAPI, client *kataclient.Client, projects []projectRef, row moveBatchRow, result *moveBatchResult, actor string, subtreeSupported bool) {
	if a.ctx.Err() != nil {
		return
	}
	refuse := func(code string, err error) {
		if a.ctx.Err() == nil {
			result.Result, result.Code, result.Message, result.Hint = "refused", code, err.Error(), ""
		}
	}
	if !utf8.ValidString(row.Ref) {
		result.Ref = strconv.Quote(row.Ref)
		refuse("invalid_ref", errors.New("issue ref must be valid UTF-8"))
		return
	}
	if !utf8.ValidString(row.Target) {
		refuse("invalid_target", errors.New("target project must be valid UTF-8"))
		return
	}
	parsed, err := shortid.Parse(row.Ref)
	if err != nil || (parsed.ULID == "" && parsed.Project == "") {
		refuse("invalid_ref", errors.New("use project#id or the issue UID"))
		return
	}
	target, err := resolveProjectSelectorFromRefs(strings.TrimSpace(row.Target), projects)
	if err != nil {
		refuse("invalid_target", err)
		return
	}
	if target.UID == "" {
		refuse("invalid_target", fmt.Errorf("project %q has no UID", target.Name))
		return
	}
	result.Target, result.targetID = target.Name, target.ID
	pid, ref := target.ID, parsed.ULID
	if parsed.Project != "" {
		source, e := resolveProjectSelectorFromRefs(parsed.Project, projects)
		if e != nil {
			refuse("invalid_ref", e)
			return
		}
		pid, ref = source.ID, parsed.ShortID
	}
	var issue moveIssueWire
	if parsed.ULID != "" {
		response, callErr := client.ShowIssueByUIDWithResponse(a.ctx, &generated.ShowIssueByUIDRequestOptions{PathParams: &generated.ShowIssueByUIDPath{UID: parsed.ULID}})
		if response == nil {
			err = externalCLITransportError(response, callErr)
		} else if err = externalCLIResponseError(response.StatusCode, response.Body, callErr); err == nil {
			var body struct {
				Issue moveIssueWire `json:"issue"`
			}
			err = json.Unmarshal(response.Body, &body)
			issue = body.Issue
			pid = issue.ProjectID
		}
	} else {
		issue, err = fetchMoveIssue(a.ctx, a.client, a.baseURL, pid, ref)
	}
	if err != nil {
		refuse("issue_lookup", err)
		return
	}
	result.UID = issue.UID
	result.sourceID = issue.ProjectID
	for _, p := range projects {
		if p.ID == issue.ProjectID {
			result.source = p.Name
			break
		}
	}
	if result.source == "" {
		refuse("issue_lookup", errors.New("source project not found"))
		return
	}
	if a.ctx.Err() != nil {
		return
	}
	etag := fmt.Sprintf(`"rev-%d"`, issue.Revision)
	single := true
	request := &generated.MoveIssueRequestOptions{PathParams: &generated.MoveIssuePath{ProjectID: pid, Ref: ref}, Header: &generated.MoveIssueHeaders{IfMatch: &etag}, Body: &generated.MoveIssueBody{Actor: &actor, ToProjectUID: target.UID}}
	if subtreeSupported {
		request.Body.Single = &single
	}
	var dispatched atomic.Bool
	ctx := httptrace.WithClientTrace(a.ctx, &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { dispatched.Store(true) }})
	response, callErr := client.MoveIssueWithResponse(ctx, request)
	if response == nil {
		if !dispatched.Load() {
			if callErr != nil && a.ctx.Err() == nil {
				result.Message = callErr.Error()
			}
			return
		}
		result.Result = "unknown"
		result.Hint = fmt.Sprintf("outcome unconfirmed; run kata show %s --json to see the current issue.project_id. Source project ID: %d; target project ID: %d. Check move history before retrying if the issue may have moved more than once.", shellQuote(result.UID), result.sourceID, result.targetID)
		return
	}
	if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
		result.Result, result.Code, result.Message, result.Hint = "refused", "move_refused", err.Error(), ""
		return
	}
	var moved moveResponseWire
	if err := json.Unmarshal(response.Body, &moved); err != nil {
		result.Result, result.Code, result.Message, result.Hint = "refused", "invalid_response", err.Error(), ""
		return
	}
	result.Result, result.Hint = "moved", ""
	result.From, result.To = moveQualifiedID(result.source, issue.ShortID), moveQualifiedID(target.Name, moved.Issue.ShortID)
	result.NewShortID, result.EventID = moved.NewShortID, moved.EventID
}

func moveBatchRefusalError() error {
	err := &cliError{Message: "some issues were refused", Kind: kindConflict, ExitCode: ExitConflict}
	if currentOutputMode() == outputHuman && !flags.Quiet {
		err.Code = "batch_refused_reported"
	}
	return err
}
func printMoveBatch(cmd *cobra.Command, results []moveBatchResult) error {
	if flags.Quiet && currentOutputMode() == outputHuman {
		return nil
	}
	var summary struct {
		Moved       int `json:"moved"`
		Refused     int `json:"refused"`
		Unknown     int `json:"unknown"`
		Unattempted int `json:"unattempted"`
	}
	for i := range results {
		r := &results[i]

		switch r.Result {
		case "moved":
			summary.Moved++
		case "refused":
			summary.Refused++
		case "unknown":
			summary.Unknown++
		case "unattempted":
			summary.Unattempted++
		}
	}
	out := cmd.OutOrStdout()
	if currentOutputMode() == outputJSON {
		return json.MarshalWrite(out, map[string]any{"includes_descendants": false, "summary": summary, "results": results})
	}
	var b strings.Builder
	if currentOutputMode() == outputAgent {
		fmt.Fprintf(&b, "OK move-batch requested=%d moved=%d refused=%d unknown=%d unattempted=%d includes_descendants=false\n", len(results), summary.Moved, summary.Refused, summary.Unknown, summary.Unattempted)
	} else {
		fmt.Fprintf(&b, "Move batch (listed issues only; descendants excluded): %d moved, %d refused, %d unknown, %d unattempted\n", summary.Moved, summary.Refused, summary.Unknown, summary.Unattempted)
	}
	for _, r := range results {
		fmt.Fprintf(&b, "- ref=%s result=%s", agentValue(r.Ref), r.Result)
		if r.UID != "" {
			fmt.Fprintf(&b, " uid=%s", agentValue(r.UID))
		}
		if r.Target != "" {
			fmt.Fprintf(&b, " target=%s", agentValue(r.Target))
		}
		if r.To != "" {
			fmt.Fprintf(&b, " to=%s", agentValue(r.To))
		}
		if r.Code != "" {
			fmt.Fprintf(&b, " code=%s", r.Code)
		}
		if r.Message != "" {
			fmt.Fprintf(&b, " message=%s", agentValue(r.Message))
		}
		if r.Hint != "" {
			fmt.Fprintf(&b, " hint=%s", agentValue(r.Hint))
		}
		b.WriteByte('\n')
	}
	_, err := io.WriteString(out, b.String())
	return err
}
