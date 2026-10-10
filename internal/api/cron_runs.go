package api

import (
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// CronRun is flat, bounded, independently attributed public evidence.
type CronRun db.CronRun

// CronRunFrom maps a persisted observation to its public representation.
func CronRunFrom(value db.CronRun) CronRun { return CronRun(value) }

// CronProjectRequest scopes a cron operation to one project.
type CronProjectRequest struct {
	ProjectID int64 `path:"project_id"`
}

// CronCapabilitiesResponse advertises ordinary read-only feature support.
type CronCapabilitiesResponse struct {
	EventFeatures string `header:"X-Kata-Event-Features"`
	Body          struct {
		ProjectUID    string   `json:"project_uid"`
		EventFeatures []string `json:"event_features"`
	}
}

// ObserveCronRunBody replaces evidence for a caller-retained run UID.
type ObserveCronRunBody struct {
	Actor                      string       `json:"actor,omitempty"`
	JobUID                     *string      `json:"job_uid,omitempty"`
	DefinitionEventUID         *string      `json:"definition_event_uid,omitempty"`
	WorkflowUID                *string      `json:"workflow_uid,omitempty"`
	WorkflowDefinitionEventUID *string      `json:"workflow_definition_event_uid,omitempty"`
	OccurrenceKey              *string      `json:"occurrence_key,omitempty"`
	IssueUID                   *string      `json:"issue_uid,omitempty"`
	Teammate                   *string      `json:"teammate,omitempty" nullable:"true"`
	ExecutorLabel              *string      `json:"executor_label,omitempty"`
	Status                     string       `json:"status" enum:"running,succeeded,failed,cancelled,unknown"`
	Summary                    cron.Summary `json:"summary"`
	StartedAt                  *time.Time   `json:"started_at,omitempty"`
	EndedAt                    *time.Time   `json:"ended_at,omitempty"`
	ExpectedRevision           int64        `json:"expected_revision" minimum:"0"`
}

// Native applies the admitted actor and project to the portable input.
func (b ObserveCronRunBody) Native(projectID int64, runUID, actor string) db.ObserveCronRun {
	return db.ObserveCronRun{ProjectID: projectID, UID: runUID, Actor: actor, JobUID: b.JobUID, DefinitionEventUID: b.DefinitionEventUID, WorkflowUID: b.WorkflowUID, WorkflowDefinitionEventUID: b.WorkflowDefinitionEventUID, OccurrenceKey: b.OccurrenceKey, IssueUID: b.IssueUID, Teammate: b.Teammate, ExecutorLabel: b.ExecutorLabel, Status: b.Status, Summary: b.Summary, StartedAt: b.StartedAt, EndedAt: b.EndedAt, ExpectedRevision: b.ExpectedRevision}
}

// CronRunRequest identifies one independent run within its project.
type CronRunRequest struct {
	CronProjectRequest
	RunUID string `path:"run_uid"`
}

// ObserveCronRunRequest pairs immutable run identity with reported evidence.
type ObserveCronRunRequest struct {
	CronRunRequest
	Body ObserveCronRunBody
}

// CronRunResponse returns one flat observation without execution authority.
type CronRunResponse struct {
	Body struct {
		Run CronRun `json:"run"`
	}
}

// ObserveCronRunResponse returns evidence and its exact committed events.
type ObserveCronRunResponse struct {
	Body struct {
		Run      CronRun    `json:"run"`
		Events   []db.Event `json:"events"`
		Replayed bool       `json:"replayed"`
	}
}

// CronRunsRequest selects a bounded project-scoped history page.
type CronRunsRequest struct {
	CronProjectRequest
	JobUID    string `query:"job_uid"`
	Limit     int    `query:"limit" minimum:"0" maximum:"100"`
	BeforeUID string `query:"before_uid"`
}

// CronRunsResponse returns evidence and an optional ordinary page cursor.
type CronRunsResponse struct {
	Body struct {
		Runs          []CronRun `json:"runs"`
		NextBeforeUID string    `json:"next_before_uid,omitempty"`
	}
}
