package db

import (
	"errors"
	"time"
)

var (
	ErrCronOccurrenceClaimed = errors.New("cron occurrence is already claimed")
	ErrCronClaimIdentity     = errors.New("cron claim run identity conflicts")
	ErrCronClaimPending      = errors.New("cron claim authority or definition is unavailable")
)

// CronClaimAuthorityPrefix is internal owner metadata, never replicated project metadata.
const CronClaimAuthorityPrefix = "cron_claim_authority."

// CronOccurrenceClaim is a non-expiring root-local reservation, not run evidence.
// Independent execution attempts require distinct run UIDs, persisted before claiming.
type CronOccurrenceClaim struct {
	ProjectID                  int64     `json:"project_id"`
	JobUID                     string    `json:"job_uid"`
	OccurrenceKey              string    `json:"occurrence_key"`
	RunUID                     string    `json:"run_uid"`
	HolderInstanceUID          string    `json:"holder_instance_uid"`
	Actor                      string    `json:"actor"`
	Teammate                   *string   `json:"teammate,omitempty"`
	ExecutorLabel              *string   `json:"executor_label,omitempty"`
	DefinitionEventUID         string    `json:"definition_event_uid"`
	WorkflowUID                *string   `json:"workflow_uid,omitempty"`
	WorkflowDefinitionEventUID *string   `json:"workflow_definition_event_uid,omitempty"`
	AuthorityInstanceUID       string    `json:"authority_instance_uid"`
	CreatedAt                  time.Time `json:"created_at"`
}

// CronClaimResult confirms an existing reservation on replay, never a new launch.
type CronClaimResult struct {
	Claim    CronOccurrenceClaim `json:"claim"`
	Replayed bool                `json:"replayed"`
}
