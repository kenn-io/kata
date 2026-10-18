package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func registerCronPreviewHandlers(h huma.API, cfg ServerConfig) {
	path := "/api/v1/projects/{project_id}/cron"
	huma.Register(h, huma.Operation{OperationID: "previewCronJob", Method: http.MethodGet, Path: path + "/jobs/{cron_uid}/preview"}, func(ctx context.Context, in *api.PreviewCronJobRequest) (*api.CronPreviewResponse, error) {
		count, err := strconv.Atoi(in.Count)
		if err != nil {
			return nil, previewValidation("count must be between 1 and 100")
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		job, err := cfg.DB.CronJob(ctx, in.ProjectID, in.UID)
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, in.ProjectID, in.UID, true)
		}
		return previewCron(ctx, cfg, in.ProjectID, job.Definition, &job, count, in.From)
	})
	huma.Register(h, huma.Operation{OperationID: "previewCronDefinition", Method: http.MethodPost, Path: path + "/preview"}, func(ctx context.Context, in *api.PreviewCronDefinitionRequest) (*api.CronPreviewResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		count := 10
		if in.Body.Count != nil {
			count = *in.Body.Count
		}
		return previewCron(ctx, cfg, in.ProjectID, in.Body.Definition.Native(), nil, count, in.Body.From)
	})
}
func previewValidation(message string) error {
	return api.NewError(400, "validation", message, "", nil)
}
func previewOccurrences(times []time.Time, loc *time.Location) []api.CronOccurrence {
	out := make([]api.CronOccurrence, 0, len(times))
	for _, at := range times {
		out = append(out, api.CronOccurrence{At: at.UTC(), Local: at.In(loc).Format(time.RFC3339)})
	}
	return out
}

func previewCron(ctx context.Context, cfg ServerConfig, projectID int64, def cron.JobDefinition, job *db.CronJob, count int, fromRaw string) (*api.CronPreviewResponse, error) {
	if count < 1 || count > 100 {
		return nil, previewValidation("count must be between 1 and 100")
	}
	var now time.Time
	if cfg.CronClock != nil {
		now = cfg.CronClock().UTC()
	} else {
		now = time.Now().UTC()
	}
	from := now
	if fromRaw != "" {
		parsed, err := time.Parse(time.RFC3339, fromRaw)
		if err != nil || parsed.IsZero() || parsed.UTC().Year() < 0 || parsed.UTC().Year() > 9999 {
			return nil, previewValidation("from must be an RFC3339 instant")
		}
		from = parsed.UTC()
	}
	switch def.Trigger.Kind {
	case "cron", "interval", "once":
	default:
		return nil, previewValidation("preview supports cron, interval and once triggers; this job uses " + def.Trigger.Kind)
	}
	if job == nil {
		if err := def.Validate(); err != nil {
			return nil, cronAPIError(ctx, cfg, err, projectID, "", true)
		}
	}
	out := &api.CronPreviewResponse{}
	p := &out.Body.Preview
	*p = api.CronPreview{ScheduledBy: "adapter", From: from, Count: count, Occurrences: []api.CronOccurrence{}}
	if job != nil {
		p.JobUID = job.UID
	}
	loc, locationErr := cron.RecurringLocation(def.Trigger)
	if locationErr == nil {
		p.Timezone = loc.String()
		if def.Trigger.Timezone != "" && def.Trigger.Timezone != p.Timezone {
			p.ConfiguredTimezone = def.Trigger.Timezone
		}
	}
	native := cron.IsRecurringIssueJob(def)
	var anchor time.Time
	var batch cron.RecurringBatch
	fail := func(err error) (*api.CronPreviewResponse, error) {
		if job == nil {
			return nil, previewValidation(err.Error())
		}
		p.Error = err.Error()
		return out, nil
	}
	if native {
		p.ScheduledBy = "kata"
		parent, reason, err := previewRecurringParent(ctx, cfg, projectID, def, job)
		if err != nil {
			return nil, err
		}
		active := reason == ""
		p.Active = &active
		p.InactiveReason = reason
		if job != nil {
			p.NextSweep = &api.CronNextSweep{Now: now}
			progress, err := db.RecurringProgressOf(parent, job.UID)
			if err != nil {
				p.NextSweep.Error = err.Error()
				return fail(err)
			}
			if fromRaw == "" && progress.Through.After(from) {
				from = progress.Through
				p.From = from
			}
			anchor = progress.Anchor
			p.NextSweep.Anchor = &progress.Anchor
			p.NextSweep.Through = &progress.Through
			revision := job.DefinitionEventUID
			enabled := def
			enabled.Enabled = true
			if !def.Enabled {
				revision, err = uid.New()
				if err != nil {
					return nil, internalAPIError(err)
				}
			}
			// Plan first: never list times for a stored job the sweep cannot evaluate.
			batch, err = cron.PlanRecurring(enabled, revision, progress, now)
			if err != nil {
				p.NextSweep.Error = err.Error()
				return fail(err)
			}
		} else {
			anchor = from
		}
	} else {
		p.Note = "the adapter owns overlap, catchup and activation; these are the trigger's occurrences only"
		if def.Trigger.Kind == "interval" {
			anchor = from
		}
	}
	if locationErr != nil {
		return fail(locationErr)
	}
	if def.Trigger.Kind == "interval" {
		p.Anchor = &anchor
		p.AnchorSource = "from"
		if native && job != nil {
			p.AnchorSource = "binding"
		}
	}
	if p.NextSweep != nil {
		p.NextSweep.Due = previewOccurrences(batch.Due, loc)
		p.NextSweep.More = batch.More
	}
	var times []time.Time
	var err error
	if def.Trigger.Kind == "once" {
		at, err := time.Parse(time.RFC3339, def.Trigger.At)
		if err != nil {
			return fail(fmt.Errorf("%w: once requires an RFC3339 instant", cron.ErrInvalid))
		}
		if at.UTC().Year() < 0 || at.UTC().Year() > 9999 {
			return fail(fmt.Errorf("%w: once instant exceeds supported UTC range", cron.ErrInvalid))
		}
		times = []time.Time{at.UTC()}
		p.Exhausted = true
	} else {
		times, err = cron.RecurringOccurrences(def.Trigger, anchor, from, count)
		if err != nil {
			return fail(err)
		}
		p.Exhausted = len(times) < count
	}
	p.Occurrences = previewOccurrences(times, loc)
	return out, nil
}

// previewRecurringParent uses the dispatcher's skip order, but reads only.
func previewRecurringParent(ctx context.Context, cfg ServerConfig, projectID int64, def cron.JobDefinition, job *db.CronJob) (db.Issue, string, error) {
	reason := ""
	binding, err := cfg.DB.FederationBindingByProject(ctx, projectID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return db.Issue{}, "", internalAPIError(err)
	}
	if err == nil && (binding.Role != db.FederationRoleHub || !binding.Enabled) {
		reason = "not_owning_hub"
	}
	if reason == "" && job != nil && job.DeletedAt != nil {
		reason = "job_deleted"
	}
	if reason == "" && !def.Enabled {
		reason = "disabled"
	}
	parent, err := cfg.DB.IssueByUID(ctx, def.Issue.ParentUID, db.IncludeDeletedYes)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return db.Issue{}, "", internalAPIError(err)
	}
	if errors.Is(err, db.ErrNotFound) || parent.ProjectID != projectID {
		// Do not expose a foreign parent's checkpoint through a restored definition.
		parent = db.Issue{Metadata: db.JSONBlob("{}")}
		if reason == "" {
			reason = "parent_missing"
		}
	} else if reason == "" {
		if parent.DeletedAt != nil {
			reason = "parent_deleted"
		} else if parent.Status != "open" {
			reason = "parent_not_open"
		}
	}
	return parent, reason, nil
}
