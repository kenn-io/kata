package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/internal/version"
)

func registerCronRuntimeHandlers(h huma.API, cfg ServerConfig) {
	base := "/api/v1/projects/{project_id}/cron/runtimes"
	huma.Register(h, huma.Operation{OperationID: "beatCronRuntime", Method: http.MethodPut, Path: base + "/{runtime_uid}", MaxBodyBytes: cron.RuntimeBeatLimit}, func(ctx context.Context, in *api.BeatCronRuntimeRequest) (*api.BeatCronRuntimeResponse, error) {
		ctx, actor, enrollment, err := cronJobWriteContext(ctx, cfg, in.ProjectID, in.Authorization, in.Body.Actor, "beatCronRuntime")
		if err != nil {
			return nil, err
		}
		project, err := activeProjectByID(ctx, cfg.DB, in.ProjectID)
		if err != nil {
			return nil, err
		}
		if !uid.Valid(in.RuntimeUID) || strings.ToUpper(in.RuntimeUID) != in.RuntimeUID {
			return nil, api.NewError(400, "validation", "runtime identity must be a canonical uppercase ULID", "", nil)
		}
		var fields map[string]jsontext.Value
		if err := json.Unmarshal(in.RawBody, &fields); err != nil {
			return nil, api.NewError(400, "validation", "invalid runtime JSON", "", nil)
		}
		for _, key := range []string{"state", "interval_seconds", "choices"} {
			if strings.TrimSpace(string(fields[key])) == "null" {
				return nil, api.NewError(400, "validation", "runtime "+key+" cannot be null", "", nil)
			}
		}
		beat, err := cron.NormalizeRuntimeBeat(in.Body.RuntimeBeat)
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, in.ProjectID, "", true)
		}
		// Memory writes cannot execute request fences themselves. Successful storage
		// admission is the authorization boundary; no network operation intervenes.
		if err := cronForwardAdmission(ctx, cfg.DB, project.ID, project.UID); err != nil {
			return nil, cronAPIError(ctx, cfg, err, in.ProjectID, "", true)
		}
		forwarder := ""
		if enrollment {
			forwarder = actor
		}
		runtime, err := cfg.CronRuntimes.beatNormalized(in.ProjectID, in.RuntimeUID, actor, beat, forwarder)
		if err != nil {
			return nil, api.NewError(429, "cron_runtime_limit", err.Error(), "retry after retained runtime evidence expires", nil)
		}
		out := &api.BeatCronRuntimeResponse{}
		out.Body.Runtime = runtime
		out.Body.Forwarded = forwardCronRuntime(ctx, cfg, in.ProjectID, in.RuntimeUID, fields, enrollment)
		return out, nil
	})
	huma.Register(h, huma.Operation{OperationID: "listCronRuntimes", Method: http.MethodGet, Path: base}, func(ctx context.Context, in *api.CronProjectRequest) (*api.CronRuntimesResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		jobs, err := cfg.DB.ListCronJobs(ctx, db.CronList{ProjectID: in.ProjectID})
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, in.ProjectID, "", true)
		}
		inputs := make([]cron.RuntimeJob, 0, len(jobs))
		for _, job := range jobs {
			inputs = append(inputs, cron.RuntimeJob{UID: job.UID, Definition: job.Definition})
		}
		return &api.CronRuntimesResponse{Body: cfg.CronRuntimes.List(in.ProjectID, inputs, version.Version)}, nil
	})
}
