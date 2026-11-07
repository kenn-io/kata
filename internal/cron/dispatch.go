package cron

// DispatchReason describes a native recurring dispatch decision. It is separate
// from adapter run reasons and does not claim that an adapter executed a job.
type DispatchReason string

// Native recurring dispatch reasons describe eligibility and committed outcomes.
const (
	DispatchDue                DispatchReason = "due"
	DispatchDelivered          DispatchReason = "delivered"
	DispatchIdle               DispatchReason = "idle"
	DispatchSkipped            DispatchReason = "skipped"
	DispatchScanning           DispatchReason = "scanning"
	DispatchExternalAdapter    DispatchReason = "external_adapter"
	DispatchDisabled           DispatchReason = "disabled"
	DispatchDeleted            DispatchReason = "deleted"
	DispatchReplica            DispatchReason = "replica"
	DispatchBindingDisabled    DispatchReason = "binding_disabled"
	DispatchProjectUnavailable DispatchReason = "project_unavailable"
	DispatchDefinitionChanged  DispatchReason = "definition_changed"
	DispatchJobMissing         DispatchReason = "job_missing"
	DispatchParentMissing      DispatchReason = "parent_missing"
	DispatchParentDeleted      DispatchReason = "parent_deleted"
	DispatchParentClosed       DispatchReason = "parent_closed"
	DispatchProgressInvalid    DispatchReason = "progress_invalid"
	DispatchScheduleInvalid    DispatchReason = "schedule_invalid"
	DispatchRecipientInvalid   DispatchReason = "recipient_invalid"
	DispatchError              DispatchReason = "error"
)

// PublicDetail is safe to return to API clients. Raw decoder, database and
// provider errors are local diagnostics and must never be reflected there.
func (r DispatchReason) PublicDetail() string {
	switch r {
	case DispatchProgressInvalid:
		return "Recurring progress is invalid; delivery is paused."
	case DispatchScheduleInvalid:
		return "The recurring definition or schedule is invalid."
	case DispatchRecipientInvalid:
		return "The resolved notification recipient is invalid."
	case DispatchError:
		return "Dispatch failed; inspect the daemon's local cron log."
	default:
		return ""
	}
}
