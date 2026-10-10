package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// CronEventFeature identifies support for portable definitions and run observations.
const CronEventFeature = "cron_v1"

// EventFeaturesHeader advertises supported federation event formats.
const EventFeaturesHeader = "X-Kata-Event-Features"

// RequiredEventFeaturesHeader names formats needed to read a project history.
const RequiredEventFeaturesHeader = "X-Kata-Required-Event-Features"

// ErrFederationNotEnabled reports a metadata read for a project that is not an
// enabled federation hub.
var ErrFederationNotEnabled = errors.New("project must be an enabled federation hub")

// ErrUnsupportedEventFeatures prevents peers from consuming unknown event formats.
var ErrUnsupportedEventFeatures = errors.New("unsupported event features; upgrade the federation peer")

// RequireEventFeatures rejects any required format absent from the advertised set.
func RequireEventFeatures(supported, required string) error {
	available := map[string]bool{}
	for feature := range strings.SplitSeq(supported, ",") {
		available[strings.TrimSpace(feature)] = true
	}
	for feature := range strings.SplitSeq(required, ",") {
		feature = strings.TrimSpace(feature)
		if feature != "" && !available[feature] {
			return fmt.Errorf("%w: %s", ErrUnsupportedEventFeatures, feature)
		}
	}
	return nil
}

// EventRequiredFeatures returns the wire feature needed for an event type.
func EventRequiredFeatures(kind string) string {
	if strings.HasPrefix(kind, "cron.") {
		return CronEventFeature
	}
	return ""
}

// ProjectRequiredEventFeatures inspects current and historical project cron data.
func ProjectRequiredEventFeatures(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, projectUID string) (string, error) {
	var present bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects p WHERE p.uid=$1 AND (
 EXISTS(SELECT 1 FROM cron_jobs WHERE project_id=p.id) OR
 EXISTS(SELECT 1 FROM cron_workflows WHERE project_id=p.id) OR
 EXISTS(SELECT 1 FROM cron_runs WHERE project_id=p.id) OR
 EXISTS(SELECT 1 FROM events WHERE project_id=p.id AND type LIKE 'cron.%')))`, projectUID).Scan(&present)
	if err != nil {
		return "", err
	}
	if present {
		return CronEventFeature, nil
	}
	return "", nil
}

// FederationReadParams captures metadata, required features, purge boundary and event
// page in one backend transaction. Metadata may refresh a purged baseline;
// unsupported readers fail before that write or any returned reset/cursor.
// Features is protocol support, independent of enrollment permissions.
type FederationReadParams struct {
	ProjectID int64
	Features  string
	Metadata  bool
	AfterID   int64
	Limit     int
}

// FederationReadResult captures one consistent metadata or event-page transaction.
type FederationReadResult struct {
	Project                Project
	Binding                FederationBinding
	RequiredFeatures       string
	BaselineThroughEventID int64
	Events                 []Event
	NextAfterID            int64
	ResetAfterID           int64
}

// ReadFederationTransaction supplies common decisions while each backend owns
// its isolation/retry/fences, baseline event writer and event row decoder.
func ReadFederationTransaction(ctx context.Context, tx *sql.Tx, in FederationReadParams, project Project, binding FederationBinding, refresh func() (FederationBinding, error), readEvents func() ([]Event, error)) (FederationReadResult, error) {
	out := FederationReadResult{Project: project, Binding: binding, Events: []Event{}, NextAfterID: in.AfterID}
	var err error
	out.RequiredFeatures, err = ProjectRequiredEventFeatures(ctx, tx, project.UID)
	if err != nil {
		return out, err
	}
	if err = RequireEventFeatures(in.Features, out.RequiredFeatures); err != nil {
		return out, err
	}
	after := in.AfterID
	if in.Metadata {
		after = binding.ReplayHorizonEventID
	}
	var reset sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT MAX(cursor) FROM (
 SELECT MAX(purge_reset_after_event_id) AS cursor FROM purge_log WHERE project_id=$1 AND purge_reset_after_event_id>$2
 UNION ALL SELECT MAX(purge_reset_after_event_id) AS cursor FROM project_purge_log WHERE project_id=$1 AND purge_reset_after_event_id>$2) resets`, project.ID, after).Scan(&reset)
	if err != nil {
		return out, err
	}
	if in.Metadata {
		if binding.Role != FederationRoleHub || !binding.Enabled {
			return out, ErrFederationNotEnabled
		}
		if reset.Int64 > 0 {
			binding, err = refresh()
			if err != nil {
				return out, err
			}
			out.Binding = binding
		}
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),$2) FROM events WHERE project_id=$1 AND id >= $2 AND type IN (`+FederationSnapshotEventTypesSQL()+`)`, project.ID, binding.ReplayHorizonEventID).Scan(&out.BaselineThroughEventID)
		return out, err
	}
	if reset.Int64 > 0 {
		out.ResetAfterID = reset.Int64
		out.NextAfterID = reset.Int64
		return out, nil
	}
	out.Events, err = readEvents()
	if err != nil {
		return out, err
	}
	// Inspect the returned page itself, including when a store was imported from
	// an earlier snapshot with a missing protocol latch. Never trust a pre-read.
	for _, event := range out.Events {
		required := EventRequiredFeatures(event.Type)
		if err := RequireEventFeatures(in.Features, required); err != nil {
			return out, err
		}
		if required != "" {
			out.RequiredFeatures = required
		}
		out.NextAfterID = event.ID
	}
	return out, nil
}
