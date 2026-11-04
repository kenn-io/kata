package mcpserver

import (
	"context"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/pkg/client/generated"
)

// CronProjectPauseInput supplies a reason while retaining the configured actor.
type CronProjectPauseInput struct {
	CronProjectInput
	Reason string `json:"reason,omitempty" jsonschema:"Pause reason, up to 256 bytes"`
}

func registerCronProjectControlTools(server *sdkmcp.Server, h toolHandlers) {
	addCronTool(server, "kata.cron_project_pause", "Pause project cron", "Stop native cron delivery and ask adapters to launch no new work. Root-only; running work may finish. Repeating retains the original pause reason and timestamp.", toolHints(false, false, false), h.cronProjectPause)
	addCronTool(server, "kata.cron_project_resume", "Resume project cron", "Resume project cron policy at its owning hub. Preserves per-job flags; missed native children follow each job's catch-up policy.", toolHints(false, false, false), h.cronProjectResume)
}
func (h toolHandlers) cronProjectPause(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronProjectPauseInput) (*sdkmcp.CallToolResult, *generated.PauseCronProjectResponse, error) {
	project, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	return cronResult(h.options.Client.PauseCronProject(ctx, &generated.PauseCronProjectRequestOptions{PathParams: &generated.PauseCronProjectPath{ProjectID: project.ID}, Body: &generated.PauseCronProjectBody{Actor: &h.options.Actor, Reason: &in.Reason}}))
}
func (h toolHandlers) cronProjectResume(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronProjectInput) (*sdkmcp.CallToolResult, *generated.ResumeCronProjectResponse, error) {
	project, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	return cronResult(h.options.Client.ResumeCronProject(ctx, &generated.ResumeCronProjectRequestOptions{PathParams: &generated.ResumeCronProjectPath{ProjectID: project.ID}, Body: &generated.ResumeCronProjectBody{Actor: &h.options.Actor}}))
}
