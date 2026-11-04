package db

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/kata/internal/cron"
)

// ErrCronConflict reports a stale revision or different immutable identity.
var ErrCronConflict = errors.New("cron revision or identity conflict")

// CronDefinitionHLC preserves the winning document clock through
// snapshots; generating a baseline must never advance this clock.
type CronDefinitionHLC = cron.HLC

// CronDefinition stores a project-scoped document identity and winning clock.
type CronDefinition struct {
	ID                 int64             `json:"id"`
	UID                string            `json:"uid"`
	ProjectID          int64             `json:"project_id"`
	Name               string            `json:"name"`
	DefinitionEventUID string            `json:"definition_event_uid"`
	DefinitionHLC      CronDefinitionHLC `json:"definition_hlc"`
	Author             string            `json:"author"`
	Revision           int64             `json:"revision"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
	DeletedAt          *time.Time        `json:"deleted_at,omitempty"`
}

// CronJob combines durable document provenance with a portable job.
type CronJob struct {
	CronDefinition
	Definition cron.JobDefinition `json:"definition"`
}

// CronWorkflow combines durable document provenance with a portable workflow.
type CronWorkflow struct {
	CronDefinition
	Definition cron.WorkflowDefinition `json:"definition"`
}

// PutCronJob replaces a complete definition. Empty UID allocates an
// identity; every edit, tombstone, or restore requires the winning event UID.
type PutCronJob struct {
	UID              string
	ProjectID        int64
	Name             string
	Definition       cron.JobDefinition
	ExpectedEventUID string
	Actor            string
	Deleted          bool
}

// PutCronWorkflow replaces a complete workflow using its expected winning event.
type PutCronWorkflow struct {
	UID              string
	ProjectID        int64
	Name             string
	Definition       cron.WorkflowDefinition
	ExpectedEventUID string
	Actor            string
	Deleted          bool
}

// CronList scopes definition reads to one project and tombstone policy.
type CronList struct {
	ProjectID      int64
	IncludeDeleted bool
}

// CronDefinitions is the dormant shared configuration storage API.
type CronDefinitions interface {
	PutCronJob(context.Context, PutCronJob) (CronJob, []Event, error)
	PutCronWorkflow(context.Context, PutCronWorkflow) (CronWorkflow, Event, error)
	CronJob(context.Context, int64, string) (CronJob, error)
	CronWorkflow(context.Context, int64, string) (CronWorkflow, error)
	ListCronJobs(context.Context, CronList) ([]CronJob, error)
	ListCronWorkflows(context.Context, CronList) ([]CronWorkflow, error)
}
