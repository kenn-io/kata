package main

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/shortid"
	"go.kenn.io/kata/internal/uid"
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
		text := strings.TrimPrefix(scanner.Text(), "\ufeff")
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
	defaultTarget, err := validateMoveBatchDefault(args, refsFrom, projects)
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
		var target *projectRef
		if defaultTarget != nil && rows[i].Target == args[0] {
			target = defaultTarget
		}
		prepared[i] = resolveMoveBatchOne(a, client, projects, rows[i], target, &results[i], &halt)
	})
	refuseDuplicateMoveBatchRows(prepared, results)
	runMoveBatchJobs(a, &halt, len(rows), func(i int) {
		if prepared[i] != nil {
			moveBatchOne(a, client, prepared[i], &results[i], actor, subtreeSupported, &halt)
		}
	})
	commentErrs := postMoveBatchComments(cmd, a, results, actor, comment, handle)
	printErr := printMoveBatch(cmd, results)
	summary := summarizeMoveBatch(results)
	summaryShown := summary.Unattempted > 0 && printErr == nil && currentOutputMode() == outputHuman
	var outcome error
	if a.ctx.Err() != nil {
		msg := fmt.Sprintf("batch move cancelled; %d rows not attempted", summary.Unattempted)
		if summary.Unknown > 0 {
			msg = "batch move cancelled; check unknown rows before retrying"
		} else if summaryShown {
			msg = "batch move cancelled"
		}
		outcome = &cliError{Message: msg, Kind: kindDaemonUnavail, ExitCode: ExitDaemonUnavail}
	} else if summary.Unknown > 0 {
		outcome = &cliError{Message: "check unknown rows before retrying", Kind: kindDaemonUnavail, ExitCode: ExitDaemonUnavail}
	} else if summary.Unattempted > 0 {
		msg := fmt.Sprintf("%d rows not attempted", summary.Unattempted)
		if summaryShown {
			msg = ""
		}
		outcome = &cliError{Message: msg, Kind: kindDaemonUnavail, ExitCode: ExitDaemonUnavail}
	} else if summary.Refused > 0 {
		outcome = moveBatchRefusalError(printErr)
	}
	if commentErrs != nil {
		combined := errors.Join(outcome, commentErrs)
		cli := *cliErrorForErr(combined, true)
		cli.Code = ""
		cli.Message = strings.TrimSpace(combined.Error())
		return errors.Join(&cli, printErr)
	}
	return errors.Join(outcome, printErr)
}

func postMoveBatchComments(cmd *cobra.Command, a daemonAPI, results []moveBatchResult, actor, comment, handle string) error {
	if comment == "" {
		return nil
	}
	suffix := moveBatchRecoveryFlags()
	if cmd.Flags().Changed("as") {
		suffix += " --as " + shellQuote(flags.As)
	}
	if cmd.Flags().Changed("teammate") {
		teammate, _ := cmd.Flags().GetString("teammate")
		suffix += " --teammate " + shellQuote(teammate)
	}
	errs := make([]error, len(results))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range moveBatchWorkers {
		wg.Go(func() {
			for i := range jobs {
				r := &results[i]
				key, err := uid.New()
				if err != nil {
					r.Message = "comment not sent: " + err.Error()
					errs[i] = err
					continue
				}
				err = a.ctx.Err()
				if err == nil {
					err = postFollowupCommentWithKey(a.ctx, a.client, a.baseURL, r.targetID, r.UID, actor, comment, handle, key)
				}
				if err != nil {
					detail := strings.Replace(err.Error(), " (rerun the original kata close command with the same --idempotency-key)", "", 1)
					r.Message = strings.TrimSpace(r.Message + " comment failed: " + detail)
					r.Hint = fmt.Sprintf("retry with: kata comment %s --project %d --body ... --idempotency-key %s%s", r.UID, r.targetID, key, suffix)
					errs[i] = fmt.Errorf("comment for %s failed: %s; %s", r.UID, detail, r.Hint)
				}
			}
		})
	}
	for i := range results {
		if results[i].Result == "moved" {
			jobs <- i
		}
	}
	close(jobs)
	wg.Wait()
	return errors.Join(errs...)
}

// validateMoveBatchDefault checks the optional positional target of a refs
// file, which rows that name their own project never consult.
func validateMoveBatchDefault(args []string, refsFrom string, projects []projectRef) (*projectRef, error) {
	if refsFrom == "" || len(args) == 0 {
		return nil, nil
	}
	target, err := resolveProjectSelectorFromRefs(strings.TrimSpace(args[0]), projects)
	return &target, err
}

func refuseDuplicateMoveBatchRows(prepared []*moveBatchPrepared, results []moveBatchResult) {
	groups := make(map[string][]int)
	for i, p := range prepared {
		if p != nil {
			groups[p.issue.UID] = append(groups[p.issue.UID], i)
		}
	}
	for _, indexes := range groups {
		conflict := false
		for _, i := range indexes[1:] {
			// Each non-nil index belongs to one UID group, which clears rows only after its comparisons.
			//nolint:nilaway // The group still contains non-nil rows.
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
}

func moveBatchSourceProject(selector string, projects []projectRef) (projectRef, error) {
	source, found, err := uniqueProjectMatch(selector, projects, projectNameExact)
	if err == nil && !found {
		err = fmt.Errorf("project selector %q did not match any project", selector)
	}
	return source, err
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

func resolveMoveBatchOne(a daemonAPI, client *kataclient.Client, projects []projectRef, row moveBatchRow, sharedTarget *projectRef, result *moveBatchResult, halt *atomic.Bool) *moveBatchPrepared {
	if a.ctx.Err() != nil {
		return nil
	}
	refuse := func(code string, err error) {
		if a.ctx.Err() == nil {
			result.Result, result.Code, result.Message, result.Hint = "refused", code, err.Error(), ""
		}
	}
	parsed, err := shortid.Parse(row.Ref)
	if err == nil && parsed.ULID == "" && parsed.Project == "" {
		parsed.Project = strings.TrimSpace(flags.Project)
	}
	if err != nil || (parsed.ULID == "" && parsed.Project == "") {
		refuse("invalid_ref", errors.New("use project#id, the issue UID, or --project with a short ID"))
		return nil
	}
	var target projectRef
	if sharedTarget != nil {
		target = *sharedTarget
	} else {
		target, err = resolveProjectSelectorFromRefs(strings.TrimSpace(row.Target), projects)
		if err != nil {
			refuse("invalid_target", err)
			return nil
		}
	}
	if target.UID == "" {
		refuse("invalid_target", fmt.Errorf("project %q has no UID", target.Name))
		return nil
	}
	result.Target, result.targetID = target.Name, target.ID
	pid, ref := target.ID, parsed.ULID
	if parsed.Project != "" {
		source, e := moveBatchSourceProject(parsed.Project, projects)
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
		if apiErr, ok := errors.AsType[*cliError](err); ok && apiErr.HTTPStatus >= 400 && apiErr.HTTPStatus < 500 {
			refuse("issue_lookup", err)
		} else {
			result.Message = err.Error()
			halt.Store(true)
		}
		return nil
	}
	if !uid.Valid(issue.UID) {
		result.Message = "issue lookup returned an invalid UID"
		halt.Store(true)
		return nil
	}
	result.UID = issue.UID
	return &moveBatchPrepared{issue: issue, target: target, pid: pid, ref: ref}
}

func moveBatchOne(a daemonAPI, client *kataclient.Client, prepared *moveBatchPrepared, result *moveBatchResult, actor string, subtreeSupported bool, halt *atomic.Bool) {
	issue, target, pid, ref := prepared.issue, prepared.target, prepared.pid, prepared.ref
	if a.ctx.Err() != nil || halt.Load() {
		return
	}
	request := moveRequest(pid, ref, actor, target.UID, issue.Revision, false, subtreeSupported)
	response, callErr := client.MoveIssueWithResponse(a.ctx, request)
	unknown := func() {
		halt.Store(true)
		result.Result = "unknown"
		result.Hint = moveBatchUnknownHint(result.UID, result.targetID, issue.ProjectID)
	}
	if response == nil {
		transportErr, marked := errors.AsType[*daemonTransportError](callErr)
		if marked && !transportErr.mutationOutcomeUnknown() {
			if callErr != nil && a.ctx.Err() == nil {
				result.Message = callErr.Error()
			}
			return
		}
		unknown()
		return
	}
	if response.StatusCode == http.StatusRequestTimeout || response.StatusCode >= 500 && response.StatusCode <= 599 {
		unknown()
		return
	}
	if response.StatusCode >= 400 {
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			result.Result, result.Code, result.Message, result.Hint = "refused", "move_refused", err.Error(), ""
			return
		}
	}
	var moved moveResponseWire
	if err := json.Unmarshal(response.Body, &moved); err != nil || !moveBatchComplete(moved, target.ID, result.UID) {
		unknown()
		return
	}
	result.Result, result.Hint = "moved", ""
	result.To, result.EventID = moveQualifiedID(target.Name, moved.Issue.ShortID), moved.EventID
}

func moveBatchComplete(moved moveResponseWire, targetID int64, uid string) bool {
	return moved.Changed && moved.EventID != 0 && moved.Issue.UID == uid && moved.Issue.ProjectID == targetID && moved.Issue.ShortID != "" && moved.NewShortID == moved.Issue.ShortID
}

func moveBatchUnknownHint(uid string, targetID, sourceID int64) string {
	flags := moveBatchRecoveryFlags()
	return fmt.Sprintf("outcome unconfirmed; inspect the target with kata show %s --project %d --json%s and the source with kata show %s --project %d --json%s. Check move history before retrying if the issue may have moved more than once.", uid, targetID, flags, uid, sourceID, flags)
}

func moveBatchRecoveryFlags() string {
	var result string
	if flags.Daemon != "" {
		result += " --daemon " + shellQuote(flags.Daemon)
	}
	if flags.Workspace != "" {
		result += " --workspace " + shellQuote(flags.Workspace)
	}
	return result
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
