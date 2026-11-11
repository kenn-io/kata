package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"

	"go.kenn.io/kata/internal/cron"
)

// CheckCronDependencies checks the live dependencies of a new job/run.
// Definition writers pass their transaction so referenced documents are checked
// consistently with the write. Frozen resumes use their
// stored snapshot and must not reload an edited workflow through this helper.
func CheckCronDependencies(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, postgres bool, projectID int64, definition cron.JobDefinition) error {
	check := func(table, kind, id string) error {
		if id == "" {
			return nil
		}
		deleted, err := cronDependency(ctx, q, postgres, projectID, table, kind, id)
		if err == nil && deleted {
			err = fmt.Errorf("%w: tombstoned %s dependency %s", cron.ErrInvalid, kind, id)
		}
		return err
	}
	if err := check("cron_workflows", "workflow", definition.Action.WorkflowUID); err != nil {
		return err
	}
	if cron.IsRecurringIssueJob(definition) {
		return check("issues", "parent issue", definition.Issue.ParentUID)
	}
	issueUID := definition.Trigger.IssueUID
	if definition.Issue != nil && definition.Issue.Kind == "existing" {
		if issueUID != "" && issueUID != definition.Issue.UID && definition.Action.Kind != "notify" {
			return fmt.Errorf("%w: issue target differs from date source", cron.ErrInvalid)
		}
		if err := check("issues", "source issue", issueUID); err != nil {
			return err
		}
		issueUID = definition.Issue.UID
	}
	return check("issues", "issue", issueUID)
}

// workflowRevisionSupport reports held retained evidence by exact revision.
func workflowRevisionSupport(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, postgres bool, projectID int64, revisions map[string]string) (map[string]error, error) {
	held, versions := map[string]error{}, slices.Collect(maps.Keys(revisions))
	raw, err := json.Marshal(versions)
	if err != nil || len(versions) == 0 {
		return held, err
	}
	rows, err := q.QueryContext(ctx, cronRetainedWorkflowRevisionQuery(postgres), projectID, string(raw))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var e FoldEvent
		var version sql.NullString
		var payload string
		if err := rows.Scan(&e.UID, &e.Type, &e.OriginInstanceUID, &e.HLCPhysicalMS, &e.HLCCounter, &version, &payload); err != nil {
			return nil, err
		}
		revision := e.UID
		if _, ok := revisions[version.String]; ok {
			revision = version.String
		}
		uid, ok := revisions[revision]
		if !ok || !workflowDefinitionEvent(e.Type) {
			continue
		}
		e.Payload = []byte(payload)
		body, value, err := parseCronDefinitionEvent(e)
		if err == nil && (value.DefinitionEventUID != revision || value.UID != uid) {
			continue
		}
		if err == nil {
			_, err = cron.DecodeWorkflow(body.Definition)
		}
		if err != nil {
			held[revision] = fmt.Errorf("%w: workflow revision is unsupported: %v", cron.ErrInvalid, err)
		}
	}
	return held, rows.Err()
}

// cronDependency checks project ownership, tombstones and workflow evidence.
func cronDependency(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, postgres bool, projectID int64, table, kind, id string) (bool, error) {
	var owner int64
	var deleted sql.NullString
	var revision, row string
	query, dest := "SELECT project_id,deleted_at FROM "+table+" WHERE uid=$1", []any{&owner, &deleted}
	if table == "cron_workflows" {
		query = "SELECT project_id,deleted_at,definition_event_uid,definition_json FROM cron_workflows WHERE uid=$1"
		dest = append(dest, &revision, &row)
	}
	err := q.QueryRowContext(ctx, query, id).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: missing %s dependency %s", cron.ErrInvalid, kind, id)
	}
	if err != nil {
		return false, fmt.Errorf("read %s dependency: %w", kind, err)
	}
	if owner != projectID {
		return false, fmt.Errorf("%w: %s dependency belongs to another project", cron.ErrInvalid, kind)
	}
	if table == "cron_workflows" {
		if _, err := cron.DecodeWorkflow([]byte(row)); err != nil {
			return false, fmt.Errorf("%w: workflow dependency is unsupported: %v", cron.ErrInvalid, err)
		}
		support, err := workflowRevisionSupport(ctx, q, postgres, projectID, map[string]string{revision: id})
		if err != nil {
			return false, err
		}
		if err := support[revision]; err != nil {
			return false, fmt.Errorf("%w: workflow dependency is unsupported: %v", cron.ErrInvalid, err)
		}
	}
	return deleted.Valid, nil
}

// checkWorkflowUnused refuses to tombstone a workflow that a live job in the
// project still runs. Such a job could not be edited again without first
// being repointed, so the user resolves the job before deleting the workflow.
func checkWorkflowUnused(ctx context.Context, tx *sql.Tx, postgres bool, projectID int64, workflowUID string) error {
	reference := `json_extract(definition_json,'$.action.workflow_uid')`
	if postgres {
		reference = `(definition_json::jsonb #>> '{action,workflow_uid}')`
	}
	var jobUID string
	//nolint:gosec // The reference expression is one of two fixed strings; values are bound.
	err := tx.QueryRowContext(ctx, `SELECT uid FROM cron_jobs WHERE project_id=$1 AND deleted_at IS NULL AND `+reference+`=$2 ORDER BY uid LIMIT 1`, projectID, workflowUID).Scan(&jobUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read jobs using workflow: %w", err)
	}
	return fmt.Errorf("%w: workflow %s is used by live job %s; update or delete that job first", cron.ErrInvalid, workflowUID, jobUID)
}
