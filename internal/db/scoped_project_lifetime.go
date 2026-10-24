package db

import (
	"context"
	"database/sql"
	"errors"
)

// RejectScopedProjectRetirementTx preserves project UIDs named by retained
// generations and generation-free root, source, control and predicate state.
// Retired generations retain their immutable cuts or custody.
// Archiving keeps the UID and remains separate from merge/purge retirement.
func RejectScopedProjectRetirementTx(ctx context.Context, tx interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, projectUID string) error {
	// Owner restore also accepts local source retention and root floor state
	// before any view generation exists. Those rows outlive project deletion,
	// so refuse retirement before native domain writes or ID allocation.
	retained, err := tx.QueryContext(ctx, `SELECT project_uid FROM federation_scoped_projects WHERE project_uid=$1 UNION ALL SELECT project_uid FROM federation_private_sources WHERE project_uid=$1 UNION ALL SELECT project_uid FROM federation_predicate_incarnations WHERE project_uid=$1 LIMIT 1`, projectUID)
	if err != nil {
		return err
	}
	found := retained.Next()
	err = retained.Err()
	closeErr := retained.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if found {
		return errors.New("project UID is required by retained scoped state")
	}
	rows, err := tx.QueryContext(ctx, `SELECT 'generation',descriptor_bytes FROM federation_scoped_generations UNION ALL SELECT 'cutover',project_uids_bytes FROM federation_protocol_cutovers UNION ALL SELECT 'control',request_bytes FROM federation_control_custody UNION ALL SELECT 'source',manifest_bytes FROM federation_source_operations`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var raw []byte
		if err := rows.Scan(&kind, &raw); err != nil {
			return err
		}
		var projects []string
		switch kind {
		case "generation":
			var view ScopedViewDescriptor
			if err := DecodeScopedRecord(raw, &view); err != nil {
				return err
			}
			for _, selected := range view.Projects {
				projects = append(projects, selected.ProjectUID)
			}
		case "cutover":
			if err := DecodeScopedRecord(raw, &projects); err != nil {
				return err
			}
		case "control":
			var request ScopedControlRequest
			if err := DecodeScopedRecord(raw, &request); err != nil {
				return err
			}
			projects = []string{request.Subject.ProjectUID}
		case "source":
			submission, err := DecodeScopedSourceSubmission(raw)
			if err != nil {
				return err
			}
			projects, err = scopedOperationProjects(submission.Operation)
			if err != nil {
				return err
			}
			for _, proof := range submission.Operation.Membership {
				projects = append(projects, proof.Endpoint.ProjectUID)
			}
		}
		for _, project := range projects {
			if project == projectUID {
				return errors.New("project UID is required by retained scoped state")
			}
		}
	}
	return rows.Err()
}
