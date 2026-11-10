package mcpserver

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/cronhistory"
	"go.kenn.io/kata/internal/shortid"
	"go.kenn.io/kata/pkg/client/generated"
)

// CronJobCreateInput creates one caller-identified definition.
type CronJobCreateInput struct {
	CronIdentityInput
	Name       string                      `json:"name"`
	Definition generated.CronJobDefinition `json:"definition"`
	ParentRef  string                      `json:"parent_ref,omitempty" jsonschema:"Optional in-project parent issue reference; must agree with definition.issue.parent_uid"`
}

// CronJobUpdateInput requires explicit compare-and-swap authority.
type CronJobUpdateInput struct {
	CronJobCreateInput
	ExpectedEventUID string `json:"expected_event_uid"`
}

// CronJobHistoryInput selects a page of locally retained definition versions.
type CronJobHistoryInput struct {
	CronIdentityInput
	Diff           bool   `json:"diff,omitempty" jsonschema:"Include exact field changes against each preceding retained version"`
	Limit          *int64 `json:"limit,omitempty" jsonschema:"Results from 1 through 100; omit for 20"`
	BeforeEventUID string `json:"before_event_uid,omitempty" jsonschema:"next_before_event_uid from the preceding page"`
}

// Drop generated decoding methods so JSON v2 can inline the read fields.
type cronJobHistoryFields generated.CronDefinitionVersion

// CronJobHistoryVersion adds optional changes to the generated read fields.
type CronJobHistoryVersion struct {
	cronJobHistoryFields
	Changes jsontext.Value `json:"changes,omitzero"`
}

// CronJobHistoryOutput preserves route metadata with optional per-version diffs.
type CronJobHistoryOutput struct {
	Versions           []CronJobHistoryVersion `json:"versions"`
	NextBeforeEventUID *string                 `json:"next_before_event_uid,omitempty"`
	SkippedEventUIDs   []string                `json:"skipped_event_uids,omitempty"`
}

func (h toolHandlers) cronJobHistory(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronJobHistoryInput) (*sdkmcp.CallToolResult, CronJobHistoryOutput, error) {
	var out CronJobHistoryOutput
	id, err := cronUID(in.UID)
	if err != nil {
		return nil, out, err
	}
	if in.Limit != nil && *in.Limit == 0 {
		return nil, out, errors.New("limit must be from 1 through 100")
	}
	if in.BeforeEventUID != "" {
		in.BeforeEventUID, err = cronUID(in.BeforeEventUID)
		if err != nil {
			return nil, out, err
		}
	}
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, out, err
	}
	response, err := h.options.Client.ListCronJobVersions(ctx, &generated.ListCronJobVersionsRequestOptions{
		PathParams: &generated.ListCronJobVersionsPath{ProjectID: p.ID, CronUID: id},
		Query:      &generated.ListCronJobVersionsQuery{Limit: in.Limit, BeforeEventUID: optionalString(in.BeforeEventUID)},
	})
	if err != nil {
		return nil, out, err
	}
	out.NextBeforeEventUID, out.SkippedEventUIDs = response.NextBeforeEventUID, response.SkippedEventUids
	out.Versions = make([]CronJobHistoryVersion, len(response.Versions))
	for i, version := range response.Versions {
		out.Versions[i].cronJobHistoryFields = cronJobHistoryFields(version)
	}
	if in.Diff {
		raw, err := json.Marshal(response)
		if err != nil {
			return nil, out, err
		}
		enriched, err := cronhistory.WithDiff(ctx, raw, func(ctx context.Context, cursor string) ([]byte, error) {
			older, err := h.options.Client.ListCronJobVersions(ctx, &generated.ListCronJobVersionsRequestOptions{
				PathParams: &generated.ListCronJobVersionsPath{ProjectID: p.ID, CronUID: id},
				Query:      &generated.ListCronJobVersionsQuery{Limit: new(int64(1)), BeforeEventUID: &cursor},
			})
			if err != nil {
				return nil, err
			}
			return json.Marshal(older)
		})
		if err != nil {
			return nil, out, err
		}
		// Decode only the added fields; retain generated definitions and their exact
		// json.Number literals from the initial response.
		var diff struct {
			Versions []struct {
				Changes jsontext.Value `json:"changes"`
			} `json:"versions"`
			Skipped []string `json:"skipped_event_uids"`
		}
		if err := json.Unmarshal(enriched, &diff); err != nil {
			return nil, out, err
		}
		for i := range out.Versions {
			out.Versions[i].Changes = diff.Versions[i].Changes
		}
		out.SkippedEventUIDs = diff.Skipped
	}
	return successResult(), out, nil
}

func (h toolHandlers) cronJobs(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronListInput) (*sdkmcp.CallToolResult, *generated.ListCronJobsResponse, error) {
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	return cronResult(h.options.Client.ListCronJobs(ctx, &generated.ListCronJobsRequestOptions{PathParams: &generated.ListCronJobsPath{ProjectID: p.ID}, Query: &generated.ListCronJobsQuery{IncludeDeleted: &in.IncludeDeleted}}))
}
func (h toolHandlers) cronJob(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronIdentityInput) (*sdkmcp.CallToolResult, *generated.ShowCronJobResponse, error) {
	id, err := cronUID(in.UID)
	if err != nil {
		return nil, nil, err
	}
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	return cronResult(h.options.Client.ShowCronJob(ctx, &generated.ShowCronJobRequestOptions{PathParams: &generated.ShowCronJobPath{ProjectID: p.ID, CronUID: id}}))
}
func (h toolHandlers) cronJobCreate(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronJobCreateInput) (*sdkmcp.CallToolResult, *generated.ReplaceCronJobResponse, error) {
	return h.cronPutJob(ctx, in, nil)
}
func (h toolHandlers) cronJobUpdate(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronJobUpdateInput) (*sdkmcp.CallToolResult, *generated.ReplaceCronJobResponse, error) {
	expected, err := cronExpected(in.ExpectedEventUID)
	if err != nil {
		return nil, nil, err
	}
	return h.cronPutJob(ctx, in.CronJobCreateInput, &expected)
}
func (h toolHandlers) cronPutJob(ctx context.Context, in CronJobCreateInput, expected *string) (*sdkmcp.CallToolResult, *generated.ReplaceCronJobResponse, error) {
	h = h.withLongRunningClient()
	id, err := cronUID(in.UID)
	if err != nil {
		return nil, nil, err
	}
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(in.ParentRef) != "" {
		parent, err := h.cronParent(ctx, p, in.ParentRef)
		if err != nil {
			return nil, nil, err
		}
		if in.Definition.Issue == nil {
			return nil, nil, errors.New("parent_ref requires definition.issue")
		}
		if in.Definition.Issue.ParentUID != nil && *in.Definition.Issue.ParentUID != parent {
			return nil, nil, errors.New("parent_ref disagrees with definition.issue.parent_uid")
		}
		in.Definition.Issue.ParentUID = &parent
	}
	raw, err := json.Marshal(in.Definition)
	if err != nil {
		return nil, nil, err
	}
	if _, err = cron.ParseJob(raw); err != nil {
		return nil, nil, err
	}
	return cronResult(h.options.Client.ReplaceCronJob(ctx, &generated.ReplaceCronJobRequestOptions{PathParams: &generated.ReplaceCronJobPath{ProjectID: p.ID, CronUID: id}, Body: &generated.ReplaceCronJobBody{Actor: &h.options.Actor, Name: in.Name, Definition: in.Definition, ExpectedEventUID: expected}}))
}
func (h toolHandlers) cronParent(ctx context.Context, p ProjectIdentity, raw string) (string, error) {
	parsed, err := shortid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if parsed.Project != "" && parsed.Project != p.Name {
		return "", errors.New("parent_ref must belong to the selected project")
	}
	ref := parsed.ShortID
	if parsed.ULID != "" {
		ref = parsed.ULID
	}
	shown, err := h.options.Client.ShowIssue(ctx, &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: p.ID, Ref: ref}})
	if err != nil {
		return "", err
	}
	if shown.Issue.ProjectID != p.ID || (p.UID != "" && (shown.Issue.ProjectUID == nil || *shown.Issue.ProjectUID != p.UID)) {
		return "", fmt.Errorf("parent_ref resolved outside project %q", p.Name)
	}
	return shown.Issue.UID, nil
}
func (h toolHandlers) cronJobDelete(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronActionInput) (*sdkmcp.CallToolResult, *generated.ArchiveCronJobResponse, error) {
	h = h.withLongRunningClient()
	id, err := cronUID(in.UID)
	if err != nil {
		return nil, nil, err
	}
	expected, err := cronExpected(in.ExpectedEventUID)
	if err != nil {
		return nil, nil, err
	}
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	return cronResult(h.options.Client.ArchiveCronJob(ctx, &generated.ArchiveCronJobRequestOptions{PathParams: &generated.ArchiveCronJobPath{ProjectID: p.ID, CronUID: id}, Body: &generated.ArchiveCronJobBody{Actor: &h.options.Actor, ExpectedEventUID: expected}}))
}
func (h toolHandlers) cronJobRestore(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronActionInput) (*sdkmcp.CallToolResult, *generated.RestoreCronJobResponse, error) {
	h = h.withLongRunningClient()
	id, err := cronUID(in.UID)
	if err != nil {
		return nil, nil, err
	}
	expected, err := cronExpected(in.ExpectedEventUID)
	if err != nil {
		return nil, nil, err
	}
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	return cronResult(h.options.Client.RestoreCronJob(ctx, &generated.RestoreCronJobRequestOptions{PathParams: &generated.RestoreCronJobPath{ProjectID: p.ID, CronUID: id}, Body: &generated.RestoreCronJobBody{Actor: &h.options.Actor, ExpectedEventUID: expected}}))
}
