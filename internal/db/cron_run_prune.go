package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
)

// PruneCronRuns selects one batch of old terminal evidence at its authority.
// Before is an absolute instant. The daemon additionally enforces a 24h age.
type PruneCronRuns struct {
	ProjectID           int64
	Actor               string
	Before              time.Time
	Keep, Limit         int
	JobUID, WorkflowUID string
	DryRun              bool
}

// CronRunPruneGroup describes only the selected batch, not lifetime totals.
type CronRunPruneGroup struct {
	JobUID          *string        `json:"job_uid,omitempty"`
	WorkflowUID     *string        `json:"workflow_uid,omitempty"`
	Name            string         `json:"name,omitempty"`
	Counts          map[string]int `json:"counts"`
	OldestCreatedAt time.Time      `json:"oldest_created_at"`
	NewestCreatedAt time.Time      `json:"newest_created_at"`
}

// CronRunPruneResult is identical for preview and deletion except the cursor.
type CronRunPruneResult struct {
	MetadataEvent     Event               `json:"-"`
	DryRun            bool                `json:"dry_run"`
	Before            time.Time           `json:"before"`
	Keep              int                 `json:"keep"`
	Pruned            int                 `json:"pruned"`
	EventsDeleted     int                 `json:"events_deleted"`
	More              bool                `json:"more"`
	ResetAfterEventID int64               `json:"reset_after_event_id,omitempty"`
	Groups            []CronRunPruneGroup `json:"groups"`
}

type pruneRun struct {
	uid, kind, group, status string
	created                  time.Time
}

// PruneRuns atomically removes a bounded number of eligible runs and records
// a reset. Ranking may inspect the entire project; the deadline bounds work,
// while run/event deletion chunks bound parameter counts. No tombstones are kept.
func (a CronSQL) PruneRuns(ctx context.Context, in PruneCronRuns) (CronRunPruneResult, error) {
	if in.Keep == 0 {
		in.Keep = 10
	}
	if in.Limit == 0 {
		in.Limit = 5000
	}
	in.Before = in.Before.UTC()
	in.JobUID = strings.ToUpper(in.JobUID)
	in.WorkflowUID = strings.ToUpper(in.WorkflowUID)
	if in.Before.IsZero() || in.Before.Year() < 1 || in.Before.Year() > 9999 || in.Keep < 1 || in.Keep > 1000 || in.Limit < 1 || in.Limit > 20000 || (in.JobUID != "" && !cronUID(in.JobUID)) || (in.WorkflowUID != "" && !cronUID(in.WorkflowUID)) || (in.JobUID != "" && in.WorkflowUID != "") || strings.TrimSpace(in.Actor) == "" {
		return CronRunPruneResult{}, fmt.Errorf("%w: invalid prune window, keep, limit, group or actor", cron.ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result CronRunPruneResult
	err := a.transactProject(ctx, in.ProjectID, func(tx *sql.Tx) error {
		// Reset retry-local results: serializable transactions can run again.
		result = CronRunPruneResult{DryRun: in.DryRun, Before: in.Before, Keep: in.Keep, Groups: []CronRunPruneGroup{}}
		if err := LockCronProject(ctx, tx, in.ProjectID); err != nil {
			return err
		}
		project := Project{ID: in.ProjectID}
		if err := tx.QueryRowContext(ctx, `SELECT uid,name,metadata FROM projects WHERE id=$1 AND deleted_at IS NULL AND name<>$2`, in.ProjectID, SystemProjectName).Scan(&project.UID, &project.Name, &project.Metadata); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if err := CheckProjectAccessTransaction(ctx, tx, project.UID); err != nil {
			return err
		}
		tallies, err := DecodeCronTallies(project.Metadata)
		if err != nil {
			return err
		}
		if err := a.PruneGate(ctx, tx, project); err != nil {
			return err
		}
		actor, err := a.effectiveMutationActor(ctx, tx, in.ProjectID, in.Actor)
		if err != nil {
			return err
		}
		selected, more, err := a.selectPruneRuns(ctx, tx, in)
		if err != nil {
			return err
		}
		result.More = more
		result.Pruned = len(selected)
		if len(selected) == 0 {
			return nil
		}
		groups := map[string]int{}
		ids := map[string]bool{}
		for _, r := range selected {
			ids[r.uid] = true
			key := r.kind + ":" + r.group
			index, ok := groups[key]
			if !ok {
				g := CronRunPruneGroup{Counts: map[string]int{"succeeded": 0, "failed": 0, "cancelled": 0, "unknown": 0}, OldestCreatedAt: r.created, NewestCreatedAt: r.created}
				if r.kind == "job" {
					v := r.group
					g.JobUID = &v
				} else {
					v := r.group
					g.WorkflowUID = &v
				}
				table := "cron_jobs"
				if r.kind == "workflow" {
					table = "cron_workflows"
				}
				err := tx.QueryRowContext(ctx, `SELECT name FROM `+table+` WHERE project_id=$1 AND uid=$2`, in.ProjectID, r.group).Scan(&g.Name)
				if err != nil && err != sql.ErrNoRows {
					return err
				}
				index = len(result.Groups)
				groups[key] = index
				result.Groups = append(result.Groups, g)
			}
			g := &result.Groups[index]
			g.Counts[r.status]++
			if r.created.Before(g.OldestCreatedAt) {
				g.OldestCreatedAt = r.created
			}
			if r.created.After(g.NewestCreatedAt) {
				g.NewestCreatedAt = r.created
			}
		}
		result.EventsDeleted, err = pruneRunEvents(ctx, tx, in.ProjectID, ids, in.DryRun)
		if err != nil {
			return err
		}
		patch, err := accumulateCronTallies(tallies, result.Groups, time.Now().UTC())
		if err != nil {
			return err
		}
		if in.DryRun {
			return nil
		}
		values := make([]any, 0, len(selected))
		for _, run := range selected {
			values = append(values, run.uid)
		}
		if err := deleteProjectRows(ctx, tx, "cron_runs", "uid", in.ProjectID, values); err != nil {
			return err
		}
		// This event is below the mandatory reset boundary. Keep its event and
		// provenance for audit/hooks, while downstream delivery uses the new
		// checkpoint instead of an outbox body that precedes that reset.
		metadata, err := a.PatchProjectMetadataTx(WithRelayStateCapture(ctx), tx, PatchProjectMetadataIn{ProjectID: in.ProjectID, Actor: actor, Patch: patch})
		if err != nil {
			return err
		}
		result.MetadataEvent = metadata.Event
		result.ResetAfterEventID, err = a.ReserveResetCursor(ctx, tx)
		if err != nil {
			return err
		}
		return RecordProjectStreamResets(ctx, tx, []string{project.UID}, result.ResetAfterEventID)
	})
	if err != nil {
		return CronRunPruneResult{}, err
	}
	return result, nil
}

func (a CronSQL) selectPruneRuns(ctx context.Context, tx *sql.Tx, in PruneCronRuns) ([]pruneRun, bool, error) {
	// Kind prevents a valid job/workflow UID collision. Rank before narrowing:
	// workflow filtering must never redefine a job's newest retained evidence.
	// The existing project/job/time index can read each job's newest keep rows
	// directly. Derive groups from runs so deleted definitions stay protected.
	// Workflow-only rows have no equivalent index and retain window ranking.
	// SQLite expands each correlated top-keep result with json_each; PostgreSQL
	// uses LATERAL for the same indexed lookup. SQLite has no LATERAL syntax.
	protectedJobs := `SELECT kept.value FROM (
 SELECT DISTINCT job_uid FROM cron_runs WHERE project_id=$1 AND job_uid IS NOT NULL
 ) AS jobs CROSS JOIN json_each((
 SELECT json_group_array(uid) FROM (
 SELECT uid FROM cron_runs WHERE project_id=$1 AND job_uid=jobs.job_uid
 ORDER BY created_at DESC,uid DESC LIMIT $2) AS newest
 )) AS kept`
	if a.Postgres {
		protectedJobs = `SELECT newest.uid FROM (
 SELECT DISTINCT job_uid FROM cron_runs WHERE project_id=$1 AND job_uid IS NOT NULL
 ) AS jobs CROSS JOIN LATERAL (
 SELECT uid FROM cron_runs WHERE project_id=$1 AND job_uid=jobs.job_uid
 ORDER BY created_at DESC,uid DESC LIMIT $2) AS newest`
	}
	// UIDs are unique and non-null. Excluding these protected rows preserves
	// rank>keep without ranking and sorting every job-backed run.
	query := `SELECT uid,CASE WHEN job_uid IS NOT NULL THEN 'job' ELSE 'workflow' END AS kind,
 COALESCE(job_uid,workflow_uid) AS group_uid,status,created_at,updated_at
 FROM cron_runs WHERE project_id=$1 AND uid NOT IN (` + protectedJobs + `
 UNION ALL SELECT uid FROM (
 SELECT uid,ROW_NUMBER() OVER(PARTITION BY workflow_uid ORDER BY created_at DESC,uid DESC) AS rank
 FROM cron_runs WHERE project_id=$1 AND job_uid IS NULL) AS ranked WHERE rank<=$2)
 AND status IN ('succeeded','failed','cancelled','unknown') AND created_at<$3
 AND ($4='' OR job_uid=$4) AND ($5='' OR workflow_uid=$5)
 ORDER BY created_at ASC,uid ASC`
	rows, err := tx.QueryContext(ctx, query, in.ProjectID, in.Keep, in.Before.Format(CronRunCreatedAtFormat), in.JobUID, in.WorkflowUID)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	selected := []pruneRun{}
	for rows.Next() {
		var r pruneRun
		var created, updated string
		if err := rows.Scan(&r.uid, &r.kind, &r.group, &r.status, &created, &updated); err != nil {
			return nil, false, err
		}
		r.created, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, false, err
		}
		at, err := time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, false, err
		}
		if !r.created.Before(in.Before) || !at.Before(in.Before) {
			continue
		}
		if len(selected) == in.Limit {
			return selected, true, nil
		}
		selected = append(selected, r)
	}
	return selected, false, rows.Err()
}

func pruneRunEvents(ctx context.Context, tx *sql.Tx, projectID int64, ids map[string]bool, dry bool) (int, error) {
	// Stream once rather than planning and opening a query for every page.
	// Decode in Go so both backends share one parser instead of per-backend JSON SQL.
	rows, err := tx.QueryContext(ctx, `SELECT id,payload FROM events WHERE project_id=$1 AND id>0 AND type LIKE 'cron.%' AND type IN ('cron.run.observed','cron.run.snapshot') ORDER BY id`, projectID)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	count := 0
	var matches []any
	var value struct {
		UID string `json:"uid"`
	}
	for rows.Next() {
		var id int64
		var payload sql.RawBytes
		if err := rows.Scan(&id, &payload); err != nil {
			return 0, err
		}
		// RawBytes is valid until Next; decoding here avoids a payload copy.
		// Reset the reused target so a missing/null UID cannot inherit a match.
		value.UID = ""
		if err := json.Unmarshal(payload, &value); err != nil {
			return 0, err
		}
		if ids[value.UID] {
			count++
			if !dry {
				matches = append(matches, id)
			}
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return 0, err
	}
	// PostgreSQL needs the reader closed before writing on this transaction.
	// Only matching IDs are retained; preview needs no per-event storage.
	if !dry {
		if err := deleteProjectRows(ctx, tx, "events", "id", projectID, matches); err != nil {
			return 0, err
		}
	}
	return count, nil
}

func deleteProjectRows(ctx context.Context, tx *sql.Tx, table, column string, projectID int64, values []any) error {
	for start := 0; start < len(values); start += 500 {
		args := []any{projectID}
		slots := []string{}
		for _, value := range values[start:min(start+500, len(values))] {
			args = append(args, value)
			slots = append(slots, fmt.Sprintf("$%d", len(args)))
		}
		statement := `DELETE FROM ` + table + ` WHERE project_id=$1 AND ` + column + ` IN (` + strings.Join(slots, ",") + `)` // #nosec G202 -- table/column are internal literals; values stay bound parameters.
		if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
			return err
		}
	}
	return nil
}
