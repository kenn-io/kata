package db

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"time"

	"go.kenn.io/kata/internal/uid"
)

// IssueStatusState is private to an import identity. Desired status and order
// are obtained from the referenced event, never copied into this checkpoint.
type IssueStatusState struct {
	Observed        *IssueStatusObservation
	PendingEventUID string
	RemoteLocator   string
}

// IssueStatusObservation distinguishes no observation from an observed null
// Notion status. Version is the provider's timestamp, independent of content.
type IssueStatusObservation struct {
	Raw     *string   `json:"raw"`
	Version time.Time `json:"version"`
}

func invalidIssueStatusState() error {
	return fmt.Errorf("%w: invalid private issue status checkpoint", ErrImportValidation)
}

// DecodeIssueStatusState reads the legacy schema-30 JSON checkpoint. Opaque
// identities and provider content are not included in diagnostic errors.
func DecodeIssueStatusState(raw jsontext.Value) (IssueStatusState, error) {
	var state struct {
		Observed          *IssueStatusObservation `json:"observed,omitzero"`
		PendingEventUID   string                  `json:"pending_event_uid,omitzero"`
		GitHubIssueNumber int64                   `json:"github_issue_number,omitzero"`
	}
	if len(raw) == 0 {
		return IssueStatusState{}, nil
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return IssueStatusState{}, invalidIssueStatusState()
	}
	if err := json.Unmarshal(raw, &state, json.RejectUnknownMembers(true)); err != nil {
		return IssueStatusState{}, invalidIssueStatusState()
	}
	if observation, present := fields["observed"]; present {
		var observed map[string]jsontext.Value
		if err := json.Unmarshal(observation, &observed); err != nil || observed == nil || state.Observed == nil {
			return IssueStatusState{}, invalidIssueStatusState()
		}
		if _, present := observed["raw"]; !present {
			return IssueStatusState{}, invalidIssueStatusState()
		}
		if state.Observed.Version.IsZero() {
			return IssueStatusState{}, invalidIssueStatusState()
		}
		state.Observed.Version = state.Observed.Version.UTC()
	}
	if _, present := fields["pending_event_uid"]; present && !uid.Valid(state.PendingEventUID) {
		return IssueStatusState{}, invalidIssueStatusState()
	}
	if _, present := fields["github_issue_number"]; present && state.GitHubIssueNumber <= 0 {
		return IssueStatusState{}, invalidIssueStatusState()
	}
	result := IssueStatusState{Observed: state.Observed, PendingEventUID: state.PendingEventUID}
	if state.GitHubIssueNumber != 0 {
		result.RemoteLocator = strconv.FormatInt(state.GitHubIssueNumber, 10)
	}
	return result, nil
}

// DecodeIssueStatusColumns validates nullable persisted values. An observation
// exists only when its provider timestamp is present, including a null raw status.
func DecodeIssueStatusColumns(raw, observedAt, pending, locator *string) (IssueStatusState, error) {
	var state IssueStatusState
	if observedAt == nil {
		if raw != nil {
			return state, invalidIssueStatusState()
		}
	} else {
		version, err := time.Parse(time.RFC3339Nano, *observedAt)
		if err != nil || version.IsZero() {
			return state, invalidIssueStatusState()
		}
		state.Observed = &IssueStatusObservation{Raw: raw, Version: version.UTC()}
	}
	if pending != nil {
		if !uid.Valid(*pending) {
			return IssueStatusState{}, invalidIssueStatusState()
		}
		state.PendingEventUID = *pending
	}
	if locator != nil {
		if *locator == "" {
			return IssueStatusState{}, invalidIssueStatusState()
		}
		state.RemoteLocator = *locator
	}
	return state, nil
}

// NormalizeIssueStatusExport converts the former JSON checkpoint when reading
// legacy exports. New exports contain only the four nullable mapping columns.
func NormalizeIssueStatusExport(record ImportMappingExport) (ImportMappingExport, error) {
	if len(record.StatusSync) != 0 {
		if record.ObservedStatus != nil || record.ObservedStatusAt != nil || record.PendingEventUID != nil || record.RemoteLocator != nil {
			return record, invalidIssueStatusState()
		}
		state, err := DecodeIssueStatusState(record.StatusSync)
		if err != nil {
			return record, err
		}
		if state.Observed != nil {
			record.ObservedStatus = state.Observed.Raw
			stamp := state.Observed.Version.Format(time.RFC3339Nano)
			record.ObservedStatusAt = &stamp
		}
		if state.PendingEventUID != "" {
			record.PendingEventUID = &state.PendingEventUID
		}
		if state.RemoteLocator != "" {
			record.RemoteLocator = &state.RemoteLocator
		}
		record.StatusSync = nil
	}
	state, err := DecodeIssueStatusColumns(record.ObservedStatus, record.ObservedStatusAt, record.PendingEventUID, record.RemoteLocator)
	if err != nil {
		return record, err
	}
	if state.Observed != nil {
		stamp := state.Observed.Version.Format(time.RFC3339Nano)
		record.ObservedStatusAt = &stamp
	}
	return record, nil
}

// IssueStatusMapping joins an existing identity to private delivery authority.
// It is an internal worker value, not a public import response.
type IssueStatusMapping struct {
	Mapping      ImportMapping
	State        IssueStatusState
	PendingEvent *Event
}

// IssueStatusQuery bounds one scan lap independently of the content cursor.
type IssueStatusQuery struct {
	Guard       IssueSyncImportGuard
	AfterID     int64
	ThroughID   int64
	Limit       int
	PendingOnly bool
}

// IssueStatusPage contains one bounded mapping page and its lap high-watermark.
type IssueStatusPage struct {
	Mappings    []IssueStatusMapping
	HighWaterID int64
}

// IssueStatusReader uses the existing binding claim for consistent private reads.
type IssueStatusReader interface {
	IssueStatusMappingByID(context.Context, IssueSyncImportGuard, int64) (IssueStatusMapping, error)
	ListIssueStatusMappings(context.Context, IssueStatusQuery) (IssueStatusPage, error)
}

// IssueStatusObservationParams carries a verified status-only read and, for
// delivery, the exact event serviced. Identity is rechecked in the transaction.
type IssueStatusObservationParams struct {
	Guard            IssueSyncImportGuard
	MappingID        int64
	ExternalID       string
	IssueUID         string
	Observation      IssueStatusObservation
	Status           string
	ClosedReason     string
	ClosedAt         *time.Time
	RemoteLocator    *string
	ServicedEventUID string
	Authoritative    bool
}

// IssueStatusWriter atomically records a provider observation and its exact
// pending-event acknowledgement.
type IssueStatusWriter interface {
	ObserveIssueStatus(context.Context, IssueStatusObservationParams) (bool, []Event, error)
}

// PlanIssueStatusObservation separates provider timestamp authority from local
// content clocks. Only an exact pending event can be acknowledged; older
// acknowledgements never become inward status writes after cancellation.
func PlanIssueStatusObservation(current IssueStatusMapping, p IssueStatusObservationParams, localStatus string) (accept, apply, ack bool, err error) {
	if (p.ClosedReason != "" && p.ClosedReason != "done" && p.ClosedReason != "wontfix") || p.MappingID <= 0 || p.ExternalID == "" || !uid.Valid(p.IssueUID) || p.Observation.Version.IsZero() || (p.Status != "open" && p.Status != "closed") || (p.ServicedEventUID != "" && !uid.Valid(p.ServicedEventUID)) || (p.RemoteLocator != nil && *p.RemoteLocator == "") {
		return false, false, false, invalidIssueStatusState()
	}
	if prior := current.State.Observed; prior != nil {
		if p.Observation.Version.Before(prior.Version) {
			return false, false, false, nil
		}
		if p.Observation.Version.Equal(prior.Version) && !p.Authoritative && !sameStatusRaw(prior.Raw, p.Observation.Raw) {
			return false, false, false, invalidIssueStatusState()
		}
	}
	ack = p.ServicedEventUID != "" && p.ServicedEventUID == current.State.PendingEventUID
	if ack {
		if current.PendingEvent == nil || (current.PendingEvent.Type == "issue.closed" && p.Status != "closed") || (current.PendingEvent.Type == "issue.reopened" && p.Status != "open") {
			return false, false, false, invalidIssueStatusState()
		}
	}
	return true, current.State.PendingEventUID == "" && p.ServicedEventUID == "" && localStatus != p.Status, ack, nil
}

func sameStatusRaw(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// IssueStatusSummaryReader exposes only a count to operator status output.
// The private observation and event authority remain internal to the worker.
type IssueStatusSummaryReader interface {
	CountPendingIssueStatuses(context.Context, int64) (int, error)
}

// IssueStatusLocator comes only from a verified API identity row. Aliases allow
// existing mappings to adopt the canonical foreign identity without duplicates.
type IssueStatusLocator struct {
	ExternalID        string
	LegacyExternalIDs []string
	Locator           string
}

// IssueStatusLocatorStore saves provider-verified remote object locators.
type IssueStatusLocatorStore interface {
	SaveIssueStatusLocator(context.Context, IssueSyncImportGuard, IssueStatusLocator) (bool, error)
}
