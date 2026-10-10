package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/teammate"
)

// CronObservationLimit bounds a complete run observation or event in bytes.
const CronObservationLimit = 96 * 1024

func validateCronRun(value CronRun) error {
	if !cronUID(value.UID) || value.ProjectID <= 0 || strings.TrimSpace(value.Actor) == "" || len(value.Actor) > 256 || !utf8.ValidString(value.Actor) {
		return fmt.Errorf("%w: invalid run identity", cron.ErrInvalid)
	}
	for _, pair := range [][2]*string{{value.JobUID, value.DefinitionEventUID}, {value.WorkflowUID, value.WorkflowDefinitionEventUID}} {
		if (pair[0] == nil) != (pair[1] == nil) {
			return fmt.Errorf("%w: definition UID and event UID must be paired", cron.ErrInvalid)
		}
		if pair[0] != nil && (!cronUID(*pair[0]) || !cronUID(*pair[1])) {
			return fmt.Errorf("%w: invalid definition reference", cron.ErrInvalid)
		}
	}
	if value.JobUID == nil && value.WorkflowUID == nil {
		return fmt.Errorf("%w: a job or workflow reference is required", cron.ErrInvalid)
	}
	if value.IssueUID != nil && !cronUID(*value.IssueUID) {
		return fmt.Errorf("%w: invalid issue reference", cron.ErrInvalid)
	}
	if value.OccurrenceKey != nil && (!utf8.ValidString(*value.OccurrenceKey) || utf8.RuneCountInString(*value.OccurrenceKey) < 1 || utf8.RuneCountInString(*value.OccurrenceKey) > 1024) {
		return fmt.Errorf("%w: invalid occurrence key", cron.ErrInvalid)
	}
	for _, label := range []*string{value.Teammate, value.ExecutorLabel} {
		if label != nil && (strings.TrimSpace(*label) == "" || len(*label) > 256 || !utf8.ValidString(*label)) {
			return fmt.Errorf("%w: invalid run label", cron.ErrInvalid)
		}
	}
	if value.Teammate != nil {
		if err := teammate.Validate(*value.Teammate); err != nil {
			return fmt.Errorf("%w: %v", cron.ErrInvalid, err)
		}
	}
	switch value.Status {
	case "running", "succeeded", "failed", "cancelled", "unknown":
	default:
		return fmt.Errorf("%w: unknown run status", cron.ErrInvalid)
	}
	summary, err := json.Marshal(value.Summary)
	if err != nil {
		return err
	}
	if _, err = cron.ParseSummary(summary); err != nil {
		return err
	}
	for _, at := range []*time.Time{value.StartedAt, value.EndedAt} {
		if at != nil && (at.IsZero() || at.Year() < 0 || at.Year() > 9999) {
			return fmt.Errorf("%w: invalid evidence timestamp", cron.ErrInvalid)
		}
	}
	if value.StartedAt != nil && value.EndedAt != nil && value.EndedAt.Before(*value.StartedAt) {
		return fmt.Errorf("%w: end precedes start", cron.ErrInvalid)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(encoded) > CronObservationLimit {
		return fmt.Errorf("%w: run observation exceeds byte limit", cron.ErrInvalid)
	}
	return nil
}

// Run reads an independent observation through its project-scoped UID.
func (a CronSQL) Run(ctx context.Context, projectID int64, id string) (CronRun, error) {
	value, err := scanCronRunRow(a.Query.QueryRowContext(ctx, "SELECT "+cronRunColumns+" FROM cron_runs WHERE project_id=$1 AND uid=$2", projectID, strings.ToUpper(id)))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return CronRun(value), err
}

// CronRunCreatedAtFormat stores run creation instants in UTC at a fixed
// nanosecond width, so text order is time order and the project/job time
// indexes serve history pages directly. Both schemas enforce this width.
const CronRunCreatedAtFormat = "2006-01-02T15:04:05.000000000Z"

// Runs reads a bounded creation-time and UID-ordered project history page.
func (a CronSQL) Runs(ctx context.Context, in CronRunList) ([]CronRun, error) {
	if in.Limit < 0 {
		return nil, fmt.Errorf("%w: negative limit", cron.ErrInvalid)
	}
	if in.Limit == 0 || in.Limit > 100 {
		in.Limit = 100
	}
	in.JobUID = strings.ToUpper(in.JobUID)
	in.BeforeUID = strings.ToUpper(in.BeforeUID)
	query := "SELECT " + cronRunColumns + " FROM cron_runs WHERE project_id=$1"
	args := []any{in.ProjectID}
	if in.JobUID != "" {
		if !cronUID(in.JobUID) {
			return nil, fmt.Errorf("%w: invalid job UID", cron.ErrInvalid)
		}
		args = append(args, in.JobUID)
		query += fmt.Sprintf(" AND job_uid=$%d", len(args))
	}
	if in.BeforeUID != "" {
		if !cronUID(in.BeforeUID) {
			return nil, fmt.Errorf("%w: invalid cursor", cron.ErrInvalid)
		}
		before, err := a.Run(ctx, in.ProjectID, in.BeforeUID)
		if err != nil {
			return nil, err
		}
		args = append(args, before.CreatedAt.UTC().Format(CronRunCreatedAtFormat), before.UID)
		query += fmt.Sprintf(" AND (created_at<$%d OR (created_at=$%d AND uid<$%d))", len(args)-1, len(args)-1, len(args))
	}
	args = append(args, in.Limit)
	query += fmt.Sprintf(" ORDER BY created_at DESC,uid DESC LIMIT $%d", len(args))
	rows, err := a.Query.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []CronRun{}
	for rows.Next() {
		value, err := scanCronRunRow(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, CronRun(value))
	}
	return result, rows.Err()
}

// ObserveRun commits ordinary attributed evidence with identity and revision checks.
func (a CronSQL) ObserveRun(ctx context.Context, in ObserveCronRun) (CronRunObservationResult, error) {
	in.UID = strings.ToUpper(in.UID)
	value := CronRun{UID: in.UID, ProjectID: in.ProjectID, JobUID: in.JobUID, DefinitionEventUID: in.DefinitionEventUID, WorkflowUID: in.WorkflowUID, WorkflowDefinitionEventUID: in.WorkflowDefinitionEventUID, OccurrenceKey: in.OccurrenceKey, IssueUID: in.IssueUID, Actor: in.Actor, Teammate: in.Teammate, ExecutorLabel: in.ExecutorLabel, Status: in.Status, Summary: in.Summary, StartedAt: in.StartedAt, EndedAt: in.EndedAt}
	if err := validateCronRun(value); err != nil {
		return CronRunObservationResult{}, err
	}
	if in.ExpectedRevision < 0 {
		return CronRunObservationResult{}, fmt.Errorf("%w: negative expected revision", cron.ErrInvalid)
	}
	var result CronRunObservationResult
	err := a.transactProject(ctx, in.ProjectID, func(tx *sql.Tx) error {
		var err error
		result, err = a.observeRunTx(ctx, tx, in, value)
		return err
	})
	if err != nil {
		return CronRunObservationResult{}, err
	}
	return result, nil
}

func (a CronSQL) observeRunTx(ctx context.Context, tx *sql.Tx, in ObserveCronRun, value CronRun) (CronRunObservationResult, error) {
	result := CronRunObservationResult{Events: []Event{}}
	project, err := a.openCronProject(ctx, tx, in.ProjectID)
	if err != nil {
		return result, err
	}
	effectiveActor, err := a.effectiveMutationActor(ctx, tx, in.ProjectID, value.Actor)
	if err != nil {
		return result, err
	}
	requestedActor := value.Actor
	value.Actor = effectiveActor
	prior, err := scanCronRunRow(tx.QueryRowContext(ctx, "SELECT "+cronRunColumns+" FROM cron_runs WHERE uid=$1", in.UID))
	fresh := errors.Is(err, sql.ErrNoRows)
	if err != nil && !fresh {
		return result, err
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	value.CreatedAt, value.UpdatedAt, value.Revision = now, now, 1
	if fresh {
		if in.ExpectedRevision != 0 {
			return result, ErrCronConflict
		}
		if err := a.checkFreshRunReferences(ctx, tx, project, value); err != nil {
			return result, err
		}
	} else {
		current := CronRun(prior)
		replayed, err := continueCronRun(&value, current, in, requestedActor, effectiveActor)
		if err != nil || replayed {
			result.Run, result.Replayed = current, replayed
			return result, err
		}
	}
	event, err := a.insertCronRunEvent(ctx, tx, project, value, effectiveActor)
	if err != nil {
		return result, err
	}
	if err := writeCronRunRow(ctx, tx, value, fresh, in.ExpectedRevision, now); err != nil {
		return result, err
	}
	saved, err := scanCronRunRow(tx.QueryRowContext(ctx, "SELECT "+cronRunColumns+" FROM cron_runs WHERE uid=$1", in.UID))
	if err != nil {
		return result, err
	}
	result.Run = CronRun(saved)
	result.Events = []Event{event}
	return result, nil
}

// continueCronRun prepares value as the next revision of current. It reports
// an exact replay, and refuses an identity change or a stale revision.
func continueCronRun(value *CronRun, current CronRun, in ObserveCronRun, requestedActor, effectiveActor string) (bool, error) {
	value.ID = current.ID
	value.CreatedAt = current.CreatedAt
	// The run keeps the actor that started it. A push spoke substitutes its
	// bound actor for every write, so the starter may continue the run under
	// either its own name or that substitution.
	if current.Actor == requestedActor || current.Actor == effectiveActor {
		value.Actor = current.Actor
	}
	if current.ProjectID != in.ProjectID || !sameCronRunIdentity(current, *value) {
		return false, ErrCronConflict
	}
	if sameCronRunEvidence(current, *value) {
		return true, nil
	}
	if in.ExpectedRevision != current.Revision {
		return false, ErrCronConflict
	}
	value.Revision = current.Revision + 1
	return false, nil
}

// checkFreshRunReferences requires a new run's job, workflow and issue
// references to belong to its project.
func (a CronSQL) checkFreshRunReferences(ctx context.Context, tx *sql.Tx, project Project, value CronRun) error {
	validator := NewCronReplayValidator(tx, a.Postgres)
	refs, err := cronRunReferences(project.ID, value)
	if err != nil {
		return classifyCronReferenceError(err, cron.ErrInvalid)
	}
	if err := validator.prepareReferences(ctx, refs); err != nil {
		return classifyCronReferenceError(err, cron.ErrInvalid)
	}
	for _, ref := range []struct {
		kind      string
		id, event *string
	}{{"job", value.JobUID, value.DefinitionEventUID}, {"workflow", value.WorkflowUID, value.WorkflowDefinitionEventUID}} {
		if ref.id != nil {
			if err := validator.validateLocalReference(ctx, project, ref.kind, *ref.id, *ref.event); err != nil {
				return err
			}
		}
	}
	if value.IssueUID == nil {
		return nil
	}
	var owner int64
	err = tx.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE uid=$1`, *value.IssueUID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != project.ID) {
		return fmt.Errorf("%w: issue reference does not belong to project", cron.ErrInvalid)
	}
	return err
}

func (a CronSQL) insertCronRunEvent(ctx context.Context, tx *sql.Tx, project Project, value CronRun, actor string) (Event, error) {
	payload, err := json.Marshal(NewCronRunObservation(value, project.UID))
	if err != nil {
		return Event{}, err
	}
	if len(payload) > CronObservationLimit {
		return Event{}, fmt.Errorf("%w: observation exceeds byte limit", cron.ErrInvalid)
	}
	return a.InsertEvent(ctx, tx, CronEvent{ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name, Type: "cron.run.observed", Actor: actor, Payload: string(payload)})
}

func writeCronRunRow(ctx context.Context, tx *sql.Tx, value CronRun, fresh bool, expectedRevision int64, now time.Time) error {
	if fresh {
		return insertCronRun(ctx, tx, value, false, false)
	}
	summary, err := json.Marshal(value.Summary)
	if err != nil {
		return err
	}
	changed, err := tx.ExecContext(ctx, `UPDATE cron_runs SET status=$1,summary_json=$2,started_at=$3,ended_at=$4,updated_at=$5,revision=$6 WHERE uid=$7 AND project_id=$8 AND revision=$9`, value.Status, string(summary), cronTimeValue(value.StartedAt), cronTimeValue(value.EndedAt), now.Format(time.RFC3339Nano), value.Revision, value.UID, value.ProjectID, expectedRevision)
	if err != nil {
		return err
	}
	return requireOneCronRow(changed)
}
func sameCronRunEvidence(a, b CronRun) bool {
	return a.Status == b.Status && reflect.DeepEqual(a.Summary, b.Summary) && sameOptionalTime(a.StartedAt, b.StartedAt) && sameOptionalTime(a.EndedAt, b.EndedAt)
}
func sameOptionalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// References point to a historical definition event, not its current revision
// or enabled state. Baselines can preserve that original UID without its edit.
func (v *CronReplayValidator) validateLocalReference(ctx context.Context, project Project, kind, id, event string) error {
	ref := cronDefinitionReference{projectID: project.ID, kind: kind, uid: id, event: event}
	if err := v.load(ctx, ref); err != nil {
		return classifyCronReferenceError(err, cron.ErrInvalid)
	}
	if actual, known := v.idx.events[event]; known && actual == ref {
		return nil
	}
	return fmt.Errorf("%w: definition event reference does not belong to project", cron.ErrInvalid)
}
