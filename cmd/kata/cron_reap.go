package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/api"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

type cronReapOptions struct {
	apply, force    bool
	message, jobUID string
	scanLimit       int
}

type cronReapResult struct {
	RunUID    string                  `json:"run_uid"`
	Revision  int64                   `json:"revision"`
	Staleness *generated.RunStaleness `json:"staleness,omitempty"`
	Action    string                  `json:"action,omitempty"`
	Skipped   string                  `json:"skipped,omitempty"`
	Error     string                  `json:"error,omitempty"`
}

type cronReapReport struct {
	DryRun           bool             `json:"dry_run"`
	Scanned          int              `json:"scanned"`
	ScanLimitReached bool             `json:"scan_limit_reached"`
	Runs             []cronReapResult `json:"runs"`
}

func newCronReapCmd() *cobra.Command {
	o := cronReapOptions{scanLimit: 1000}
	cmd := &cobra.Command{Use: "reap [<run-uid>...]", Short: "preview or record unknown outcomes for stale runs", Args: cobra.ArbitraryArgs}
	cmd.Long, cmd.Example = cronOperationHelp("run", "reap")
	cmd.Flags().BoolVar(&o.apply, "apply", false, "record unknown outcomes; otherwise preview only")
	cmd.Flags().BoolVar(&o.force, "force", false, "include running runs before their stale deadline")
	cmd.Flags().StringVar(&o.message, "message", "", "operator note (at most 1024 bytes, without U+0000)")
	cmd.Flags().StringVar(&o.jobUID, "job-uid", "", "select only runs belonging to this job")
	cmd.Flags().IntVar(&o.scanLimit, "scan-limit", 1000, "maximum history records scanned when no run UIDs are given")
	cmd.RunE = func(cmd *cobra.Command, args []string) error { return o.execute(cmd.Context(), cmd, args) }
	return cmd
}

func (o cronReapOptions) execute(ctx context.Context, cmd *cobra.Command, args []string) error {
	if o.scanLimit < 1 {
		return cronCLIValidation("--scan-limit must be positive")
	}
	if len(o.message) > 1024 || strings.ContainsRune(o.message, 0) {
		return cronCLIValidation("--message must be at most 1024 bytes and cannot contain U+0000")
	}
	var err error
	if o.jobUID != "" {
		o.jobUID, err = normalizedCronCLIUID(o.jobUID)
		if err != nil {
			return err
		}
	}
	ids := make([]string, 0, len(args))
	seen := map[string]bool{}
	for _, raw := range args {
		id, err := normalizedCronCLIUID(raw)
		if err != nil {
			return err
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	a, err := dialDaemon(ctx)
	if err != nil {
		return err
	}
	var project projectRef
	if strings.TrimSpace(flags.Project) != "" {
		project, err = resolveProjectSelectorIncludingArchived(a, flags.Project)
	} else {
		project, err = resolveProjectArgFlagOrWorkspace(a, nil)
	}
	if err != nil {
		return err
	}
	c, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
	if err != nil {
		return err
	}
	runs, scanned, bounded, err := o.read(ctx, c, project.ID, ids)
	if err != nil {
		return err
	}
	// Check support before filtering or performing any mutations. An old daemon
	// must not turn its missing evaluation into a successful empty stale scan.
	for _, r := range runs {
		if r.Revision < 1 {
			return fmt.Errorf("daemon returned invalid run data")
		}
		if r.Status == "running" && r.Staleness == nil {
			return cronCLIValidation("daemon does not support run reap; upgrade the daemon")
		}
	}
	report := cronReapReport{DryRun: !o.apply, Scanned: scanned, ScanLimitReached: bounded, Runs: []cronReapResult{}}
	actor, _ := resolveActor(ctx, flags.As, nil)
	var operationErr error
	for _, r := range runs {
		item := cronReapResult{RunUID: r.UID, Revision: r.Revision, Staleness: r.Staleness}
		if o.jobUID != "" && (r.JobUID == nil || *r.JobUID != o.jobUID) {
			item.Action = "filtered"
			report.Runs = append(report.Runs, item)
			continue
		}
		if r.Status != "running" {
			if len(ids) > 0 {
				item.Skipped = "not_running"
				report.Runs = append(report.Runs, item)
			}
			continue
		}
		if !r.Staleness.Stale && !o.force {
			if len(ids) > 0 {
				item.Skipped = "not_stale"
				report.Runs = append(report.Runs, item)
			}
			continue
		}
		item.Action = "would_reap"
		if o.apply {
			item.Action, item.Skipped, operationErr = applyCronReap(ctx, c, project.ID, r, actor, o)
			if operationErr != nil {
				item.Error = operationErr.Error()
			}
		}
		report.Runs = append(report.Runs, item)
		// Preserve completed outcomes, and leave unattempted runs for a fresh scan.
		// Unexpected failures and uncertain transport outcomes are never retried.
		if operationErr != nil {
			break
		}
	}
	if !flags.Quiet || currentOutputMode() == outputJSON {
		if err := emitJSON(cmd.OutOrStdout(), report); err != nil {
			return err
		}
	}
	return operationErr
}

func (o cronReapOptions) read(ctx context.Context, c *kataclient.Client, projectID int64, ids []string) ([]generated.CronRun, int, bool, error) {
	runs := []generated.CronRun{}
	if len(ids) > 0 {
		for _, id := range ids {
			response, err := c.ShowCronRunWithResponse(ctx, &generated.ShowCronRunRequestOptions{PathParams: &generated.ShowCronRunPath{ProjectID: projectID, RunUID: id}})
			if response == nil {
				return nil, 0, false, externalCLITransportError(response, err)
			}
			if _, err := cronCLIResponse(response.StatusCode, response.Body, err); err != nil {
				return nil, 0, false, err
			}
			if response.JSON200 == nil || response.JSON200.Run.UID != id {
				return nil, 0, false, fmt.Errorf("malformed cron run response")
			}
			runs = append(runs, response.JSON200.Run)
		}
		return runs, len(runs), false, nil
	}
	cursor := ""
	seen := map[string]bool{}
	cursors := map[string]bool{}
	for len(runs) < o.scanLimit {
		limit := int64(min(100, o.scanLimit-len(runs)))
		response, err := c.ListCronRunsWithResponse(ctx, &generated.ListCronRunsRequestOptions{PathParams: &generated.ListCronRunsPath{ProjectID: projectID}, Query: &generated.ListCronRunsQuery{Limit: &limit, BeforeUID: &cursor, JobUID: &o.jobUID}})
		if response == nil {
			return nil, 0, false, externalCLITransportError(response, err)
		}
		if _, err := cronCLIResponse(response.StatusCode, response.Body, err); err != nil {
			return nil, 0, false, err
		}
		if response.JSON200 == nil || int64(len(response.JSON200.Runs)) > limit {
			return nil, 0, false, fmt.Errorf("malformed cron run page")
		}
		page := response.JSON200
		for _, r := range page.Runs {
			if r.UID == "" || seen[r.UID] {
				return nil, 0, false, fmt.Errorf("daemon returned missing or duplicate run identifiers")
			}
			seen[r.UID] = true
			runs = append(runs, r)
		}
		next := ""
		if page.NextBeforeUID != nil {
			next = *page.NextBeforeUID
		}
		if next == "" {
			return runs, len(runs), false, nil
		}
		if len(page.Runs) == 0 || cursors[next] {
			return nil, 0, false, fmt.Errorf("daemon could not provide the next results")
		}
		cursors[next] = true
		cursor = next
	}
	return runs, len(runs), true, nil
}

func applyCronReap(ctx context.Context, c *kataclient.Client, projectID int64, r generated.CronRun, actor string, o cronReapOptions) (string, string, error) {
	response, err := c.ReapCronRunWithResponse(ctx, &generated.ReapCronRunRequestOptions{PathParams: &generated.ReapCronRunPath{ProjectID: projectID, RunUID: r.UID}, Body: &generated.ReapCronRunBody{Actor: &actor, ExpectedRevision: r.Revision, Message: &o.message, Force: &o.force}})
	if response == nil {
		return "", "", externalCLITransportError(response, err)
	}
	var envelope api.ErrorEnvelope
	decoded := json.Unmarshal(response.Body, &envelope) == nil
	if response.StatusCode == 405 || (response.StatusCode == 404 && (!decoded || envelope.Error.Code != "not_found")) {
		return "", "", cronCLIValidation("daemon does not support run reap; upgrade the daemon")
	}
	if response.StatusCode == 409 && decoded {
		switch envelope.Error.Code {
		case "cron_conflict":
			return "", "changed", nil
		case "cron_run_not_running":
			return "", "not_running", nil
		case "cron_run_not_stale":
			return "", "not_stale", nil
		}
	}
	if _, err := cronCLIResponse(response.StatusCode, response.Body, err); err != nil {
		return "", "", err
	}
	if response.JSON200 == nil || response.JSON200.Run.UID != r.UID || response.JSON200.Run.Status != "unknown" {
		return "", "", fmt.Errorf("malformed cron reap response")
	}
	result := response.JSON200
	if result.Run.Revision <= r.Revision || result.Run.Revision-r.Revision != 1 || len(result.Events) != 1 || result.Events[0].UID == "" || result.Events[0].Type != "cron.run.observed" {
		return "", "", fmt.Errorf("daemon returned an invalid reap result; inspect the run before retrying")
	}
	return "reaped", "", nil
}
