package main

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/shortid"
	kataclient "go.kenn.io/kata/pkg/client"
)

const (
	moveBatchMaxItems = 5000
	moveBatchWorkers  = 6
)

type moveBatchRow struct{ Ref, Target string }
type moveBatchPrepared struct {
	issue  moveIssueWire
	target projectRef
	pid    int64
	ref    string
}
type moveBatchSummary struct {
	Moved       int `json:"moved"`
	Refused     int `json:"refused"`
	Unknown     int `json:"unknown"`
	Unattempted int `json:"unattempted"`
}
type moveBatchResult struct {
	Ref      string `json:"ref"`
	UID      string `json:"uid,omitempty"`
	Target   string `json:"target,omitempty"`
	Result   string `json:"result"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
	Hint     string `json:"hint,omitempty"`
	To       string `json:"to,omitempty"`
	EventID  int64  `json:"event_id,omitempty,omitzero"`
	sourceID int64
	targetID int64
}

func moveBatchUsage(format string, args ...any) error {
	return &cliError{Message: fmt.Sprintf(format, args...), Kind: kindUsage, ExitCode: ExitUsage}
}

// parseRefsFrom reads one `ref` or `ref<TAB>target` row per line.
func parseRefsFrom(r io.Reader, sharedTarget string) ([]moveBatchRow, error) {
	scanner := bufio.NewScanner(r)
	var rows []moveBatchRow
	for line := 1; len(rows) <= moveBatchMaxItems && scanner.Scan(); line++ {
		text := scanner.Text()
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
		results[i] = moveBatchResult{Ref: row.Ref, Result: "unattempted", Hint: "not sent; safe to rerun"}
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
	var halt atomic.Bool
	prepared := make([]*moveBatchPrepared, len(rows))
	runMoveBatchJobs(a, &halt, len(rows), func(i int) {
		prepared[i] = resolveMoveBatchOne(a, client, projects, rows[i], &results[i])
	})
	groups := make(map[string][]int)
	for i, p := range prepared {
		if p != nil {
			groups[p.issue.UID] = append(groups[p.issue.UID], i)
		}
	}
	for _, indexes := range groups {
		conflict := false
		for _, i := range indexes[1:] {
			//nolint:nilaway // Group indexes only reference non-nil prepared rows.
			conflict = conflict || prepared[i].target.ID != prepared[indexes[0]].target.ID
		}
		for n, i := range indexes {
			if conflict || n > 0 {
				results[i].Result, results[i].Code, results[i].Hint = "refused", "duplicate_ref", ""
				results[i].Message = "issue appears more than once"
				if conflict {
					results[i].Message = "issue has conflicting target projects"
				}
				prepared[i] = nil
			}
		}
	}
	runMoveBatchJobs(a, &halt, len(rows), func(i int) {
		if prepared[i] != nil {
			moveBatchOne(a, client, prepared[i], &results[i], actor, subtreeSupported, &halt)
		}
	})
	var commentErrs []error
	for i := range results {
		r := &results[i]
		if r.Result != "moved" || comment == "" {
			continue
		}
		e := a.ctx.Err()
		if e == nil {
			e = postFollowupComment(a.ctx, a.client, a.baseURL, r.targetID, r.UID, actor, comment, handle)
		}
		if e != nil {
			detail := strings.Replace(e.Error(), " (retry with: kata comment "+r.UID+" --body ...)", "", 1)
			r.Message = strings.TrimSpace(r.Message + " comment failed: " + detail)
			r.Hint = fmt.Sprintf("retry with: kata comment %s --project %s --body ...", r.UID, shellQuote(r.Target))
			commentErrs = append(commentErrs, fmt.Errorf("comment for %s failed: %s; %s", r.UID, detail, r.Hint))
		}
	}
	printErr := printMoveBatch(cmd, results)
	summary := summarizeMoveBatch(results)
	var outcome error
	if a.ctx.Err() != nil {
		msg := fmt.Sprintf("batch move cancelled; %d rows not attempted", summary.Unattempted)
		if summary.Unknown > 0 {
			msg = "batch move cancelled; check unknown rows before retrying"
		}
		outcome = &cliError{Message: msg, Kind: kindDaemonUnavail, ExitCode: ExitDaemonUnavail}
	} else if summary.Unknown > 0 {
		outcome = &cliError{Message: "check unknown rows before retrying", Kind: kindDaemonUnavail, ExitCode: ExitDaemonUnavail}
	} else if summary.Unattempted > 0 {
		outcome = &cliError{Message: fmt.Sprintf("%d rows not attempted", summary.Unattempted), Kind: kindDaemonUnavail, ExitCode: ExitDaemonUnavail}
	} else if summary.Refused > 0 {
		outcome = moveBatchRefusalError(printErr)
	}
	if len(commentErrs) > 0 {
		combined := errors.Join(outcome, errors.Join(commentErrs...))
		cli := *cliErrorForErr(combined, true)
		cli.Code = ""
		cli.Message = strings.TrimSpace(combined.Error())
		return errors.Join(&cli, printErr)
	}
	return errors.Join(outcome, printErr)
}

func runMoveBatchJobs(a daemonAPI, halt *atomic.Bool, count int, job func(int)) {
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range moveBatchWorkers {
		wg.Go(func() {
			for i := range jobs {
				job(i)
			}
		})
	}
	for i := range count {
		if a.ctx.Err() != nil || halt.Load() {
			break
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}

func resolveMoveBatchOne(a daemonAPI, client *kataclient.Client, projects []projectRef, row moveBatchRow, result *moveBatchResult) *moveBatchPrepared {
	if a.ctx.Err() != nil {
		return nil
	}
	refuse := func(code string, err error) {
		if a.ctx.Err() == nil {
			result.Result, result.Code, result.Message, result.Hint = "refused", code, err.Error(), ""
		}
	}
	parsed, err := shortid.Parse(row.Ref)
	if err != nil || (parsed.ULID == "" && parsed.Project == "") {
		refuse("invalid_ref", errors.New("use project#id or the issue UID"))
		return nil
	}
	target, err := resolveProjectSelectorFromRefs(strings.TrimSpace(row.Target), projects)
	if err != nil {
		refuse("invalid_target", err)
		return nil
	}
	if target.UID == "" {
		refuse("invalid_target", fmt.Errorf("project %q has no UID", target.Name))
		return nil
	}
	result.Target, result.targetID = target.Name, target.ID
	pid, ref := target.ID, parsed.ULID
	if parsed.Project != "" {
		source, e := resolveProjectSelectorFromRefs(parsed.Project, projects)
		if e != nil {
			refuse("invalid_ref", e)
			return nil
		}
		pid, ref = source.ID, parsed.ShortID
	}
	var issue moveIssueWire
	if parsed.ULID != "" {
		issue, err = fetchIssueByUID(a.ctx, client, parsed.ULID)
		pid = issue.ProjectID
	} else {
		issue, err = fetchMoveIssue(a.ctx, a.client, a.baseURL, pid, ref)
	}
	if err != nil {
		if _, ok := errors.AsType[*url.Error](err); ok {
			result.Message = err.Error()
		} else {
			refuse("issue_lookup", err)
		}
		return nil
	}
	result.UID, result.sourceID = issue.UID, issue.ProjectID
	return &moveBatchPrepared{issue: issue, target: target, pid: pid, ref: ref}
}

func moveBatchOne(a daemonAPI, client *kataclient.Client, prepared *moveBatchPrepared, result *moveBatchResult, actor string, subtreeSupported bool, halt *atomic.Bool) {
	issue, target, pid, ref := prepared.issue, prepared.target, prepared.pid, prepared.ref
	if a.ctx.Err() != nil || halt.Load() {
		return
	}
	request := moveRequest(pid, ref, actor, target.UID, issue.Revision, false, subtreeSupported)
	var dispatched atomic.Bool
	ctx := httptrace.WithClientTrace(a.ctx, &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { dispatched.Store(true) }})
	response, callErr := client.MoveIssueWithResponse(ctx, request)
	unknown := func() {
		halt.Store(true)
		result.Result = "unknown"
		result.Hint = fmt.Sprintf("outcome unconfirmed; run kata show %s --json to see the current issue.project_id. Source project ID: %d; target project ID: %d. Check move history before retrying if the issue may have moved more than once.", shellQuote(result.UID), result.sourceID, result.targetID)
	}
	if response == nil {
		if !dispatched.Load() {
			if callErr != nil && a.ctx.Err() == nil {
				result.Message = callErr.Error()
			}
			return
		}
		unknown()
		return
	}
	if response.StatusCode >= 500 && response.StatusCode <= 599 {
		unknown()
		return
	}
	if response.StatusCode != 200 {
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			result.Result, result.Code, result.Message, result.Hint = "refused", "move_refused", err.Error(), ""
			return
		}
	}
	var moved moveResponseWire
	if err := json.Unmarshal(response.Body, &moved); err != nil || !moveBatchComplete(moved, target.ID) {
		unknown()
		return
	}
	result.Result, result.Hint = "moved", ""
	result.To, result.EventID = moveQualifiedID(target.Name, moved.Issue.ShortID), moved.EventID
}

func moveBatchComplete(moved moveResponseWire, targetID int64) bool {
	return moved.Changed && moved.EventID != 0 && moved.Issue.ProjectID == targetID && moved.Issue.ShortID != "" && moved.NewShortID == moved.Issue.ShortID
}

func moveBatchRefusalError(printErr error) error {
	err := &cliError{Message: "some issues were refused", Kind: kindConflict, ExitCode: ExitConflict}
	if printErr != nil {
		err.Message += ": " + printErr.Error()
	} else if currentOutputMode() == outputHuman {
		err.Message = ""
	}
	return err
}

func summarizeMoveBatch(results []moveBatchResult) (summary moveBatchSummary) {
	for _, r := range results {
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
	return summary
}

func printMoveBatch(cmd *cobra.Command, results []moveBatchResult) error {
	summary := summarizeMoveBatch(results)
	out := cmd.OutOrStdout()
	if currentOutputMode() == outputJSON {
		return json.MarshalWrite(out, map[string]any{"includes_descendants": false, "summary": summary, "results": results}, jsontext.AllowInvalidUTF8(true))
	}
	quiet := flags.Quiet && currentOutputMode() == outputHuman
	var rows strings.Builder
	for _, r := range results {
		if quiet && r.Result == "moved" && r.Message == "" && r.Hint == "" {
			continue
		}
		_ = writeAgentKVRow(&rows, agentRowField("ref", r.Ref), agentRowField("result", r.Result),
			agentOptionalRowField("uid", &r.UID), agentOptionalRowField("target", &r.Target), agentOptionalRowField("to", &r.To),
			agentOptionalRowField("code", &r.Code), agentOptionalRowField("message", &r.Message), agentOptionalRowField("hint", &r.Hint))
	}
	if quiet && rows.Len() == 0 {
		return nil
	}
	header := fmt.Sprintf("Move batch (listed issues only; descendants excluded): %d moved, %d refused, %d unknown, %d unattempted\n", summary.Moved, summary.Refused, summary.Unknown, summary.Unattempted)
	if currentOutputMode() == outputAgent {
		header = fmt.Sprintf("OK move-batch requested=%d moved=%d refused=%d unknown=%d unattempted=%d includes_descendants=false\n", len(results), summary.Moved, summary.Refused, summary.Unknown, summary.Unattempted)
	}
	_, err := io.WriteString(out, header+rows.String())
	return err
}
