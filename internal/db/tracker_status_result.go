package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"strings"
	"time"
)

// TrackerStatusResult is the small retained provider observation. It contains
// no transport error, credential or owner-local selector. Root application must
// match this exact result before acknowledging a named execution lane.
type TrackerStatusResult struct {
	Raw          *string    `json:"raw"`
	Status       string     `json:"status"`
	ClosedReason string     `json:"closed_reason,omitempty"`
	ClosedAt     *time.Time `json:"closed_at,omitempty"`
	Version      time.Time  `json:"version"`
	Locator      string     `json:"locator,omitempty"`
}

func (r TrackerStatusResult) matches(p IssueStatusObservationParams, provider string) bool {
	sameString := func(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
	sameTime := func(a, b *time.Time) bool { return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b)) }
	locator := ""
	if p.RemoteLocator != nil {
		locator = *p.RemoteLocator
	} else if provider == "notion" {
		locator, _ = strings.CutPrefix(p.ExternalID, "page:")
		if locator == p.ExternalID {
			locator = ""
		}
	}
	return (r.Status == "open" || r.Status == "closed") && !r.Version.IsZero() && r.Status == p.Status && r.Version.Equal(p.Observation.Version) && sameString(r.Raw, p.Observation.Raw) && r.ClosedReason == p.ClosedReason && sameTime(r.ClosedAt, p.ClosedAt) && (r.Locator == "" || r.Locator == locator)
}

// AcknowledgeStatusInTransaction joins exact-event application and retained
// semantic history. It is never an HTTP result-admission shortcut: only a
// previously confirmed local coordinator head can be acknowledged here.
func (a TrackerFenceSQL) AcknowledgeStatusInTransaction(ctx context.Context, tx *sql.Tx, binding IssueSyncBinding, p IssueStatusObservationParams) error {
	if p.TrackerWriteUID == "" {
		return nil
	}
	if !trackerDigest(p.TrackerWriteUID) || p.ServicedEventUID == "" {
		return ErrTrackerFenceInvalid
	}
	var projectUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, binding.ProjectID).Scan(&projectUID); err != nil {
		return err
	}
	key, _ := TrackerExecutionFenceKey(a.InstanceUID, projectUID, "coordinator")
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&raw); err != nil {
		return err
	}
	var fence TrackerExecutionFence
	if len(raw) > 32<<10 || json.Unmarshal([]byte(raw), &fence, json.RejectUnknownMembers(true)) != nil || fence.Validate() != nil || !trackerKeyMatches(key, fence) || fence.WriteUID != p.TrackerWriteUID || fence.PendingEventUID != p.ServicedEventUID || fence.SourceKey != binding.SourceKey || fence.Locator != p.ExternalID || fence.Target != p.Status || (fence.Phase != TrackerWriteConfirmed && fence.Phase != TrackerWriteAcknowledged) {
		return ErrTrackerFenceChanged
	}
	selection, err := IssueSyncConnectionFromConfig(binding.Config)
	if err != nil {
		return err
	}
	if selection == nil || *selection != fence.Connection {
		return ErrIssueSyncBindingChanged
	}
	var receipt TrackerStatusResult
	if json.Unmarshal(fence.Result, &receipt, json.RejectUnknownMembers(true)) != nil || !receipt.matches(p, binding.Provider) {
		return ErrTrackerFenceInvalid
	}
	if fence.Phase == TrackerWriteAcknowledged {
		return nil
	}
	next := fence
	next.Phase = TrackerWriteAcknowledged
	return a.StoreInTransaction(ctx, tx, &fence, next, fence.ProcessEpoch)
}
