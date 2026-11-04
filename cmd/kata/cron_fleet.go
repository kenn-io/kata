package main

import (
	"context"
	"encoding/json/v2"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/cronread"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func cronSinceInstant(value string, now time.Time) (time.Time, error) {
	if at, err := time.Parse(time.RFC3339Nano, value); err == nil && !at.IsZero() {
		return at.UTC(), nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return time.Time{}, cronCLIValidation("--since must be an RFC3339 instant or positive Go duration")
	}
	return now.Add(-duration).UTC(), nil
}

func (o *cronCLIOptions) prepareFleetFilters(cmd *cobra.Command) error {
	if o.allProjects && strings.TrimSpace(flags.Project) != "" {
		return cronCLIValidation("--all and --project are mutually exclusive")
	}
	if o.resource != "run" || o.action != "list" {
		return nil
	}
	for name, value := range map[string]string{"issue": o.issue, "since": o.since, "executor-label": o.executorLabel} {
		if cmd.Flags().Changed(name) && strings.TrimSpace(value) == "" {
			return cronCLIValidation("--" + name + " must not be empty")
		}
	}
	query := db.CronRunList{AllProjects: true, JobUID: o.jobUID, Limit: int(o.limit), BeforeUID: o.beforeUID, Statuses: o.statuses, ExecutorLabel: o.executorLabel}
	// The persistent flag is also write attribution. Read filtering is explicit
	// only and must never inherit KATA_TEAMMATE.
	if cmd.Flags().Changed("teammate") {
		query.Teammate, _ = cmd.Flags().GetString("teammate")
	}
	if o.since != "" {
		at, err := cronSinceInstant(o.since, time.Now())
		if err != nil {
			return err
		}
		query.Since = at
	}
	if o.issue != "" {
		o.issue = strings.TrimSpace(o.issue)
		if uid.Valid(strings.ToUpper(o.issue)) {
			query.IssueUID = strings.ToUpper(o.issue)
		} else if o.allProjects && !strings.Contains(o.issue, "#") {
			return cronCLIValidation("--issue with --all requires a full UID or qualified project#ref")
		}
	}
	normalized, err := db.NormalizeCronRunList(query)
	if err != nil {
		return cronCLIValidation(err.Error())
	}
	o.runQuery = normalized
	return nil
}

func (o *cronCLIOptions) resolveFleetIssue(ctx context.Context, cmd *cobra.Command, a daemonAPI, project projectRef) error {
	if o.issue == "" || o.runQuery.IssueUID != "" {
		return nil
	}
	var issueProject int64
	var ref resolvedIssueRef
	if !o.allProjects && !strings.Contains(o.issue, "#") {
		parsed, err := ResolveRef(o.issue, project.Name)
		if err != nil {
			return cronCLIValidation(err.Error())
		}
		issueProject = project.ID
		ref = resolvedIssueRef{RefForAPI: parsed.RefForAPI, ProjectName: project.Name}
	} else {
		var err error
		_, _, issueProject, ref, err = resolveIssueRefForCommand(cmd, o.issue)
		if err != nil {
			return err
		}
	}
	if !o.allProjects && issueProject != project.ID {
		return cronCLIValidation("--issue must belong to the selected project")
	}
	client, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
	if err != nil {
		return err
	}
	response, err := client.ShowIssueWithResponse(ctx, &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: issueProject, Ref: ref.RefForAPI}})
	if response == nil {
		return externalCLITransportError(response, err)
	}
	raw, err := cronCLIResponse(response.StatusCode, response.Body, err)
	if err != nil {
		return err
	}
	var out struct {
		Issue struct {
			UID string `json:"uid"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	o.runQuery.IssueUID, err = normalizedCronCLIUID(out.Issue.UID)
	return err
}

func (o cronCLIOptions) callRunList(ctx context.Context, c *kataclient.Client, projectID int64) ([]byte, error) {
	var raw []byte
	var err error
	if o.allProjects {
		response, callErr := c.ListAllCronRunsWithResponse(ctx, &generated.ListAllCronRunsRequestOptions{Query: cronread.FleetQuery(o.runQuery, nil)})
		if response == nil {
			return nil, externalCLITransportError(response, callErr)
		}
		raw, err = cronCLIResponse(response.StatusCode, response.Body, callErr)
	} else {
		response, callErr := c.ListCronRunsWithResponse(ctx, &generated.ListCronRunsRequestOptions{PathParams: &generated.ListCronRunsPath{ProjectID: projectID}, Query: cronread.ProjectQuery(o.runQuery)})
		if response == nil {
			return nil, externalCLITransportError(response, callErr)
		}
		raw, err = cronCLIResponse(response.StatusCode, response.Body, callErr)
	}
	if err != nil {
		return nil, err
	}
	if err := cronread.VerifyFilters(raw, o.runQuery.Filters()); err != nil {
		return nil, cronCLIValidation(err.Error())
	}
	return raw, nil
}
