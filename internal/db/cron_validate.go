package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/uid"
)

// CronValidationInput describes an unsaved create or replacement candidate.
type CronValidationInput struct {
	ProjectID                   int64
	UID, ExpectedEventUID, Name string
	Raw                         []byte
	From                        time.Time
	Count                       int
}

// CronValidationError identifies the first failed stage of a preflight.
type CronValidationError struct {
	Stage              string `json:"stage"`
	Code               string `json:"code"`
	Message            string `json:"message"`
	DefinitionEventUID string `json:"definition_event_uid,omitempty"`
}

// CronValidationWarning explains a limitation of the hypothetical preview.
type CronValidationWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// CronValidation contains normalization and state checks, never a saved revision.
type CronValidation struct {
	Valid      bool                    `json:"valid"`
	Name       string                  `json:"name"`
	Definition jsontext.Value          `json:"definition"`
	Errors     []CronValidationError   `json:"errors"`
	Warnings   []CronValidationWarning `json:"warnings"`
	Schedule   *cron.SchedulePreview   `json:"schedule,omitempty"`
}

// ValidateJob checks a job using the same document and state rules as PutJob.
func (a CronSQL) ValidateJob(ctx context.Context, in CronValidationInput) (CronValidation, error) {
	return a.validateDefinition(ctx, in, true)
}

// ValidateWorkflow checks a workflow using the same rules as PutWorkflow.
func (a CronSQL) ValidateWorkflow(ctx context.Context, in CronValidationInput) (CronValidation, error) {
	return a.validateDefinition(ctx, in, false)
}

func (a CronSQL) validateDefinition(ctx context.Context, in CronValidationInput, job bool) (CronValidation, error) {
	out := CronValidation{Name: strings.TrimSpace(in.Name), Errors: []CronValidationError{}, Warnings: []CronValidationWarning{}}
	in.UID = strings.ToUpper(in.UID)
	if in.UID != "" && !uid.Valid(in.UID) {
		return out, fmt.Errorf("%w: invalid UID", cron.ErrInvalid)
	}
	if job && (in.Count < 1 || in.Count > 100) {
		return out, fmt.Errorf("%w: count must be between 1 and 100", cron.ErrInvalid)
	}
	fail := func(stage string, err error) error {
		if !errors.Is(err, cron.ErrInvalid) && !errors.Is(err, ErrCronConflict) && !errors.Is(err, ErrNotFound) {
			return err
		}
		code := "validation"
		if errors.Is(err, ErrCronConflict) {
			code = "cron_conflict"
		}
		out.Errors = append(out.Errors, CronValidationError{Stage: stage, Code: code, Message: err.Error()})
		return nil
	}
	err := a.ReadTransact(ctx, func(tx *sql.Tx) error {
		var projectID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE id=$1 AND deleted_at IS NULL AND name<>$2`, in.ProjectID, SystemProjectName).Scan(&projectID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if out.Name == "" || len(out.Name) > 256 {
			return fail("request", fmt.Errorf("%w: name required (maximum 256 bytes)", cron.ErrInvalid))
		}
		req := cronPutRequest{job: job, id: in.UID, projectID: in.ProjectID, expected: in.ExpectedEventUID}
		var definition cron.JobDefinition
		if job {
			definition, out.Definition, err = cron.NormalizeJob(in.Raw)
			req.definition = definition
		} else {
			var workflow cron.WorkflowDefinition
			workflow, out.Definition, err = cron.NormalizeWorkflow(in.Raw)
			req.definition = workflow
		}
		if err != nil {
			return fail("document", err)
		}
		// Look up globally to detect collisions, but never expose another project's revision.
		current, readErr := scanCron(tx.QueryRowContext(ctx, "SELECT "+cronDefinitionColumns+" FROM "+cronTable(job)+" WHERE uid=$1", in.UID))
		fresh := errors.Is(readErr, ErrNotFound)
		if readErr != nil && !fresh {
			return readErr
		}
		if err := checkCronPutState(req, current, fresh); err != nil {
			if err := fail("revision", err); err != nil {
				return err
			}
			if !fresh && current.ProjectID == in.ProjectID {
				out.Errors[0].DefinitionEventUID = current.DefinitionEventUID
			}
			return nil
		}
		if !job {
			out.Valid = true
			return nil
		}
		var old cron.JobDefinition
		if !fresh {
			old, err = cron.DecodeJob(current.document)
			if err != nil {
				return fail("ownership", err)
			}
		}
		progress, err := a.recurringOwnership(ctx, tx, req, old, false)
		if err != nil {
			return fail("ownership", err)
		}
		native := cron.IsRecurringIssueJob(definition)
		var parent Issue
		if native {
			parent, _, err = a.recurringParent(ctx, tx, in.ProjectID, definition.Issue.ParentUID, false)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			if err == nil && !cron.IsRecurringIssueJob(old) {
				if _, err := recurringInitialValues(parent, in.UID); err != nil {
					return fail("ownership", err)
				}
			}
		}
		if err := CheckCronDependencies(ctx, tx, in.ProjectID, definition); err != nil {
			return fail("dependencies", err)
		}
		if native {
			authority, _, err := recurringAuthority(ctx, tx, in.ProjectID)
			if err != nil {
				return err
			}
			if !authority {
				out.Warnings = append(out.Warnings, CronValidationWarning{Code: "hub_owned", Message: "native recurring writes are committed by the owning hub"})
			}
			if parent.Status == "closed" {
				out.Warnings = append(out.Warnings, CronValidationWarning{Code: "parent_closed", Message: "parent is closed; preview assumes it is open"})
			}
		}
		if !definition.Enabled {
			out.Warnings = append(out.Warnings, CronValidationWarning{Code: "paused", Message: "job is paused; preview assumes it is enabled"})
		}
		schedule, err := cron.PreviewSchedule(definition, progress, in.From, in.Count)
		if err != nil {
			return err
		}
		out.Valid, out.Schedule = true, &schedule
		return nil
	})
	return out, err
}
