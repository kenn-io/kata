package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/uid"
)

// CronSQL shares the portable definition transaction across backends.
// Each backend supplies its own retry/isolation, authorization fence, and event
// writer. SQL uses numbered placeholders supported by SQLite and PostgreSQL.
type CronSQL struct {
	InstanceUID string
	Postgres    bool
	Query       interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}
	Transact       func(context.Context, func(*sql.Tx) error) error
	WriteGate      func(context.Context, *sql.Tx, int64) error
	EffectiveActor func(context.Context, *sql.Tx, int64, string) (string, error)
	InsertEvent    func(context.Context, *sql.Tx, CronEvent) (Event, error)
}

func (a CronSQL) effectiveMutationActor(ctx context.Context, tx *sql.Tx, projectID int64, requested string) (string, error) {
	if a.EffectiveActor == nil {
		return requested, nil
	}
	actor, err := a.EffectiveActor(ctx, tx, projectID, requested)
	if err != nil {
		return "", err
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "", fmt.Errorf("%w: actor required", cron.ErrInvalid)
	}
	return actor, nil
}

// CronEvent carries the attributed event returned by a definition transaction.
type CronEvent struct {
	IssueID                                       *int64
	IssueUID                                      *string
	ProjectID                                     int64
	ProjectUID, ProjectName, Type, Actor, Payload string
}

const cronDefinitionColumns = `id,uid,project_id,name,definition_json,definition_event_uid,definition_hlc_json,author,revision,created_at,updated_at,deleted_at`

type cronRow struct {
	CronDefinition
	document []byte
}

func scanCron(row interface{ Scan(...any) error }) (cronRow, error) {
	var result cronRow
	var definition, clock, created, updated string
	var deleted sql.NullString
	fields := []any{&result.ID, &result.UID, &result.ProjectID, &result.Name, &definition, &result.DefinitionEventUID, &clock, &result.Author, &result.Revision, &created, &updated, &deleted}
	if err := row.Scan(fields...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, ErrNotFound
		}
		return result, err
	}
	if err := json.Unmarshal([]byte(clock), &result.DefinitionHLC); err != nil {
		return result, err
	}
	var err error
	result.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return result, err
	}
	result.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return result, err
	}
	if deleted.Valid {
		value, err := time.Parse(time.RFC3339Nano, deleted.String)
		if err != nil {
			return result, err
		}
		result.DeletedAt = &value
	}
	result.document = []byte(definition)
	return result, nil
}

func cronTable(job bool) string {
	if job {
		return "cron_jobs"
	}
	return "cron_workflows"
}
func jobFromRow(row cronRow) (CronJob, error) {
	result := CronJob{CronDefinition: row.CronDefinition}
	var err error
	result.Definition, err = cron.DecodeJob(row.document)
	if err != nil {
		return result, err
	}
	return result, err
}
func workflowFromRow(row cronRow) (CronWorkflow, error) {
	definition, err := cron.DecodeWorkflow(row.document)
	return CronWorkflow{CronDefinition: row.CronDefinition, Definition: definition}, err
}

// Job reads one project-scoped definition, including a retained tombstone.
func (a CronSQL) Job(ctx context.Context, project int64, id string) (CronJob, error) {
	row, err := scanCron(a.Query.QueryRowContext(ctx, "SELECT "+cronDefinitionColumns+" FROM cron_jobs WHERE project_id=$1 AND uid=$2", project, strings.ToUpper(id)))
	if err != nil {
		return CronJob{}, err
	}
	return jobFromRow(row)
}

// Workflow reads one project-scoped definition, including a retained tombstone.
func (a CronSQL) Workflow(ctx context.Context, project int64, id string) (CronWorkflow, error) {
	row, err := scanCron(a.Query.QueryRowContext(ctx, "SELECT "+cronDefinitionColumns+" FROM cron_workflows WHERE project_id=$1 AND uid=$2", project, strings.ToUpper(id)))
	if err != nil {
		return CronWorkflow{}, err
	}
	return workflowFromRow(row)
}

func (a CronSQL) list(ctx context.Context, in CronList, job bool) ([]cronRow, error) {
	query := "SELECT " + cronDefinitionColumns + " FROM " + cronTable(job) + " WHERE project_id=$1"
	if !in.IncludeDeleted {
		query += " AND deleted_at IS NULL"
	}
	query += " ORDER BY name,uid"
	rows, err := a.Query.QueryContext(ctx, query, in.ProjectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []cronRow{}
	for rows.Next() {
		row, err := scanCron(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// Jobs lists project-scoped job documents under the requested tombstone policy.
func (a CronSQL) Jobs(ctx context.Context, in CronList) ([]CronJob, error) {
	rows, err := a.list(ctx, in, true)
	if err != nil {
		return nil, err
	}
	result := []CronJob{}
	for _, row := range rows {
		value, err := jobFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}

// Workflows lists project-scoped workflow documents under the requested tombstone policy.
func (a CronSQL) Workflows(ctx context.Context, in CronList) ([]CronWorkflow, error) {
	rows, err := a.list(ctx, in, false)
	if err != nil {
		return nil, err
	}
	result := []CronWorkflow{}
	for _, row := range rows {
		value, err := workflowFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}

// validateLiveDefinition applies the current rules to a definition that will
// be live after the write. A tombstone keeps whatever document it had, so an
// older row that today's rules reject can still be deleted.
func validateLiveDefinition(deleted bool, validate func() error) error {
	if deleted {
		return nil
	}
	return validate()
}

// PutJob validates and replaces a job using the expected whole-document revision.
func (a CronSQL) PutJob(ctx context.Context, in PutCronJob) (CronJob, []Event, error) {
	if err := validateLiveDefinition(in.Deleted, in.Definition.Validate); err != nil {
		return CronJob{}, nil, err
	}
	row, event, err := a.put(ctx, cronPutRequest{job: true, id: in.UID, projectID: in.ProjectID, name: in.Name, expected: in.ExpectedEventUID, actor: in.Actor, deleted: in.Deleted, definition: in.Definition})
	if err != nil {
		return CronJob{}, nil, err
	}
	value, err := jobFromRow(row)
	return value, event, err
}

// PutWorkflow validates and replaces a workflow using the expected whole-document revision.
func (a CronSQL) PutWorkflow(ctx context.Context, in PutCronWorkflow) (CronWorkflow, Event, error) {
	if err := validateLiveDefinition(in.Deleted, in.Definition.Validate); err != nil {
		return CronWorkflow{}, Event{}, err
	}
	row, events, err := a.put(ctx, cronPutRequest{id: in.UID, projectID: in.ProjectID, name: in.Name, expected: in.ExpectedEventUID, actor: in.Actor, deleted: in.Deleted, definition: in.Definition})
	if err != nil {
		return CronWorkflow{}, Event{}, err
	}
	value, err := workflowFromRow(row)
	if err != nil {
		return CronWorkflow{}, Event{}, err
	}
	if len(events) != 1 {
		return CronWorkflow{}, Event{}, fmt.Errorf("definition write produced %d events", len(events))
	}
	return value, events[0], nil
}

// cronPutRequest is one whole-document definition write.
type cronPutRequest struct {
	job        bool
	id         string
	projectID  int64
	name       string
	expected   string
	actor      string
	deleted    bool
	definition any
	document   []byte
}

func (a CronSQL) put(ctx context.Context, req cronPutRequest) (cronRow, []Event, error) {
	req.id = strings.ToUpper(req.id)
	req.name = strings.TrimSpace(req.name)
	req.actor = strings.TrimSpace(req.actor)
	if req.name == "" || len(req.name) > 256 || req.actor == "" {
		return cronRow{}, nil, fmt.Errorf("%w: name and actor required", cron.ErrInvalid)
	}
	if req.id != "" && !uid.Valid(req.id) {
		return cronRow{}, nil, fmt.Errorf("%w: invalid UID", cron.ErrInvalid)
	}
	var err error
	if req.document, err = json.Marshal(req.definition); err != nil {
		return cronRow{}, nil, err
	}
	if req.id == "" {
		if req.id, err = uid.New(); err != nil {
			return cronRow{}, nil, err
		}
	}
	var result cronRow
	var event Event
	err = a.transactProject(ctx, req.projectID, func(tx *sql.Tx) error {
		var err error
		result, event, err = a.putTx(ctx, tx, req)
		return err
	})
	if err != nil {
		return cronRow{}, nil, err
	}
	return result, []Event{event}, nil
}

func (a CronSQL) putTx(ctx context.Context, tx *sql.Tx, req cronPutRequest) (cronRow, Event, error) {
	project, err := a.openCronProject(ctx, tx, req.projectID)
	if err != nil {
		return cronRow{}, Event{}, err
	}
	effectiveActor, err := a.effectiveMutationActor(ctx, tx, req.projectID, req.actor)
	if err != nil {
		return cronRow{}, Event{}, err
	}
	current, readErr := scanCron(tx.QueryRowContext(ctx, "SELECT "+cronDefinitionColumns+" FROM "+cronTable(req.job)+" WHERE uid=$1", req.id))
	fresh := errors.Is(readErr, ErrNotFound)
	if readErr != nil && !fresh {
		return cronRow{}, Event{}, readErr
	}
	if err := checkCronPutState(req, current, fresh); err != nil {
		return cronRow{}, Event{}, err
	}
	if err := a.checkCronPutDependencies(ctx, tx, req, current); err != nil {
		return cronRow{}, Event{}, err
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	next := CronDefinition{UID: req.id, ProjectID: req.projectID, Name: req.name, Author: effectiveActor, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if !fresh {
		next.CreatedAt, next.Author, next.Revision = current.CreatedAt, current.Author, current.Revision+1
	}
	if req.deleted {
		next.DeletedAt = &now
	}
	event, err := a.insertCronDefinitionEvent(ctx, tx, project, req, next, cronPutOperation(req, current, fresh), effectiveActor)
	if err != nil {
		return cronRow{}, Event{}, err
	}
	if err := writeCronDefinitionRow(ctx, tx, req, next, event, fresh); err != nil {
		return cronRow{}, Event{}, err
	}
	row, err := scanCron(tx.QueryRowContext(ctx, "SELECT "+cronDefinitionColumns+" FROM "+cronTable(req.job)+" WHERE uid=$1", req.id))
	return row, event, err
}

// openCronProject serializes cron writes for a live, writable project and
// returns its portable identity.
func (a CronSQL) openCronProject(ctx context.Context, tx *sql.Tx, projectID int64) (Project, error) {
	if err := LockCronProject(ctx, tx, projectID); err != nil {
		return Project{}, err
	}
	project := Project{ID: projectID}
	err := tx.QueryRowContext(ctx, `SELECT uid,name FROM projects WHERE id=$1 AND deleted_at IS NULL AND name<>$2`, projectID, SystemProjectName).Scan(&project.UID, &project.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, err
	}
	if err := a.WriteGate(ctx, tx, projectID); err != nil {
		return Project{}, err
	}
	return project, nil
}

// checkCronPutState enforces whole-document compare-and-swap: a create names
// no expected event, and every other write names the current winning event.
func checkCronPutState(req cronPutRequest, current cronRow, fresh bool) error {
	if fresh && (req.expected != "" || req.deleted) {
		return ErrCronConflict
	}
	if !fresh && (current.ProjectID != req.projectID || req.expected == "" || req.expected != current.DefinitionEventUID) {
		return ErrCronConflict
	}
	return nil
}

func (a CronSQL) checkCronPutDependencies(ctx context.Context, tx *sql.Tx, req cronPutRequest, current cronRow) error {
	if req.job && !req.deleted {
		return CheckCronDependencies(ctx, tx, req.projectID, req.definition.(cron.JobDefinition))
	}
	if !req.job && req.deleted && current.DeletedAt == nil {
		return checkWorkflowUnused(ctx, tx, a.Postgres, req.projectID, req.id)
	}
	return nil
}

func cronPutOperation(req cronPutRequest, current cronRow, fresh bool) string {
	switch {
	case fresh:
		return "created"
	case req.deleted:
		return "deleted"
	case current.DeletedAt != nil:
		return "restored"
	default:
		return "updated"
	}
}

func (a CronSQL) insertCronDefinitionEvent(ctx context.Context, tx *sql.Tx, project Project, req cronPutRequest, next CronDefinition, operation, actor string) (Event, error) {
	payload, err := json.Marshal(struct {
		UID        string     `json:"uid"`
		ProjectUID string     `json:"project_uid"`
		Name       string     `json:"name"`
		Definition any        `json:"definition"`
		Author     string     `json:"author"`
		CreatedAt  time.Time  `json:"created_at"`
		UpdatedAt  time.Time  `json:"updated_at"`
		DeletedAt  *time.Time `json:"deleted_at,omitempty"`
	}{next.UID, project.UID, next.Name, req.definition, next.Author, next.CreatedAt, next.UpdatedAt, next.DeletedAt})
	if err != nil {
		return Event{}, err
	}
	kind := "workflow"
	if req.job {
		kind = "job"
	}
	return a.InsertEvent(ctx, tx, CronEvent{ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name, Type: "cron." + kind + "." + operation, Actor: actor, Payload: string(payload)})
}

func writeCronDefinitionRow(ctx context.Context, tx *sql.Tx, req cronPutRequest, next CronDefinition, event Event, fresh bool) error {
	clock, err := json.Marshal(CronDefinitionHLC{Version: 1, PhysicalMS: event.HLCPhysicalMS, Counter: event.HLCCounter, OriginInstanceUID: event.OriginInstanceUID})
	if err != nil {
		return err
	}
	updatedAt := next.UpdatedAt.Format(time.RFC3339Nano)
	var deletedValue any
	if next.DeletedAt != nil {
		deletedValue = updatedAt
	}
	var written sql.Result
	if fresh {
		// A concurrent writer in another project may claim this UID after the
		// existence check; DO NOTHING turns that into a conflict.
		//nolint:gosec // Boolean selector returns one of two fixed table names; values are bound.
		written, err = tx.ExecContext(ctx, "INSERT INTO "+cronTable(req.job)+`(uid,project_id,name,definition_json,definition_event_uid,definition_hlc_json,author,revision,created_at,updated_at,deleted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(uid) DO NOTHING`, next.UID, next.ProjectID, next.Name, string(req.document), event.UID, string(clock), next.Author, next.Revision, next.CreatedAt.Format(time.RFC3339Nano), updatedAt, deletedValue)
	} else {
		//nolint:gosec // Boolean selector returns one of two fixed tables; values are bound.
		written, err = tx.ExecContext(ctx, "UPDATE "+cronTable(req.job)+` SET name=$1,definition_json=$2,definition_event_uid=$3,definition_hlc_json=$4,revision=$5,updated_at=$6,deleted_at=$7 WHERE uid=$8 AND project_id=$9 AND definition_event_uid=$10`, next.Name, string(req.document), event.UID, string(clock), next.Revision, updatedAt, deletedValue, next.UID, next.ProjectID, req.expected)
	}
	if err != nil {
		return err
	}
	return requireOneCronRow(written)
}
