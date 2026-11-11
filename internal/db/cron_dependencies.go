package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/cron"
)

// CheckCronDependencies checks the live dependencies of a new job/run.
// Definition writers pass their transaction so referenced documents are checked
// consistently with the write. Frozen resumes use their
// stored snapshot and must not reload an edited workflow through this helper.
func CheckCronDependencies(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, projectID int64, definition cron.JobDefinition) error {
	check := func(table, kind, id string) error {
		if id == "" {
			return nil
		}
		deleted, err := cronDependency(ctx, q, projectID, table, kind, id, SupportedEventFeatures)
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

// cronDependency requires one dependency to exist in the project and reports
// whether it is tombstoned. A workflow also needs supported retained evidence:
// its definition event beats the normalized projection, and a compacted event
// leaves the projection. Callers decide what a tombstone means.
func cronDependency(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, projectID int64, table, kind, id, features string) (bool, error) {
	var owner int64
	var deleted, row, retained sql.NullString
	query, dest := "SELECT project_id,deleted_at FROM "+table+" WHERE uid=$1", []any{&owner, &deleted}
	if table == "cron_workflows" {
		query = `SELECT w.project_id,w.deleted_at,w.definition_json,e.payload FROM cron_workflows w LEFT JOIN events e ON e.uid=w.definition_event_uid WHERE w.uid=$1`
		dest = append(dest, &row, &retained)
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
	if len(dest) > 2 {
		if err := retainedWorkflowSupported(row.String, retained, features); err != nil {
			return false, fmt.Errorf("%w: workflow dependency is unsupported: %w", cron.ErrInvalid, err)
		}
	}
	return deleted.Valid, nil
}

// retainedWorkflowSupported requires the projection and its retained raw
// definition event, when still present, to decode under the strict contract.
func retainedWorkflowSupported(row string, event sql.NullString, features string) error {
	if err := checkRunWorkflowDocument(row, features); err != nil || !event.Valid {
		return err
	}
	var body CronDefinitionEvent
	if err := json.Unmarshal([]byte(event.String), &body, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	return checkRunWorkflowDocument(string(body.Definition), features)
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
