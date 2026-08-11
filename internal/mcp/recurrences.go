package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/pkg/client/generated"
)

// RecurrencesInput lists recurrences or selects one UID.
type RecurrencesInput struct {
	Project string `json:"project,omitempty"`
	UID     string `json:"uid,omitempty"`
}

// RecurrencesOutput contains scoped recurrence records.
type RecurrencesOutput struct {
	Project     ProjectIdentity        `json:"project"`
	Recurrences []generated.Recurrence `json:"recurrences"`
}

// RecurrenceUpdateInput creates or patches a recurrence.
type RecurrenceUpdateInput struct {
	Project         string                                   `json:"project,omitempty"`
	Action          string                                   `json:"action"`
	UID             string                                   `json:"uid,omitempty"`
	Revision        *int64                                   `json:"revision,omitempty"`
	DTStart         string                                   `json:"dtstart,omitempty"`
	RRule           string                                   `json:"rrule,omitempty"`
	Timezone        string                                   `json:"timezone,omitempty"`
	InitialIssueRef string                                   `json:"initial_issue_ref,omitempty"`
	Template        *generated.RecurrenceTemplateInput       `json:"template,omitempty"`
	TemplatePatch   *generated.RecurrenceTemplateUpdateInput `json:"template_patch,omitempty"`
}

// RecurrenceUpdateOutput reports the current recurrence record.
type RecurrenceUpdateOutput struct {
	Project    ProjectIdentity      `json:"project"`
	Recurrence generated.Recurrence `json:"recurrence"`
	Changed    bool                 `json:"changed"`
}

// RecurrenceDeleteInput selects a recurrence at a visible revision.
type RecurrenceDeleteInput struct {
	Project  string `json:"project,omitempty"`
	UID      string `json:"uid"`
	Revision int64  `json:"revision"`
}

// RecurrenceDeleteOutput reports a completed recurrence deletion.
type RecurrenceDeleteOutput struct {
	Project ProjectIdentity `json:"project"`
	UID     string          `json:"uid"`
	Deleted bool            `json:"deleted"`
}

func registerRecurrenceTools(server *sdkmcp.Server, handlers toolHandlers) {
	addTool(server, "kata.recurrences", "Read recurrences", "List recurrences or show one recurrence in an in-scope project.", toolHints(true, false, false), handlers.recurrences)
	addTool(server, "kata.recurrence_update", "Update recurrence", "Create a recurrence or patch one at its required visible revision.", toolHints(false, false, false), handlers.recurrenceUpdate)
	addTool(server, "kata.recurrence_delete", "Delete recurrence", "Delete a recurrence at its visible revision.", toolHints(false, true, false), handlers.recurrenceDelete)
}

func (h toolHandlers) recurrences(ctx context.Context, _ *sdkmcp.CallToolRequest, input RecurrencesInput) (*sdkmcp.CallToolResult, RecurrencesOutput, error) {
	project, err := h.options.Scope.Project(ctx, h.options.Client, input.Project, false)
	if err != nil {
		return nil, RecurrencesOutput{}, err
	}
	if uid := strings.TrimSpace(input.UID); uid != "" {
		response, showErr := h.options.Client.ShowRecurrence(ctx, &generated.ShowRecurrenceRequestOptions{PathParams: &generated.ShowRecurrencePath{ProjectID: project.ID, RecurrenceUID: uid}})
		if showErr != nil {
			return nil, RecurrencesOutput{}, showErr
		}
		return successResult(), RecurrencesOutput{Project: project, Recurrences: []generated.Recurrence{response.Recurrence}}, nil
	}
	response, err := h.options.Client.ListRecurrences(ctx, &generated.ListRecurrencesRequestOptions{PathParams: &generated.ListRecurrencesPath{ProjectID: project.ID}})
	if err != nil {
		return nil, RecurrencesOutput{}, err
	}
	return successResult(), RecurrencesOutput{Project: project, Recurrences: response.Recurrences}, nil
}

func (h toolHandlers) recurrenceUpdate(ctx context.Context, _ *sdkmcp.CallToolRequest, input RecurrenceUpdateInput) (*sdkmcp.CallToolResult, RecurrenceUpdateOutput, error) {
	project, err := h.options.Scope.Project(ctx, h.options.Client, input.Project, false)
	if err != nil {
		return nil, RecurrenceUpdateOutput{}, err
	}
	switch input.Action {
	case "create":
		if input.Template == nil {
			return nil, RecurrenceUpdateOutput{}, errors.New("create requires template")
		}
		response, createErr := h.options.Client.CreateRecurrence(ctx, &generated.CreateRecurrenceRequestOptions{
			PathParams: &generated.CreateRecurrencePath{ProjectID: project.ID},
			Body:       &generated.CreateRecurrenceBody{Actor: &h.options.Actor, Dtstart: input.DTStart, Rrule: input.RRule, Timezone: input.Timezone, InitialIssueRef: optionalString(input.InitialIssueRef), Template: *input.Template},
		})
		if createErr != nil {
			return nil, RecurrenceUpdateOutput{}, createErr
		}
		return successResult(), RecurrenceUpdateOutput{Project: project, Recurrence: response.Recurrence, Changed: true}, nil
	case "patch":
		uid := strings.TrimSpace(input.UID)
		if uid == "" || input.Revision == nil || *input.Revision < 1 {
			return nil, RecurrenceUpdateOutput{}, errors.New("patch requires uid and a positive revision")
		}
		etag := `"rev-` + strconv.FormatInt(*input.Revision, 10) + `"`
		headers := &generated.PatchRecurrenceHeaders{IfMatch: &etag}
		body := &generated.PatchRecurrenceBody{Actor: &h.options.Actor, Dtstart: optionalString(input.DTStart), Rrule: optionalString(input.RRule), Timezone: optionalString(input.Timezone), Template: input.TemplatePatch}
		response, patchErr := h.options.Client.PatchRecurrence(ctx, &generated.PatchRecurrenceRequestOptions{PathParams: &generated.PatchRecurrencePath{ProjectID: project.ID, RecurrenceUID: uid}, Body: body, Header: headers})
		if patchErr != nil {
			return nil, RecurrenceUpdateOutput{}, patchErr
		}
		return successResult(), RecurrenceUpdateOutput{Project: project, Recurrence: response.Recurrence, Changed: response.Changed}, nil
	default:
		return nil, RecurrenceUpdateOutput{}, errors.New("action must be create or patch")
	}
}

func (h toolHandlers) recurrenceDelete(ctx context.Context, _ *sdkmcp.CallToolRequest, input RecurrenceDeleteInput) (*sdkmcp.CallToolResult, RecurrenceDeleteOutput, error) {
	project, err := h.options.Scope.Project(ctx, h.options.Client, input.Project, false)
	if err != nil {
		return nil, RecurrenceDeleteOutput{}, err
	}
	uid := strings.TrimSpace(input.UID)
	if uid == "" || input.Revision < 1 {
		return nil, RecurrenceDeleteOutput{}, errors.New("uid and a positive revision are required")
	}
	etag := fmt.Sprintf(`"rev-%d"`, input.Revision)
	_, err = h.options.Client.DeleteRecurrence(ctx, &generated.DeleteRecurrenceRequestOptions{PathParams: &generated.DeleteRecurrencePath{ProjectID: project.ID, RecurrenceUID: uid}, Query: &generated.DeleteRecurrenceQuery{Actor: &h.options.Actor}, Header: &generated.DeleteRecurrenceHeaders{IfMatch: etag}})
	if err != nil {
		return nil, RecurrenceDeleteOutput{}, err
	}
	return successResult(), RecurrenceDeleteOutput{Project: project, UID: uid, Deleted: true}, nil
}
