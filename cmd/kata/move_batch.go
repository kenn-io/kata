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
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
	"golang.org/x/term"
)

type moveBatchItem struct {
	Ref, TargetProject string
	ExpectedRevision   int64
}
type moveBatchOptions struct {
	DryRun, Subtree, Verbose bool
	Comment, Teammate        string
}
type moveSelector struct {
	Project                string
	Labels, NoLabels, Meta []string
	Status, Owner, Query   string
	Unowned, StatusSet     bool
}

func moveBatchUsage(message string) error {
	return &cliError{Message: message, Kind: kindUsage, ExitCode: ExitUsage}
}
func parseRefsFrom(r io.Reader, defaultTarget string) ([]moveBatchItem, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var items []moveBatchItem
	line := 0
	for scanner.Scan() {
		line++
		value := scanner.Text()
		if line == 1 {
			value = strings.TrimPrefix(value, "\ufeff")
		}
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || trimmed == "#" || strings.HasPrefix(trimmed, "# ") {
			continue
		}
		fields := strings.Split(value, "\t")
		if len(fields) > 2 {
			return nil, moveBatchUsage(fmt.Sprintf("line %d: expected ref or ref<TAB>project", line))
		}
		item := moveBatchItem{Ref: strings.TrimSpace(fields[0]), TargetProject: defaultTarget}
		if len(fields) == 2 {
			item.TargetProject = strings.TrimSpace(fields[1])
		}
		if item.Ref == "" || item.TargetProject == "" {
			return nil, moveBatchUsage(fmt.Sprintf("line %d: ref and target project are required", line))
		}
		items = append(items, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, moveBatchUsage(err.Error())
	}
	return items, nil
}

func runMoveBatchCommand(cmd *cobra.Command, args []string, refsFrom string, sel moveSelector, opts moveBatchOptions) error {
	defaultTarget := ""
	var items []moveBatchItem
	if len(args) > 0 {
		defaultTarget = args[len(args)-1]
		for _, ref := range args[:len(args)-1] {
			items = append(items, moveBatchItem{Ref: ref, TargetProject: defaultTarget})
		}
	}
	if refsFrom != "" {
		reader := cmd.InOrStdin()
		var file *os.File
		if refsFrom != "-" {
			var err error
			//nolint:gosec // G304: --refs-from explicitly selects the local input file to read.
			file, err = os.Open(refsFrom)
			if err != nil {
				return moveBatchUsage(err.Error())
			}
			defer func() { _ = file.Close() }()
			reader = file
		}
		parsed, err := parseRefsFrom(reader, defaultTarget)
		if err != nil {
			return err
		}
		items = append(items, parsed...)
	}
	if sel.Project != "" {
		if defaultTarget == "" {
			return moveBatchUsage("selector requires a target project")
		}
		if sel.Query == "" && len(sel.Labels) == 0 && len(sel.NoLabels) == 0 && len(sel.Meta) == 0 && sel.Owner == "" && !sel.Unowned && !sel.StatusSet {
			return moveBatchUsage("selector requires a filter; use kata projects merge to move a whole project")
		}
		if sel.Status != "all" && sel.Status != "open" && sel.Status != "closed" {
			return moveBatchUsage("--status must be open, closed or all")
		}
		if sel.Owner != "" && sel.Unowned {
			return moveBatchUsage("--owner and --unowned are mutually exclusive")
		}
		a, err := dialDaemon(cmd.Context())
		if err != nil {
			return err
		}
		selected, err := resolveMoveSelector(a.ctx, a, sel)
		if err != nil {
			return err
		}
		for j := range selected {
			selected[j].TargetProject = defaultTarget
		}
		items = append(items, selected...)
	}
	if len(items) == 0 && sel.Project == "" {
		return moveBatchUsage("move requires issue refs and a target project")
	}
	comment, handle, err := prepareFollowupComment(cmd)
	if err != nil {
		return err
	}
	opts.Comment = comment
	opts.Teammate = handle
	return runMoveBatch(cmd, items, opts)
}

func resolveMoveSelector(ctx context.Context, a daemonAPI, sel moveSelector) ([]moveBatchItem, error) {
	project, err := resolveProjectSelector(a, sel.Project)
	if err != nil {
		return nil, err
	}
	client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
	if err != nil {
		return nil, err
	}
	if err := requireDaemonAPIVersion(ctx, a.client, a.baseURL, apiVersionMoveBatch, "batch move"); err != nil {
		return nil, err
	}
	var selected []moveBatchItem
	cursor := ""
	status := sel.Status
	if status == "all" {
		status = ""
	}
	for {
		query := &generated.ListIssuesQuery{Compact: new(true), Sort: new(generated.ListIssuesQuerySortOldest), Limit: new(int64(200)), Status: new(generated.ListIssuesQueryStatus(status)), Label: sel.Labels, ExcludeLabel: sel.NoLabels, Meta: sel.Meta}
		if cursor != "" {
			query.Cursor = &cursor
		}
		if sel.Owner != "" {
			query.Owner = &sel.Owner
		}
		if sel.Unowned {
			query.Unowned = new(true)
		}
		response, callErr := client.ListIssuesWithResponse(ctx, &generated.ListIssuesRequestOptions{PathParams: &generated.ListIssuesPath{ProjectID: project.ID}, Query: query})
		if response == nil {
			return nil, externalCLITransportError(response, callErr)
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return nil, err
		}
		var page struct {
			Issues     []db.Issue `json:"issues"`
			NextCursor string     `json:"next_cursor"`
		}
		if err := json.Unmarshal(response.Body, &page); err != nil {
			return nil, err
		}
		for _, issue := range page.Issues {
			selected = append(selected, moveBatchItem{Ref: issue.UID, ExpectedRevision: issue.Revision})
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			return nil, fmt.Errorf("could not list all issues")
		}
		cursor = page.NextCursor
	}
	if sel.Query == "" {
		return selected, nil
	}
	// Intersect existing list filters with the exhaustive lexical search result.
	// This preserves owner and metadata semantics, which search does not expose.
	matches := map[string]bool{}
	cursor = ""
	for {
		query := &generated.SearchIssuesQuery{Q: sel.Query, Mode: new(generated.Lexical), Sort: new(generated.SearchIssuesQuerySortOldest), Limit: new(int64(200))}
		if cursor != "" {
			query.Cursor = &cursor
		}
		response, callErr := client.SearchIssuesWithResponse(ctx, &generated.SearchIssuesRequestOptions{PathParams: &generated.SearchIssuesPath{ProjectID: project.ID}, Query: query})
		if response == nil {
			return nil, externalCLITransportError(response, callErr)
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			return nil, err
		}
		var page struct {
			Results []struct {
				Issue db.Issue `json:"issue"`
			} `json:"results"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(response.Body, &page); err != nil {
			return nil, err
		}
		for _, row := range page.Results {
			matches[row.Issue.UID] = true
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			return nil, fmt.Errorf("could not complete search")
		}
		cursor = page.NextCursor
	}
	out := selected[:0]
	for _, item := range selected {
		if matches[item.Ref] {
			out = append(out, item)
		}
	}
	return out, nil
}

func runMoveBatch(cmd *cobra.Command, items []moveBatchItem, opts moveBatchOptions) error {
	if len(items) > 5000 {
		return moveBatchUsage("batch move accepts at most 5,000 items; split the input into separate runs")
	}
	if opts.Subtree && len(items) > 1 {
		return moveBatchUsage("--subtree accepts one issue")
	}
	a, err := dialDaemon(cmd.Context())
	if err != nil {
		return err
	}
	if err := requireDaemonAPIVersion(a.ctx, a.client, a.baseURL, apiVersionMoveBatch, "batch move"); err != nil {
		return err
	}
	mutationClient, err := longRunningClientForResolved(a.ctx, a.resolved)
	if err != nil {
		return err
	}
	client, err := kataclient.NewWithHTTPClient(a.baseURL, mutationClient)
	if err != nil {
		return err
	}
	actor, _ := resolveActor(a.ctx, flags.As, nil)
	request := api.MoveIssuesRequest{}
	request.Body.Actor = actor
	request.Body.DryRun = opts.DryRun
	request.Body.Subtree = opts.Subtree
	request.Body.Comment = opts.Comment
	request.Body.Teammate = opts.Teammate
	needsDefault := false
	for _, item := range items {
		parsed, err := shortid.Parse(item.Ref)
		if err == nil {
			needsDefault = needsDefault || (parsed.ULID == "" && parsed.Project == "")
		}
	}
	if needsDefault {
		start, err := resolveStartPath(flags.Workspace)
		if err != nil {
			return err
		}
		pid, _, err := resolveProjectIDAndNameWithClient(a, start)
		if err != nil {
			return err
		}
		request.Body.DefaultProjectID = pid
	}
	// Account for every original row before dropping locally refused targets.
	for _, item := range items {
		request.Body.Items = append(request.Body.Items, api.MoveIssuesItem{Ref: item.Ref, ToProjectUID: item.TargetProject, ExpectedRevision: item.ExpectedRevision})
	}
	request.Body.StartIndex = len(items)
	original, err := json.Marshal(request.Body, jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
	if err != nil {
		return err
	}
	if len(original) > 1<<20 {
		return moveBatchUsage("batch move exceeds the 1 MiB request limit; split the input into separate runs")
	}
	request.Body.Items = nil
	request.Body.StartIndex = 0
	targets := map[string]string{}
	targetErrors := map[string]error{}
	var indexes []int
	var results []api.MoveIssuesResult
	for i, item := range items {
		uid := targets[item.TargetProject]
		if uid == "" && targetErrors[item.TargetProject] == nil {
			project, err := resolveProjectSelector(a, item.TargetProject)
			if err != nil {
				targetErrors[item.TargetProject] = err
			} else {
				uid = project.UID
				targets[item.TargetProject] = uid
			}
		}
		if err := targetErrors[item.TargetProject]; err != nil {
			r := api.MoveIssuesResult{Index: i, Ref: item.Ref, Result: "refused", Message: err.Error()}
			if ce, ok := errors.AsType[*cliError](err); ok {
				r.Code, r.Message, r.Hint = ce.Code, ce.Message, ce.Hint
			}
			results = append(results, r)
			continue
		}
		indexes = append(indexes, i)
		request.Body.Items = append(request.Body.Items, api.MoveIssuesItem{Ref: item.Ref, ToProjectUID: uid, ExpectedRevision: item.ExpectedRevision})
	}
	if len(request.Body.Items) == 0 {
		if err := printMoveBatch(cmd, results, len(items), opts); err != nil {
			return err
		}
		if len(results) > 0 {
			return &cliError{Message: "some issues were refused", Kind: kindConflict, ExitCode: ExitConflict, HumanReported: true}
		}
		return nil
	}
	// Match the generated transport's JSON escaping and reserve the largest
	// continuation encoding before the first mutation.
	request.Body.StartIndex = len(request.Body.Items)
	raw, err := json.Marshal(request.Body)
	if err != nil {
		return err
	}
	var body generated.MoveIssuesBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	raw, err = json.Marshal(body, jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return moveBatchUsage("batch move exceeds the 1 MiB request limit; split the input into separate runs")
	}
	request.Body.StartIndex = 0
	requests := 0
	for request.Body.StartIndex < len(request.Body.Items) {
		body.StartIndex = new(int64(request.Body.StartIndex))
		response, callErr := client.MoveIssuesWithResponse(a.ctx, &generated.MoveIssuesRequestOptions{Body: &body})
		if response == nil {
			slices.SortFunc(results, func(a, b api.MoveIssuesResult) int { return a.Index - b.Index })
			if err := printMoveBatch(cmd, results, len(items), opts); err != nil {
				return err
			}
			message := "Connection failed; rerun the same input to continue"
			if callErr != nil {
				message += ": " + callErr.Error()
			}
			return &cliError{Message: message, Kind: kindDaemonUnavail, ExitCode: ExitDaemonUnavail}
		}
		if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
			if ce, ok := errors.AsType[*cliError](err); ok && len(ce.Data) > 0 {
				var partial struct {
					Results []api.MoveIssuesResult `json:"results"`
				}
				if json.Unmarshal(ce.Data, &partial) == nil {
					for _, r := range partial.Results {
						if r.Index >= 0 && r.Index < len(indexes) {
							r.Index = indexes[r.Index]
							results = append(results, r)
						}
					}
				}
			}
			slices.SortFunc(results, func(a, b api.MoveIssuesResult) int { return a.Index - b.Index })
			if printErr := printMoveBatch(cmd, results, len(items), opts); printErr != nil {
				return printErr
			}
			return err
		}
		var reply struct {
			Results   []api.MoveIssuesResult `json:"results"`
			NextIndex int                    `json:"next_index"`
		}
		if err := json.Unmarshal(response.Body, &reply); err != nil {
			return err
		}
		if reply.NextIndex <= request.Body.StartIndex || reply.NextIndex > len(request.Body.Items) {
			return fmt.Errorf("could not continue batch move")
		}
		for _, r := range reply.Results {
			if r.Index < 0 || r.Index >= len(indexes) {
				return fmt.Errorf("invalid batch move result index")
			}
			r.Index = indexes[r.Index]
			results = append(results, r)
		}
		request.Body.StartIndex = reply.NextIndex
		requests++
		if requests > 1 {
			if file, ok := cmd.ErrOrStderr().(*os.File); ok && term.IsTerminal(int(file.Fd())) {
				_, _ = fmt.Fprintf(file, "moved %d/%d ...\n", len(results), len(items))
			}
		}
	}
	slices.SortFunc(results, func(a, b api.MoveIssuesResult) int { return a.Index - b.Index })
	if err := printMoveBatch(cmd, results, len(items), opts); err != nil {
		return err
	}
	for _, r := range results {
		if r.Result == "refused" {
			return &cliError{Message: "some issues were refused", Kind: kindConflict, ExitCode: ExitConflict, HumanReported: true}
		}
	}
	return nil
}

func printMoveBatch(cmd *cobra.Command, results []api.MoveIssuesResult, requested int, opts moveBatchOptions) error {
	summary := api.MoveIssuesSummary{}
	for _, r := range results {
		switch r.Result {
		case "moved":
			summary.Moved++
		case "unchanged":
			summary.Unchanged++
		case "refused":
			summary.Refused++
		case "would_move":
			summary.WouldMove++
		}
	}
	if results == nil {
		results = []api.MoveIssuesResult{}
	}
	if currentOutputMode() == outputJSON {
		return json.MarshalWrite(cmd.OutOrStdout(), struct {
			DryRun  bool                   `json:"dry_run"`
			Summary api.MoveIssuesSummary  `json:"summary"`
			Results []api.MoveIssuesResult `json:"results"`
		}{opts.DryRun, summary, results})
	}
	var out bytes.Buffer
	if currentOutputMode() == outputAgent {
		fmt.Fprintf(&out, "OK move-batch requested=%d moved=%d unchanged=%d refused=%d dry_run=%t", requested, summary.Moved, summary.Unchanged, summary.Refused, opts.DryRun)
		if opts.DryRun {
			fmt.Fprintf(&out, " would_move=%d", summary.WouldMove)
		}
		out.WriteByte('\n')
	} else {
		fmt.Fprintf(&out, "Move batch: %d moved, %d unchanged, %d refused", summary.Moved, summary.Unchanged, summary.Refused)
		if opts.DryRun {
			fmt.Fprintf(&out, ", %d would move (dry-run)", summary.WouldMove)
		}
		out.WriteByte('\n')
	}
	for _, r := range results {
		if currentOutputMode() != outputAgent && !opts.Verbose && r.Result != "refused" && !r.ShortIDExtended {
			continue
		}
		fmt.Fprintf(&out, "- ref=%s result=%s", agentValue(r.Ref), r.Result)
		if r.Result == "would_move" {
			fmt.Fprintf(&out, " to_project=%s", agentValue(r.ToProjectName))
		} else if r.To != "" {
			fmt.Fprintf(&out, " to=%s", agentValue(r.To))
		}
		if r.Code != "" {
			fmt.Fprintf(&out, " code=%s", r.Code)
		}
		if r.ShortIDExtended {
			out.WriteString(" short_id_extended=true")
		}
		if r.EventID != 0 {
			fmt.Fprintf(&out, " event_id=%d", r.EventID)
		}
		if r.Message != "" {
			fmt.Fprintf(&out, " message=%s", agentValue(r.Message))
		}
		if r.Hint != "" {
			fmt.Fprintf(&out, " hint=%s", agentValue(r.Hint))
		}
		out.WriteByte('\n')
		for _, child := range r.Descendants {
			fmt.Fprintf(&out, "- uid=%s result=%s descendant_of=%s", child.UID, r.Result, agentValue(r.Ref))
			if opts.DryRun {
				fmt.Fprintf(&out, " to_project=%s", agentValue(r.ToProjectName))
			} else {
				fmt.Fprintf(&out, " to=%s", agentValue(r.ToProjectName+"#"+child.ShortID))
			}
			out.WriteByte('\n')
		}
	}
	_, err := io.Copy(cmd.OutOrStdout(), &out)
	return err
}
