package mcpserver

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/internal/cronread"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/client/generated"
	"golang.org/x/mod/semver"
)

// CronJobsOutput preserves project-list rows and adds fleet project labels.
type CronJobsOutput struct {
	Jobs     []generated.CronJob          `json:"jobs"`
	Projects []generated.CronFleetProject `json:"projects,omitempty"`
}

// CronRunsOutput preserves history pages and adds fleet project labels.
type CronRunsOutput struct {
	Runs          []generated.CronRun          `json:"runs"`
	NextBeforeUID *string                      `json:"next_before_uid,omitempty"`
	Filters       *generated.CronRunFilters    `json:"filters,omitempty"`
	Projects      []generated.CronFleetProject `json:"projects,omitempty"`
}

func (h toolHandlers) requireCronFleetAPI(ctx context.Context) error {
	health, err := h.options.Client.Health(ctx)
	if err != nil {
		return err
	}
	version := ""
	if health.APISchemaVersion != nil {
		version = strings.TrimSpace(*health.APISchemaVersion)
	}
	if !semver.IsValid("v"+version) || semver.Compare("v"+version, "v0.39.0") < 0 {
		return errors.New("daemon does not support cross-project cron reads; requires API 0.39.0 or newer; upgrade the daemon")
	}
	return nil
}

func (h toolHandlers) cronReadScope(ctx context.Context, selector string) (projects []ProjectIdentity, ids []int64, global bool, err error) {
	global = h.options.Scope.Mode() != ScopeBound && strings.TrimSpace(selector) == ""
	if global {
		if err = h.requireCronFleetAPI(ctx); err != nil {
			return
		}
	}
	projects, err = h.readProjects(ctx, selector)
	if err != nil {
		return
	}
	if global && h.options.Scope.Mode() == ScopeAllowlist {
		ids = make([]int64, 0, len(projects))
		for _, p := range projects {
			ids = append(ids, p.ID)
		}
	}
	return
}

func cronRunQuery(in CronRunsInput) (db.CronRunList, error) {
	limit := defaultResultLimit
	if in.Limit != nil {
		if *in.Limit < 1 || *in.Limit > maximumResultLimit {
			return db.CronRunList{}, errors.New("limit must be from 1 through 100")
		}
		limit = int(*in.Limit)
	}
	for _, id := range []*string{&in.JobUID, &in.BeforeUID} {
		if *id != "" {
			normalized, err := cronUID(*id)
			if err != nil {
				return db.CronRunList{}, err
			}
			*id = normalized
		}
	}
	query := db.CronRunList{AllProjects: true, Limit: limit, BeforeUID: in.BeforeUID, JobUID: in.JobUID, Statuses: in.Status, IssueUID: in.IssueUID, ExecutorLabel: in.ExecutorLabel, Teammate: in.Teammate}
	if in.Since != "" {
		at, err := time.Parse(time.RFC3339Nano, in.Since)
		if err != nil || at.IsZero() {
			return query, errors.New("since must be a nonzero RFC3339 instant")
		}
		query.Since = at
	}
	return db.NormalizeCronRunList(query)
}

func verifyCronFilters(value any, want db.CronRunFilters) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return cronread.VerifyFilters(raw, want)
}

// Inspect original arguments before optional Go strings collapse explicit empty
// values into omission. Only omitted selectors may select the startup scope.
func validateCronReadArguments(request *sdkmcp.CallToolRequest, runs bool) error {
	if request == nil || request.Params == nil || len(request.Params.Arguments) == 0 {
		return nil
	}
	var fields map[string]any
	if err := json.Unmarshal(request.Params.Arguments, &fields); err != nil {
		return err
	}
	names := []string{"project"}
	if runs {
		names = append(names, "issue_uid", "executor_label", "teammate", "since")
	}
	for _, name := range names {
		if raw, present := fields[name]; present {
			value, ok := raw.(string)
			if !ok || strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s must be a nonblank string when provided", name)
			}
		}
	}
	if raw, present := fields["status"]; runs && present {
		values, ok := raw.([]any)
		if !ok || len(values) == 0 {
			return errors.New("status must be a nonempty array when provided")
		}
	}
	return nil
}
