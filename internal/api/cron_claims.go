package api

import "go.kenn.io/kata/internal/db"

// CronClaimOrigin is attribution asserted only by a currently enrolled relay.
type CronClaimOrigin struct {
	HolderInstanceUID string   `json:"holder_instance_uid"`
	Actor             string   `json:"actor"`
	Path              []string `json:"path" maxItems:"16"`
}

// CronClaimBody is an immutable, caller-retained attempt identity.
type CronClaimBody struct {
	ExpectedAuthorityInstanceUID string           `json:"expected_authority_instance_uid,omitempty"`
	RunUID                       string           `json:"run_uid"`
	DefinitionEventUID           string           `json:"definition_event_uid"`
	OccurrenceKey                string           `json:"occurrence_key" maxLength:"1024"`
	Actor                        string           `json:"actor,omitempty"`
	Teammate                     *string          `json:"teammate,omitempty"`
	ExecutorLabel                *string          `json:"executor_label,omitempty"`
	WorkflowUID                  *string          `json:"workflow_uid,omitempty"`
	WorkflowDefinitionEventUID   *string          `json:"workflow_definition_event_uid,omitempty"`
	Origin                       *CronClaimOrigin `json:"origin,omitempty"`
}

// ClaimCronOccurrenceRequest identifies a job and its attempt.
type ClaimCronOccurrenceRequest struct {
	CronProjectRequest
	Authorization string `header:"Authorization"`
	Hops          int    `header:"X-Kata-Cron-Hops" minimum:"0" maximum:"16"`
	JobUID        string `path:"job_uid"`
	Body          CronClaimBody
}

// CronClaimRequest identifies a retained reservation by its run UID.
type CronClaimRequest struct {
	CronProjectRequest
	Authorization string `header:"Authorization"`
	Hops          int    `header:"X-Kata-Cron-Hops" minimum:"0" maximum:"16"`
	RunUID        string `path:"run_uid"`
}

// CronClaimResponseBody confirms a reservation, never permission to launch twice.
type CronClaimResponseBody struct {
	Claimed  bool                   `json:"claimed"`
	Replayed bool                   `json:"replayed"`
	Claim    db.CronOccurrenceClaim `json:"claim"`
}

// CronClaimResponse distinguishes the first grant (201) from replay/lookup (200).
type CronClaimResponse struct {
	Status int
	Body   CronClaimResponseBody
}
