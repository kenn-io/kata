// Package records holds the native daemon evidence consumed by the ported UI.
package records

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"time"

	native "go.kenn.io/kata/internal/cron"
)

// Job retains the exact definition bytes and its optimistic concurrency guard.
// ProjectUID is the copied UI's project key: the decimal native project ID.
// The host keeps actual project UIDs separately for event matching.
type Job struct {
	ProjectID          int64          `json:"project_id"`
	ProjectUID         string         `json:"-"`
	Project            string         `json:"-"`
	UID                string         `json:"uid"`
	Name               string         `json:"name"`
	DefinitionEventUID string         `json:"definition_event_uid"`
	Definition         jsontext.Value `json:"definition"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	DeletedAt          *time.Time     `json:"deleted_at,omitempty"`
	LastRun            *Run           `json:"last_run"`
	// LastRunKnown records whether the daemon supplied authoritative last_run evidence.
	// Explicit null remains authoritative never-ran.
	LastRunKnown bool `json:"-"`
}

// UnmarshalJSON preserves the distinction between an omitted legacy last_run
// and the authoritative nullable field returned by newer daemons.
func (j *Job) UnmarshalJSON(data []byte) error {
	type wireJob Job
	decoded := wireJob(*j)
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var presence struct {
		LastRun jsontext.Value `json:"last_run"`
	}
	if err := json.Unmarshal(data, &presence); err != nil {
		return err
	}
	*j = Job(decoded)
	if len(presence.LastRun) != 0 {
		j.LastRunKnown = true
	}
	return nil
}

// Workflow uses the same identity and revision envelope as a job.
type Workflow = Job

// Run contains native recorded evidence, without inferred execution state.
type Run struct {
	ProjectID                  int64          `json:"project_id"`
	ProjectKey                 string         `json:"-"`
	ProjectName                string         `json:"-"`
	UID                        string         `json:"uid"`
	JobUID                     *string        `json:"job_uid,omitempty"`
	DefinitionEventUID         *string        `json:"definition_event_uid,omitempty"`
	WorkflowUID                *string        `json:"workflow_uid,omitempty"`
	WorkflowDefinitionEventUID *string        `json:"workflow_definition_event_uid,omitempty"`
	OccurrenceKey              *string        `json:"occurrence_key,omitempty"`
	IssueUID                   *string        `json:"issue_uid,omitempty"`
	ExecutorLabel              *string        `json:"executor_label,omitempty"`
	Actor                      string         `json:"actor"`
	Teammate                   *string        `json:"teammate,omitempty"`
	Status                     string         `json:"status"`
	Summary                    native.Summary `json:"summary"`
	StartedAt                  *time.Time     `json:"started_at,omitempty"`
	EndedAt                    *time.Time     `json:"ended_at,omitempty"`
	CreatedAt                  time.Time      `json:"created_at"`
	UpdatedAt                  time.Time      `json:"updated_at"`
	Revision                   int64          `json:"revision"`
}

// RunPage retains one native history page and its cursor.
type RunPage struct {
	Runs          []Run  `json:"runs"`
	NextBeforeUID string `json:"next_before_uid,omitempty"`
}

// IssueView contains the resolved identity used by definition forms.
type IssueView struct{ UID, ShortID, ProjectUID, Title, Status string }

// Value returns an optional recorded string or its empty placeholder.
func Value(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
