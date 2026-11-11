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
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

type moveBatchItem struct {
	Ref, TargetProject string
	ParseError         string
	ExpectedRevision   int64
}
type moveBatchOptions struct {
	DryRun, Subtree   bool
	Comment, Teammate string
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
func parseRefsFrom(r io.Reader, defaultTarget string, maxItems int) ([]moveBatchItem, error) {
	reader := bufio.NewReader(r)
	var items []moveBatchItem
	retained := 0
	for line := 1; ; line++ {
		value, oversized, err := readMoveBatchLine(reader)
		if err != nil && err != io.EOF {
			return nil, moveBatchUsage(err.Error())
		}
		if err == io.EOF && value == "" && !oversized {
			break
		}
		if oversized {
			items = append(items, moveBatchItem{ParseError: fmt.Sprintf("line %d: exceeds 1 MiB", line)})
		} else {
			if line == 1 {
				value = strings.TrimPrefix(value, "\ufeff")
			}
			trimmed := strings.TrimSpace(value)
			if (trimmed != "" || strings.Contains(value, "\t")) && trimmed != "#" && !strings.HasPrefix(trimmed, "# ") {
				retained += len(value)
				if retained > 1<<20 {
					return nil, moveBatchUsage("refs-file input exceeds 1 MiB; split the input into separate runs")
				}
				fields := strings.Split(value, "\t")
				item := moveBatchItem{Ref: strings.TrimSpace(fields[0]), TargetProject: defaultTarget}
				if len(fields) >= 2 {
					item.TargetProject = strings.TrimSpace(fields[1])
				}
				if len(fields) > 2 || item.Ref == "" || item.TargetProject == "" {
					item.ParseError = fmt.Sprintf("line %d: expected ref and target project", line)
				}
				items = append(items, item)
			}
		}
		if err == io.EOF || len(items) >= maxItems {
			break
		}
	}
	return items, nil
}

func readMoveBatchLine(reader *bufio.Reader) (string, bool, error) {
	var line []byte
	oversized := false
	for {
		part, err := reader.ReadSlice('\n')
		if !oversized {
			if len(line)+len(part) >= 1<<20 {
				line, oversized = nil, true
			} else {
				line = append(line, part...)
			}
		}
		if err != bufio.ErrBufferFull {
			return strings.TrimSuffix(string(line), "\n"), oversized, err
		}
	}
}

func moveBatchTarget(a daemonAPI, item moveBatchItem, targets map[string]string) (string, error) {
	if item.ParseError != "" {
		return "", &cliError{Message: item.ParseError, Code: "validation", Kind: kindUsage, ExitCode: ExitUsage}
	}
	uid := targets[item.TargetProject]
	if uid == "" {
		project, err := resolveProjectSelector(a, item.TargetProject)
		if err != nil {
			return "", err
		}
		uid = project.UID
		targets[item.TargetProject] = uid
	}
	return uid, nil
}

func moveBatchReadLimit(items []moveBatchItem, opts moveBatchOptions) int {
	if opts.DryRun || opts.Subtree {
		return 2 - len(items)
	}
	return 5001
}

func validateMoveBatchSelection(items []moveBatchItem, opts moveBatchOptions) error {
	if len(items) > 1 {
		if opts.DryRun {
			return moveBatchUsage("dry run accepts one item")
		}
		if opts.Subtree {
			return moveBatchUsage("--subtree accepts one issue")
		}
	}
	return nil
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
	if err := validateMoveBatchSelection(items, opts); err != nil {
		return err
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
		parsed, err := parseRefsFrom(reader, defaultTarget, moveBatchReadLimit(items, opts))
		if err != nil {
			return err
		}
		items = append(items, parsed...)
	}
	if err := validateMoveBatchSelection(items, opts); err != nil {
		return err
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
	if len(items) > 5000 {
		return moveBatchUsage("batch move accepts at most 5,000 items; split the input into separate runs")
	}
	if err := validateMoveBatchSelection(items, opts); err != nil {
		return err
	}
	if (opts.DryRun || opts.Subtree) && len(items) == 1 && items[0].ParseError == "" {
		previousProject := flags.Project
		defer func() { flags.Project = previousProject }()
		if sel.Project != "" && items[0].ExpectedRevision != 0 {
			flags.Project = sel.Project
		} else if parsed, err := shortid.Parse(items[0].Ref); err == nil && parsed.ULID != "" {
			a, err := dialDaemon(cmd.Context())
			if err != nil {
				return err
			}
			client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
			if err != nil {
				return err
			}
			response, callErr := client.ShowIssueByUIDWithResponse(a.ctx, &generated.ShowIssueByUIDRequestOptions{PathParams: &generated.ShowIssueByUIDPath{UID: parsed.ULID}})
			if response == nil {
				return externalCLITransportError(response, callErr)
			}
			if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("daemon returned an invalid issue response")
			}
			project, err := resolveProjectSelector(a, strconv.FormatInt(response.JSON200.Issue.ProjectID, 10))
			if err != nil {
				return err
			}
			flags.Project = project.Name
		}
		return runMove(cmd, items[0].Ref, items[0].TargetProject, opts.DryRun, !opts.Subtree, items[0].ExpectedRevision)
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
			if sel.Query == "" && len(selected) > 5000 {
				return selected, nil
			}
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
		query := &generated.SearchIssuesQuery{Q: sel.Query, Mode: new(generated.Lexical), Sort: new(generated.SearchIssuesQuerySortOldest), Limit: new(int64(20))}
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
		page := response.JSON200
		if page == nil {
			return nil, fmt.Errorf("unexpected search response status %d", response.StatusCode)
		}
		for _, row := range page.Results {
			matches[row.Issue.UID] = true
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		if *page.NextCursor == cursor {
			return nil, fmt.Errorf("could not complete search")
		}
		cursor = *page.NextCursor
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
		if err == nil {
			pid, _, err := resolveProjectIDAndNameWithClient(a, start)
			if err == nil {
				request.Body.DefaultProjectID = pid
			}
		}
	}
	targets := map[string]string{}
	indexes := make([]int, 0, len(items))
	results := make([]api.MoveIssuesResult, len(items))
	for i, item := range items {
		results[i] = api.MoveIssuesResult{Index: i, Ref: item.Ref}
		uid, err := moveBatchTarget(a, item, targets)
		if err != nil {
			r := api.MoveIssuesResult{Index: i, Ref: item.Ref, Result: "refused", Message: err.Error()}
			if ce, ok := errors.AsType[*cliError](err); ok {
				r.Code, r.Message, r.Hint = ce.Code, ce.Message, ce.Hint
			}
			results[i] = r
			continue
		}
		indexes = append(indexes, i)
		request.Body.Items = append(request.Body.Items, api.MoveIssuesItem{Ref: item.Ref, ToProjectUID: uid, ExpectedRevision: item.ExpectedRevision})
	}
	if len(request.Body.Items) == 0 {
		if err := printMoveBatch(cmd, results, len(items)); err != nil {
			return err
		}
		if len(results) > 0 {
			return &cliError{Message: "some issues were refused", Kind: kindConflict, ExitCode: ExitConflict, HumanReported: true}
		}
		return nil
	}
	// Encode every page before sending any mutation.
	var pages []generated.MoveIssuesBody
	for start := 0; start < len(request.Body.Items); start += api.MoveBatchMaxItems {
		page := request.Body
		page.Items = request.Body.Items[start:min(start+api.MoveBatchMaxItems, len(request.Body.Items))]
		raw, err := json.Marshal(page)
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
		pages = append(pages, body)
	}
	printResults := func() error {
		return printMoveBatch(cmd, results, len(items))
	}
	for pageIndex, body := range pages {
		start := pageIndex * api.MoveBatchMaxItems
		classify := func() {
			for i := range results {
				if results[i].Result == "" {
					results[i].Result = "unattempted"
				}
			}
			for _, i := range indexes[start : start+len(body.Items)] {
				if results[i].Result == "unattempted" {
					results[i].Result = "unknown"
				}
			}
		}
		response, callErr := client.MoveIssuesWithResponse(a.ctx, &generated.MoveIssuesRequestOptions{Body: &body})
		if response == nil {
			classify()
			if err := printResults(); err != nil {
				return err
			}
			message := "Connection failed; check unknown results before retrying"
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
						if r.Index >= 0 && r.Index < len(body.Items) && r.Ref == request.Body.Items[start+r.Index].Ref && (r.Result == "moved" || r.Result == "unchanged" || r.Result == "refused") && results[indexes[start+r.Index]].Result == "" {
							r.Index = indexes[start+r.Index]
							results[r.Index] = r
						}
					}
				}
			}
			classify()
			if printErr := printResults(); printErr != nil {
				return printErr
			}
			return err
		}
		var reply struct {
			Results []api.MoveIssuesResult `json:"results"`
		}
		if err := json.Unmarshal(response.Body, &reply); err != nil {
			classify()
			if printErr := printResults(); printErr != nil {
				return printErr
			}
			return err
		}
		if len(reply.Results) != len(body.Items) {
			classify()
			if printErr := printResults(); printErr != nil {
				return printErr
			}
			return fmt.Errorf("invalid batch move result count")
		}
		for j, r := range reply.Results {
			if r.Index != j || r.Index >= len(body.Items) || r.Ref != request.Body.Items[start+j].Ref || (r.Result != "moved" && r.Result != "unchanged" && r.Result != "refused") {
				classify()
				if printErr := printResults(); printErr != nil {
					return printErr
				}
				return fmt.Errorf("invalid batch move result")
			}
		}
		for _, r := range reply.Results {
			r.Index = indexes[start+r.Index]
			results[r.Index] = r
		}
	}
	if err := printResults(); err != nil {
		return err
	}
	for _, r := range results {
		if r.Result == "refused" {
			return &cliError{Message: "some issues were refused", Kind: kindConflict, ExitCode: ExitConflict, HumanReported: true}
		}
	}
	return nil
}

func printMoveBatch(cmd *cobra.Command, results []api.MoveIssuesResult, requested int) error {
	type summaryCounts struct {
		Moved       int `json:"moved"`
		Unchanged   int `json:"unchanged"`
		Refused     int `json:"refused"`
		Unknown     int `json:"unknown"`
		Unattempted int `json:"unattempted"`
	}
	summary := summaryCounts{}
	for _, r := range results {
		switch r.Result {
		case "moved":
			summary.Moved++
		case "unchanged":
			summary.Unchanged++
		case "refused":
			summary.Refused++
		case "unknown":
			summary.Unknown++
		case "unattempted":
			summary.Unattempted++
		}
	}
	if currentOutputMode() == outputJSON {
		return json.MarshalWrite(cmd.OutOrStdout(), struct {
			Summary summaryCounts          `json:"summary"`
			Results []api.MoveIssuesResult `json:"results"`
		}{summary, results})
	}
	var out bytes.Buffer
	if currentOutputMode() == outputAgent {
		fmt.Fprintf(&out, "OK move-batch requested=%d moved=%d unchanged=%d refused=%d unknown=%d unattempted=%d\n", requested, summary.Moved, summary.Unchanged, summary.Refused, summary.Unknown, summary.Unattempted)
	} else {
		fmt.Fprintf(&out, "Move batch: %d moved, %d unchanged, %d refused, %d unknown, %d unattempted\n", summary.Moved, summary.Unchanged, summary.Refused, summary.Unknown, summary.Unattempted)
	}
	for _, r := range results {
		fmt.Fprintf(&out, "- ref=%s result=%s", agentValue(r.Ref), r.Result)
		if r.To != "" {
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

	}
	_, err := io.Copy(cmd.OutOrStdout(), &out)
	return err
}
