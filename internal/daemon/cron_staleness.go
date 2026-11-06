package daemon

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func cronNow(cfg ServerConfig) time.Time {
	if cfg.CronClock != nil {
		return cfg.CronClock()
	}
	return time.Now()
}

func cronRunWithStaleness(ctx context.Context, cfg ServerConfig, run db.CronRun) (api.CronRun, error) {
	out := api.CronRunFrom(run)
	if run.Status != "running" {
		return out, nil
	}
	var timeout int64
	if run.JobUID != nil {
		job, err := cfg.DB.CronJob(ctx, run.ProjectID, *run.JobUID)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return out, err
		}
		timeout = job.Definition.TimeoutSeconds
	}
	out.Staleness = cron.Staleness(run.Status, run.UpdatedAt, timeout, cronNow(cfg))
	return out, nil
}

func cronRunsWithStaleness(ctx context.Context, cfg ServerConfig, projectID int64, runs []db.CronRun) ([]api.CronRun, error) {
	timeouts := map[string]int64{}
	for _, run := range runs {
		if run.Status == "running" && run.JobUID != nil {
			jobs, err := cfg.DB.ListCronJobs(ctx, db.CronList{ProjectID: projectID, IncludeDeleted: true})
			if err != nil {
				return nil, err
			}
			for _, job := range jobs {
				timeouts[job.UID] = job.Definition.TimeoutSeconds
			}
			break
		}
	}
	now := cronNow(cfg)
	out := make([]api.CronRun, 0, len(runs))
	for _, run := range runs {
		var timeout int64
		if run.JobUID != nil {
			timeout = timeouts[*run.JobUID]
		}
		value := api.CronRunFrom(run)
		value.Staleness = cron.Staleness(run.Status, run.UpdatedAt, timeout, now)
		out = append(out, value)
	}
	return out, nil
}
