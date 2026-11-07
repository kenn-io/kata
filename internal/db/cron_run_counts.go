package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
)

// CronRunCountsQuery selects aggregate identity, not workflow membership.
type CronRunCountsQuery struct {
	ProjectID           int64
	JobUID, WorkflowUID string
}

// CronRunRetainedCounts describes current rows in every observation status.
type CronRunRetainedCounts struct {
	Running   int64 `json:"running"`
	Succeeded int64 `json:"succeeded"`
	Failed    int64 `json:"failed"`
	Cancelled int64 `json:"cancelled"`
	Unknown   int64 `json:"unknown"`
}

// CronRunCountGroup separates current evidence from recorded pruning actions.
type CronRunCountGroup struct {
	Kind                string                `json:"kind"`
	UID                 string                `json:"uid"`
	Name                string                `json:"name,omitempty"`
	Retained            CronRunRetainedCounts `json:"retained"`
	PrunedSinceTracking CronTallyCounts       `json:"pruned_since_tracking"`
	CoverageStartedAt   *time.Time            `json:"coverage_started_at,omitempty"`
	LastPrunedAt        *time.Time            `json:"last_pruned_at,omitempty"`
	OldestCreatedAt     *time.Time            `json:"oldest_created_at,omitempty"`
	NewestCreatedAt     *time.Time            `json:"newest_created_at,omitempty"`
}

// CronRunCountsResult enumerates retained-run and tally groups only.
type CronRunCountsResult struct {
	Groups []CronRunCountGroup `json:"groups"`
}

// ReadCronRunCounts reads metadata, projections and names through one pinned
// backend snapshot. The caller owns transaction commit/isolation.
func ReadCronRunCounts(ctx context.Context, tx cronEventQuery, in CronRunCountsQuery) (CronRunCountsResult, error) {
	in.JobUID, in.WorkflowUID = strings.ToUpper(in.JobUID), strings.ToUpper(in.WorkflowUID)
	if (in.JobUID != "" && !cronUID(in.JobUID)) || (in.WorkflowUID != "" && !cronUID(in.WorkflowUID)) || (in.JobUID != "" && in.WorkflowUID != "") {
		return CronRunCountsResult{}, fmt.Errorf("%w: choose one valid job or workflow UID", cron.ErrInvalid)
	}
	var projectUID string
	var metadata JSONBlob
	err := tx.QueryRowContext(ctx, `SELECT uid,metadata FROM projects WHERE id=$1 AND deleted_at IS NULL AND name<>$2`, in.ProjectID, SystemProjectName).Scan(&projectUID, &metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return CronRunCountsResult{}, ErrNotFound
	}
	if err != nil {
		return CronRunCountsResult{}, err
	}
	if allowed, restricted := AuthorizedProjects(ctx); restricted && !slices.Contains(allowed, projectUID) {
		return CronRunCountsResult{}, ErrNotFound
	}
	if err := CheckProjectAccessSnapshot(ctx, tx, projectUID); err != nil {
		return CronRunCountsResult{}, err
	}
	tallies, err := DecodeCronTallies(metadata)
	if err != nil {
		return CronRunCountsResult{}, err
	}
	groups := map[string]*CronRunCountGroup{}
	groupFor := func(kind, id string) *CronRunCountGroup {
		key := kind + ":" + id
		if groups[key] == nil {
			groups[key] = &CronRunCountGroup{Kind: kind, UID: id}
		}
		return groups[key]
	}
	for groupUID, value := range tallies {
		for kind, entry := range value.Groups {
			group := groupFor(kind, groupUID)
			group.PrunedSinceTracking = entry.Counts
			group.CoverageStartedAt, group.LastPrunedAt = &entry.CoverageStartedAt, &entry.LastPrunedAt
			group.OldestCreatedAt, group.NewestCreatedAt = &entry.OldestCreatedAt, &entry.NewestCreatedAt
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN job_uid IS NOT NULL THEN 'job' ELSE 'workflow' END,COALESCE(job_uid,workflow_uid),status,COUNT(*) FROM cron_runs WHERE project_id=$1 GROUP BY CASE WHEN job_uid IS NOT NULL THEN 'job' ELSE 'workflow' END,COALESCE(job_uid,workflow_uid),status`, in.ProjectID)
	if err != nil {
		return CronRunCountsResult{}, err
	}
	for rows.Next() {
		var kind, id, status string
		var count int64
		if err := rows.Scan(&kind, &id, &status, &count); err != nil {
			_ = rows.Close()
			return CronRunCountsResult{}, err
		}
		group := groupFor(kind, id)
		switch status {
		case "running":
			group.Retained.Running = count
		case "succeeded":
			group.Retained.Succeeded = count
		case "failed":
			group.Retained.Failed = count
		case "cancelled":
			group.Retained.Cancelled = count
		case "unknown":
			group.Retained.Unknown = count
		default:
			_ = rows.Close()
			return CronRunCountsResult{}, fmt.Errorf("%w: unknown stored run status %q", cron.ErrInvalid, status)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return CronRunCountsResult{}, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT 'job',uid,name FROM cron_jobs WHERE project_id=$1 UNION ALL SELECT 'workflow',uid,name FROM cron_workflows WHERE project_id=$1`, in.ProjectID)
	if err != nil {
		return CronRunCountsResult{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind, id, name string
		if err := rows.Scan(&kind, &id, &name); err != nil {
			return CronRunCountsResult{}, err
		}
		if group := groups[kind+":"+id]; group != nil {
			group.Name = name
		}
	}
	if err := rows.Err(); err != nil {
		return CronRunCountsResult{}, err
	}
	result := CronRunCountsResult{Groups: []CronRunCountGroup{}}
	for _, group := range groups {
		if in.JobUID != "" && (group.Kind != "job" || group.UID != in.JobUID) {
			continue
		}
		if in.WorkflowUID != "" && (group.Kind != "workflow" || group.UID != in.WorkflowUID) {
			continue
		}
		result.Groups = append(result.Groups, *group)
	}
	slices.SortFunc(result.Groups, func(a, b CronRunCountGroup) int {
		if order := strings.Compare(a.Kind, b.Kind); order != 0 {
			return order
		}
		return strings.Compare(a.UID, b.UID)
	})
	return result, nil
}
