package api

import (
	"net/url"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/db"
)

// CronRunQuery applies the same filters on project and fleet history.
type CronRunQuery struct {
	JobUID        string   `query:"job_uid"`
	Limit         int      `query:"limit" minimum:"0" maximum:"100"`
	BeforeUID     string   `query:"before_uid"`
	Statuses      []string `query:"status,explode"`
	IssueUID      string   `query:"issue_uid"`
	ExecutorLabel string   `query:"executor_label"`
	Teammate      string   `query:"teammate"`
	Since         string   `query:"since" doc:"Inclusive recording time (created_at), as an RFC 3339 instant"`
}

// Native validates and normalizes filters before they reach storage.
func (in CronRunQuery) Native(projectID int64, projectIDs []int64, all bool) (db.CronRunList, error) {
	out := db.CronRunList{ProjectID: projectID, ProjectIDs: projectIDs, AllProjects: all, JobUID: in.JobUID, Limit: in.Limit, BeforeUID: in.BeforeUID, Statuses: in.Statuses, IssueUID: in.IssueUID, ExecutorLabel: in.ExecutorLabel, Teammate: in.Teammate}
	if in.Since != "" {
		at, err := time.Parse(time.RFC3339Nano, in.Since)
		if err != nil || at.IsZero() {
			return out, NewError(400, "validation", "since must be a nonzero RFC 3339 instant", "", nil)
		}
		out.Since = at
	}
	normalized, err := db.NormalizeCronRunList(out)
	if err != nil {
		return out, NewError(400, "validation", err.Error(), "", nil)
	}
	return normalized, nil
}

func (in *CronRunQuery) resolve(ctx huma.Context) []error {
	q, err := cronQueryValues(ctx)
	if err != nil {
		return []error{err}
	}
	in.Statuses = append([]string(nil), q["status"]...)
	for _, name := range []string{"issue_uid", "executor_label", "teammate", "since"} {
		if q.Has(name) && (len(q[name]) != 1 || q.Get(name) == "") {
			return []error{NewError(400, "validation", name+" must have one nonempty value", "", nil)}
		}
	}
	return nil
}

// CronProjectSelection rejects an explicit empty project set before it can
// become an unrestricted fleet read.
type CronProjectSelection struct {
	ProjectIDs []int64 `query:"project_ids,explode"`
}

func (in *CronProjectSelection) resolve(ctx huma.Context) []error {
	q, err := cronQueryValues(ctx)
	if err != nil {
		return []error{err}
	}
	if !q.Has("project_ids") {
		return nil
	}
	in.ProjectIDs = make([]int64, 0, len(q["project_ids"]))
	for _, raw := range q["project_ids"] {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return []error{NewError(400, "validation", "project_ids must contain positive integers", "", nil)}
		}
		in.ProjectIDs = append(in.ProjectIDs, id)
	}
	return nil
}

// AllCronRunsRequest selects a bounded page across active authorized projects.
type AllCronRunsRequest struct {
	CronProjectSelection
	CronRunQuery
}

// Resolve preserves repeated parameters on adapters that bind only one value.
func (in *AllCronRunsRequest) Resolve(ctx huma.Context) []error {
	return append(in.CronProjectSelection.resolve(ctx), in.CronRunQuery.resolve(ctx)...)
}

// AllCronJobsRequest selects job definitions across active authorized projects.
type AllCronJobsRequest struct {
	CronProjectSelection
	IncludeDeleted bool `query:"include_deleted"`
}

// Resolve preserves all repeated project selectors.
func (in *AllCronJobsRequest) Resolve(ctx huma.Context) []error {
	return in.resolve(ctx)
}

// CronFleetProject labels a project referenced by a fleet result.
type CronFleetProject struct {
	ID   int64  `json:"id"`
	UID  string `json:"uid"`
	Name string `json:"name"`
}

// AllCronRunsResponse adds project labels to an ordinary history page.
type AllCronRunsResponse struct {
	Body struct {
		Runs          []CronRun          `json:"runs"`
		NextBeforeUID string             `json:"next_before_uid,omitempty"`
		Filters       *db.CronRunFilters `json:"filters,omitempty"`
		Projects      []CronFleetProject `json:"projects"`
	}
}

// AllCronJobsResponse returns job definitions and their project labels.
type AllCronJobsResponse struct {
	Body struct {
		Jobs     []CronJob          `json:"jobs"`
		Projects []CronFleetProject `json:"projects"`
	}
}

// URL.Query discards malformed pairs; ParseQuery must succeed before omission
// can mean an unrestricted selection or an absent predicate.
func cronQueryValues(ctx huma.Context) (url.Values, error) {
	requestURL := ctx.URL()
	q, err := url.ParseQuery(requestURL.RawQuery)
	if err != nil {
		return nil, NewError(400, "validation", "invalid query encoding", "", nil)
	}
	return q, nil
}
