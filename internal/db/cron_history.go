package db

import (
	"time"

	"go.kenn.io/kata/internal/cron"
)

// CronRun is independently attributed evidence. Its UID identifies one
// execution; occurrence keys and issue references are descriptive, not unique.
type CronRun struct {
	ID                         int64        `json:"id"`
	UID                        string       `json:"uid"`
	ProjectID                  int64        `json:"project_id"`
	JobUID                     *string      `json:"job_uid,omitempty"`
	DefinitionEventUID         *string      `json:"definition_event_uid,omitempty"`
	WorkflowUID                *string      `json:"workflow_uid,omitempty"`
	WorkflowDefinitionEventUID *string      `json:"workflow_definition_event_uid,omitempty"`
	OccurrenceKey              *string      `json:"occurrence_key,omitempty"`
	IssueUID                   *string      `json:"issue_uid,omitempty"`
	Actor                      string       `json:"actor"`
	Teammate                   *string      `json:"teammate,omitempty"`
	ExecutorLabel              *string      `json:"executor_label,omitempty"`
	Status                     string       `json:"status"`
	Summary                    cron.Summary `json:"summary"`
	Revision                   int64        `json:"revision"`
	CreatedAt                  time.Time    `json:"created_at"`
	StartedAt                  *time.Time   `json:"started_at,omitempty"`
	EndedAt                    *time.Time   `json:"ended_at,omitempty"`
	UpdatedAt                  time.Time    `json:"updated_at"`
}

// ObserveCronRun replaces the mutable evidence of one immutable identity.
// ExpectedRevision is zero for creation; exact retries return the current row.
type ObserveCronRun struct {
	ProjectID                  int64
	UID                        string
	JobUID                     *string
	DefinitionEventUID         *string
	WorkflowUID                *string
	WorkflowDefinitionEventUID *string
	OccurrenceKey              *string
	IssueUID                   *string
	Actor                      string
	Teammate                   *string
	ExecutorLabel              *string
	Status                     string
	Summary                    cron.Summary
	StartedAt                  *time.Time
	EndedAt                    *time.Time
	ExpectedRevision           int64
}

// CronRunObservationResult retains the row and exact events of one write.
type CronRunObservationResult struct {
	Run      CronRun `json:"run"`
	Events   []Event `json:"events"`
	Replayed bool    `json:"replayed"`
}

// CronRunList bounds and scopes ordinary run-history pagination.
type CronRunList struct {
	ProjectID int64
	JobUID    string
	Limit     int
	BeforeUID string
}

// CronJobExport is the portable backup representation of a job row.
type CronJobExport CronJob

// CronWorkflowExport is the portable backup representation of a workflow row.
type CronWorkflowExport CronWorkflow

// CronRunExport is the portable backup representation of a run observation.
type CronRunExport CronRun

// ImportKind identifies a portable shared job record.
func (*CronJobExport) ImportKind() string { return ImportKindCronJob }

// ImportKind identifies a portable shared workflow record.
func (*CronWorkflowExport) ImportKind() string { return ImportKindCronWorkflow }

// ImportKind identifies a portable independent observation record.
func (*CronRunExport) ImportKind() string { return ImportKindCronRun }
