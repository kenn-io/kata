package mcpserver

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/cronbundle"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// CronExportInput selects cron definitions to export from a project.
type CronExportInput struct {
	CronProjectInput
	JobUIDs      []string `json:"job_uids,omitempty"`
	WorkflowUIDs []string `json:"workflow_uids,omitempty"`
	All          bool     `json:"all,omitempty"`
}

// CronImportInput configures independent copies of a cron bundle in a project.
type CronImportInput struct {
	CronProjectInput
	Bundle        jsontext.Value    `json:"bundle"`
	IssueMappings map[string]string `json:"issue_mappings,omitempty" jsonschema:"Source issue UID to destination in-project issue reference"`
	KeepEnabled   bool              `json:"keep_enabled,omitempty"`
	Replace       bool              `json:"replace,omitempty"`
	DryRun        bool              `json:"dry_run,omitempty"`
}

func (h toolHandlers) cronBundleProject(ctx context.Context, selector string) (cronbundle.Project, error) {
	p, err := h.createTarget(ctx, selector)
	if err != nil {
		return cronbundle.Project{}, err
	}
	if p.UID == "" {
		shown, err := h.options.Client.ShowProject(ctx, &generated.ShowProjectRequestOptions{PathParams: &generated.ShowProjectPath{ProjectID: p.ID}})
		if err != nil {
			return cronbundle.Project{}, err
		}
		p.UID = shown.Project.UID
	}
	return cronbundle.Project{ID: p.ID, UID: p.UID, Name: p.Name}, nil
}
func (h toolHandlers) cronExport(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronExportInput) (*sdkmcp.CallToolResult, cron.Bundle, error) {
	selection := cronbundle.Selection{Jobs: in.JobUIDs, Workflows: in.WorkflowUIDs, All: in.All}
	if err := selection.Normalize(); err != nil {
		return nil, cron.Bundle{}, err
	}
	p, err := h.cronBundleProject(ctx, in.Project)
	if err != nil {
		return nil, cron.Bundle{}, err
	}
	bundle, err := cronbundle.Export(ctx, cronBundleDaemon{h.options.Client}, p, selection)
	return successResult(), bundle, err
}
func (h toolHandlers) cronImport(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronImportInput) (*sdkmcp.CallToolResult, cronbundle.Report, error) {
	bundle, err := cron.ParseBundle(in.Bundle)
	if err != nil {
		return nil, cronbundle.Report{}, err
	}
	p, err := h.cronBundleProject(ctx, in.Project)
	if err != nil {
		return nil, cronbundle.Report{}, err
	}
	refs := map[string]string{}
	for raw, ref := range in.IssueMappings {
		source, err := cronUID(raw)
		if err != nil {
			return nil, cronbundle.Report{}, fmt.Errorf("issue_mappings: %w", err)
		}
		ref = strings.TrimSpace(ref)
		if old, ok := refs[source]; ok && old != ref {
			return nil, cronbundle.Report{}, fmt.Errorf("conflicting issue mappings for %s", source)
		}
		refs[source] = ref
	}
	mappings := map[string]string{}
	for source, ref := range refs {
		target, err := h.cronIssueRef(ctx, ProjectIdentity{ID: p.ID, UID: p.UID, Name: p.Name}, "issue_mappings", ref)
		if err != nil {
			return nil, cronbundle.Report{}, err
		}
		mappings[source] = target
	}
	plan, err := cron.PlanBundle(bundle, cron.BundleTarget{ProjectUID: p.UID, IssueMappings: mappings, KeepEnabled: in.KeepEnabled})
	if err != nil {
		return nil, cronbundle.Report{}, err
	}
	if !in.DryRun {
		h = h.withLongRunningClient()
	}
	report, err := cronbundle.Import(ctx, cronBundleDaemon{h.options.Client}, p, plan, cronbundle.ImportOptions{Replace: in.Replace, DryRun: in.DryRun, Actor: h.options.Actor})
	// A stopped import keeps completed and uncertain items visible to the caller.
	return &sdkmcp.CallToolResult{IsError: err != nil}, report, nil
}

type cronBundleDaemon struct{ client *kataclient.Client }

func (d cronBundleDaemon) Read(ctx context.Context, projectID int64, kind, id string) (int, []byte, error) {
	if kind == "job" {
		if id == "" {
			r, err := d.client.ListCronJobsWithResponse(ctx, &generated.ListCronJobsRequestOptions{PathParams: &generated.ListCronJobsPath{ProjectID: projectID}})
			if r != nil {
				return r.StatusCode, r.Body, nil
			}
			return 0, nil, err
		}
		r, err := d.client.ShowCronJobWithResponse(ctx, &generated.ShowCronJobRequestOptions{PathParams: &generated.ShowCronJobPath{ProjectID: projectID, CronUID: id}})
		if r != nil {
			return r.StatusCode, r.Body, nil
		}
		return 0, nil, err
	}
	if id == "" {
		r, err := d.client.ListCronWorkflowsWithResponse(ctx, &generated.ListCronWorkflowsRequestOptions{PathParams: &generated.ListCronWorkflowsPath{ProjectID: projectID}})
		if r != nil {
			return r.StatusCode, r.Body, nil
		}
		return 0, nil, err
	}
	r, err := d.client.ShowCronWorkflowWithResponse(ctx, &generated.ShowCronWorkflowRequestOptions{PathParams: &generated.ShowCronWorkflowPath{ProjectID: projectID, CronUID: id}})
	if r != nil {
		return r.StatusCode, r.Body, nil
	}
	return 0, nil, err
}
func (d cronBundleDaemon) Put(ctx context.Context, projectID int64, kind, id string, body cronbundle.PutBody) (int, []byte, error) {
	var expected *string
	if body.Expected != "" {
		expected = &body.Expected
	}
	if kind == "job" {
		var definition generated.CronJobDefinition
		if err := json.Unmarshal(body.Definition, &definition); err != nil {
			return 0, nil, err
		}
		r, err := d.client.ReplaceCronJobWithResponse(ctx, &generated.ReplaceCronJobRequestOptions{PathParams: &generated.ReplaceCronJobPath{ProjectID: projectID, CronUID: id}, Body: &generated.ReplaceCronJobBody{Actor: &body.Actor, Name: body.Name, Definition: definition, ExpectedEventUID: expected}})
		if r != nil {
			return r.StatusCode, r.Body, nil
		}
		return 0, nil, err
	}
	var definition generated.CronWorkflowDefinition
	if err := json.Unmarshal(body.Definition, &definition); err != nil {
		return 0, nil, err
	}
	r, err := d.client.ReplaceCronWorkflowWithResponse(ctx, &generated.ReplaceCronWorkflowRequestOptions{PathParams: &generated.ReplaceCronWorkflowPath{ProjectID: projectID, CronUID: id}, Body: &generated.ReplaceCronWorkflowBody{Actor: &body.Actor, Name: body.Name, Definition: definition, ExpectedEventUID: expected}})
	if r != nil {
		return r.StatusCode, r.Body, nil
	}
	return 0, nil, err
}
