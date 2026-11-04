package db

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/teammate"
)

// CronAllList selects active projects and a job tombstone policy.
type CronAllList struct {
	ProjectIDs     []int64
	IncludeDeleted bool
}

// CronRunFilters is the normalized set of predicates applied to a history page.
// Pagination and authorization scope are deliberately separate from filters.
type CronRunFilters struct {
	Statuses      []string   `json:"status,omitempty"`
	JobUID        string     `json:"job_uid,omitempty"`
	IssueUID      string     `json:"issue_uid,omitempty"`
	ExecutorLabel string     `json:"executor_label,omitempty"`
	Teammate      string     `json:"teammate,omitempty"`
	Since         *time.Time `json:"since,omitempty"`
}

// Filters returns the applied predicates after NormalizeCronRunList.
func (in CronRunList) Filters() CronRunFilters {
	out := CronRunFilters{Statuses: in.Statuses, JobUID: in.JobUID, IssueUID: in.IssueUID, ExecutorLabel: in.ExecutorLabel, Teammate: in.Teammate}
	if !in.Since.IsZero() {
		at := in.Since
		out.Since = &at
	}
	return out
}

// NormalizeCronRunList validates filters for API, local storage and clients.
func NormalizeCronRunList(in CronRunList) (CronRunList, error) {
	invalid := func(message string) (CronRunList, error) { return in, fmt.Errorf("%w: %s", cron.ErrInvalid, message) }
	if in.Limit < 0 {
		return invalid("negative limit")
	}
	if in.Limit == 0 || in.Limit > 100 {
		in.Limit = 100
	}
	if in.AllProjects {
		if in.ProjectID != 0 {
			return invalid("project and all-project scopes are mutually exclusive")
		}
		for _, id := range in.ProjectIDs {
			if id <= 0 {
				return invalid("project_ids must contain positive integers")
			}
		}
		if in.ProjectIDs != nil && len(in.ProjectIDs) == 0 {
			return invalid("project_ids must not be empty")
		}
	} else if in.ProjectID <= 0 || in.ProjectIDs != nil {
		return invalid("one project is required")
	}
	in.ProjectIDs = slices.Clone(in.ProjectIDs)
	slices.Sort(in.ProjectIDs)
	in.ProjectIDs = slices.Compact(in.ProjectIDs)
	for _, id := range []*string{&in.JobUID, &in.IssueUID, &in.BeforeUID} {
		*id = strings.ToUpper(*id)
		if *id != "" && !cronUID(*id) {
			return invalid("invalid run filter UID or cursor")
		}
	}
	in.Statuses = slices.Clone(in.Statuses)
	for _, status := range in.Statuses {
		switch status {
		case "running", "succeeded", "failed", "cancelled", "unknown":
		default:
			return invalid("unknown run status")
		}
	}
	slices.Sort(in.Statuses)
	in.Statuses = slices.Compact(in.Statuses)
	if in.ExecutorLabel != "" && (strings.TrimSpace(in.ExecutorLabel) == "" || len(in.ExecutorLabel) > 256 || !utf8.ValidString(in.ExecutorLabel) || strings.ContainsRune(in.ExecutorLabel, 0)) {
		return invalid("invalid executor label")
	}
	if err := teammate.Validate(in.Teammate); err != nil {
		return invalid(err.Error())
	}
	if !in.Since.IsZero() {
		in.Since = in.Since.UTC()
		if in.Since.Year() < 0 || in.Since.Year() > 9999 {
			return invalid("invalid since instant")
		}
	}
	return in, nil
}

// cronProjectPredicate is shared by fleet jobs, runs and cursor lookups. The
// authorized boundary is applied before ordering, limiting or cursor hydration.
func cronProjectPredicate(ctx context.Context, ids []int64, args *[]any) string {
	bind := func(value any) string { *args = append(*args, value); return fmt.Sprintf("$%d", len(*args)) }
	query := "project_id IN (SELECT id FROM projects WHERE deleted_at IS NULL AND " + AuthorizedProjectPredicate(ctx, "uid", bind)
	if ids != nil {
		if len(ids) == 0 {
			query += " AND 1=0"
		} else {
			places := make([]string, len(ids))
			for n, id := range ids {
				places[n] = bind(id)
			}
			query += " AND id IN (" + strings.Join(places, ",") + ")"
		}
	}
	return query + ")"
}

// AllJobs lists definitions across active, authorized projects in stable order.
func (a CronSQL) AllJobs(ctx context.Context, in CronAllList) ([]CronJob, error) {
	for _, id := range in.ProjectIDs {
		if id <= 0 {
			return nil, fmt.Errorf("%w: invalid project ID", cron.ErrInvalid)
		}
	}
	rows, err := a.listJobs(ctx, in)
	if err != nil {
		return nil, err
	}
	result := []CronJob{}
	for _, row := range rows {
		job, err := jobFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, nil
}

func (a CronSQL) listJobs(ctx context.Context, in CronAllList) ([]cronRow, error) {
	args := []any{}
	query := "SELECT " + cronDefinitionColumns + " FROM cron_jobs WHERE " + cronProjectPredicate(ctx, in.ProjectIDs, &args)
	if !in.IncludeDeleted {
		query += " AND deleted_at IS NULL"
	}
	query += " ORDER BY project_id,name,uid"
	return a.readCronRows(ctx, query, args...)
}
