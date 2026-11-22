package db

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.kenn.io/kata/internal/metadata"
)

// CronEventFeature identifies support for all cron definitions, runs and native delivery state.
const CronEventFeature = "cron_v1"

// IssueMoveEventFeature identifies support for moving issues between projects.
const IssueMoveEventFeature = "issue_move_v1"

// IssueDateZoneEventFeature identifies support for dates carrying zone names.
const IssueDateZoneEventFeature = "issue_date_zone_v1"

// RepeatedAdoptionEventFeature identifies a peer that understands a repeated
// adoption. A spoke with it deletes its own adoption snapshot when the hub
// already held the issue, and a hub accepts a repeated adoption snapshot only
// from such a spoke. A reader with it replays the RepeatedImportEventType
// event a hub writes for a winning copy.
const RepeatedAdoptionEventFeature = "repeated_adoption_v1"

// RelayCrossLinksEventFeature identifies support for cross-project relay links.
const RelayCrossLinksEventFeature = "relay_cross_links_v1"

// SupportedEventFeatures lists event capabilities understood by this daemon.
const SupportedEventFeatures = CronEventFeature + "," + DocbankOriginEventFeature + "," + IssueMoveEventFeature + "," + RepeatedAdoptionEventFeature + "," + IssueDateZoneEventFeature + "," + RelayCrossLinksEventFeature

// Native cron job projections are immutable and retained as tombstones, so a
// positive job result remains valid for that project UID's lifetime.
var nativeProjectFeatureCache sync.Map

// MergeEventFeatures retains every format encountered across a page or transaction.
func MergeEventFeatures(values ...string) string {
	set := map[string]bool{}
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				set[part] = true
			}
		}
	}
	parts := make([]string, 0, len(set))
	for part := range set {
		parts = append(parts, part)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// PayloadRequiredFeatures supplements type classification with payload features.
func PayloadRequiredFeatures(kind, payload string) (string, error) {
	native, nativeErr := payloadHasNativeRecurringState(kind, payload)
	zone, zoneErr := payloadZonedDateFeature(kind, payload)
	origin, originErr := payloadDocbankOriginFeature(kind, payload)
	if err := cmp.Or(nativeErr, zoneErr, originErr); err != nil {
		return "", err
	}
	if native {
		zone = MergeEventFeatures(zone, CronEventFeature)
	}
	return MergeEventFeatures(EventRequiredFeatures(kind), zone, origin), nil
}

func payloadZonedDateFeature(kind, payload string) (string, error) {
	if !strings.HasPrefix(kind, "issue.") && kind != "projection.issue" {
		return "", nil
	}
	if payload == "" {
		payload = "{}"
	}
	var body struct {
		Metadata map[string]jsontext.Value `json:"metadata"`
		Diff     map[string]struct {
			To jsontext.Value `json:"to"`
		} `json:"diff"`
		Snapshot jsontext.Value `json:"snapshot"`
	}
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		return "", err
	}
	if kind == "issue.moved" && len(body.Snapshot) > 0 {
		return payloadZonedDateFeature("issue.snapshot", string(body.Snapshot))
	}
	for _, key := range []string{"scheduled_on", "deadline_on"} {
		for _, raw := range []jsontext.Value{body.Metadata[key], body.Diff[key].To} {
			var value string
			if len(raw) > 0 && json.Unmarshal(raw, &value) == nil {
				if _, zone := metadata.SplitScheduleZone(value); zone != "" {
					return IssueDateZoneEventFeature, nil
				}
			}
		}
	}
	return "", nil
}

// payloadHasNativeRecurringState identifies hub-owned state independently of
// the wire capability shared with portable cron.
func payloadHasNativeRecurringState(kind, payload string) (bool, error) {
	issueState := kind == "projection.issue" || strings.HasPrefix(kind, "issue.")
	jobDefinition := kind == "projection.job" || strings.HasPrefix(kind, "cron.job.")
	// Delivery state lives only in issue metadata and native job definitions;
	// other payloads, such as project metadata, may reuse those key names freely.
	if !issueState && !jobDefinition {
		return false, nil
	}
	if payload == "" {
		payload = "{}"
	}
	var body map[string]jsontext.Value
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		return false, err
	}
	var native bool
	var err error
	if issueState {
		native, err = nativeIssueMetadata(body)
	} else {
		native, err = nativeJobDefinition(body["definition"])
	}
	return native, err
}

func nativeIssueMetadata(body map[string]jsontext.Value) (bool, error) {
	for _, field := range []string{"metadata", "diff"} {
		var keys map[string]jsontext.Value
		if raw := body[field]; len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &keys); err != nil {
				return false, err
			}
			for key := range keys {
				if recurringMetadataKey(key) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func nativeJobDefinition(raw jsontext.Value) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	var definition struct {
		Action struct {
			Kind string `json:"kind"`
		} `json:"action"`
		Issue *struct {
			Kind string `json:"kind"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(raw, &definition); err != nil {
		return false, err
	}
	return definition.Action.Kind == "notify" && definition.Issue != nil && definition.Issue.Kind == "per-run", nil
}

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
	if kind == "issue.moved" {
		return IssueMoveEventFeature
	}
	if kind == RepeatedImportEventType {
		return RepeatedAdoptionEventFeature
	}
	if strings.HasPrefix(kind, "cron.") {
		return CronEventFeature
	}
	return ""
}

// ProjectRequiredEventFeatures inspects current cron data and native issue state.
func ProjectRequiredEventFeatures(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, projectUID string) (string, error) {
	var cronPresent, issueMoved bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects p WHERE p.uid=$1 AND (
 EXISTS(SELECT 1 FROM cron_jobs WHERE project_id=p.id) OR
 EXISTS(SELECT 1 FROM cron_workflows WHERE project_id=p.id) OR
 EXISTS(SELECT 1 FROM cron_runs WHERE project_id=p.id) OR
	 EXISTS(SELECT 1 FROM events WHERE project_id=p.id AND type LIKE 'cron.%'))),
 EXISTS(SELECT 1 FROM projects p WHERE p.uid=$1 AND p.name<>$2 AND EXISTS(
  SELECT 1 FROM events e WHERE e.project_id=p.id AND e.type='issue.moved'))`, projectUID, SystemProjectName).Scan(&cronPresent, &issueMoved)
	if err != nil {
		return "", err
	}
	required := ""
	if issueMoved {
		required = MergeEventFeatures(required, IssueMoveEventFeature)
	}
	if cronPresent {
		required = MergeEventFeatures(required, CronEventFeature)
		if _, cached := nativeProjectFeatureCache.Load(projectUID); cached {
			return MergeEventFeatures(required, CronEventFeature), nil
		}
	}
	if !cronPresent && !issueMoved {
		return required, nil
	}
	// Native jobs cannot convert to adapter ownership, and cron projections
	// (including tombstones) cannot be hard-deleted while their project lives.
	// A moved issue event is the only path that can place native child metadata
	// in a project without its job projection. Probe those moved issues directly.
	// An explicit escape keeps backslash literal on both SQLite and PostgreSQL.
	if cronPresent {
		native, err := nativeProjectionRequired(ctx, q, nativeCronJobsQuery, projectUID)
		if err != nil {
			return "", err
		}
		if native {
			nativeProjectFeatureCache.Store(projectUID, struct{}{})
			return MergeEventFeatures(required, CronEventFeature), nil
		}
	}
	if issueMoved {
		native, err := nativeProjectionRequired(ctx, q, `SELECT DISTINCT 'projection.issue',i.metadata FROM events e JOIN projects p ON p.id=e.project_id JOIN issues i ON i.uid=e.issue_uid AND i.project_id=e.project_id WHERE p.uid=$1 AND e.type='issue.moved' AND (i.metadata LIKE '%"cron.occurrence"%' OR i.metadata LIKE '%"cron.progress.%' OR i.metadata LIKE '%\u%' ESCAPE '!')`, projectUID)
		if err != nil {
			return "", err
		}
		if native {
			return MergeEventFeatures(required, CronEventFeature), nil
		}
	}
	return required, nil
}

const nativeCronJobsQuery = `SELECT 'projection.job',j.definition_json FROM cron_jobs j JOIN projects p ON p.id=j.project_id WHERE p.uid=$1 AND ((j.definition_json LIKE '%"notify"%' AND j.definition_json LIKE '%"per-run"%') OR j.definition_json LIKE '%\u%' ESCAPE '!')`

// projectHasNativeStateForAdoption also checks current issue metadata, because
// adoption replaces local event history before establishing the spoke.
func projectHasNativeStateForAdoption(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, projectUID string) (bool, error) {
	native, err := nativeProjectionRequired(ctx, q, nativeCronJobsQuery, projectUID)
	if err != nil || native {
		return native, err
	}
	// Adoption is infrequent; inspect candidate issue documents here instead of
	// adding an issue-table scan to every federation poll.
	const query = `SELECT 'projection.issue',i.metadata FROM issues i JOIN projects p ON p.id=i.project_id
WHERE p.uid=$1 AND (i.metadata LIKE '%"cron.occurrence"%' OR i.metadata LIKE '%"cron.progress.%' OR i.metadata LIKE '%\u%' ESCAPE '!')`
	return nativeProjectionRequired(ctx, q, query, projectUID)
}

func nativeProjectionRequired(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, query, projectUID string) (bool, error) {
	rows, err := q.QueryContext(ctx, query, projectUID)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind, payload string
		if err := rows.Scan(&kind, &payload); err != nil {
			return false, err
		}
		if kind == "projection.job" {
			payload = `{"definition":` + payload + `}`
		} else {
			payload = `{"metadata":` + payload + `}`
		}
		native, err := payloadHasNativeRecurringState(kind, payload)
		if err != nil {
			return false, err
		}
		if native {
			return true, nil
		}
	}
	return false, rows.Err()
}

// FederationReadParams captures metadata, required features, purge boundary and event
// page in one backend transaction. Metadata may refresh a purged baseline;
// unsupported readers fail and roll back that write before returning any reset/cursor.
// Features is protocol support, independent of enrollment permissions.
type FederationReadParams struct {
	ProjectID int64
	Features  string
	Metadata  bool
	AfterID   int64
	IssueUID  string
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
func ReadFederationTransaction(ctx context.Context, tx *sql.Tx, in FederationReadParams, project Project, binding FederationBinding, refresh func() (FederationBinding, error), readEvents func(int64, int) ([]Event, error)) (FederationReadResult, error) {
	out := FederationReadResult{Project: project, Binding: binding, Events: []Event{}, NextAfterID: in.AfterID}
	var err error
	out.RequiredFeatures, err = ProjectRequiredEventFeatures(ctx, tx, project.UID)
	if err != nil {
		return out, err
	}
	if err = RequireEventFeatures(in.Features, out.RequiredFeatures); err != nil {
		return out, err
	}
	if in.IssueUID != "" {
		var owned bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issues WHERE uid=$1 AND project_id=$2)`, in.IssueUID, project.ID).Scan(&owned); err != nil {
			return out, err
		}
		if !owned {
			return out, ErrNotFound
		}
	}
	after := in.AfterID
	if in.Metadata {
		after = binding.ReplayHorizonEventID
	}
	var reset sql.NullInt64
	var newest int64
	err = tx.QueryRowContext(ctx, `SELECT (SELECT MAX(cursor) FROM (
 SELECT MAX(purge_reset_after_event_id) AS cursor FROM purge_log WHERE project_id=$1 AND purge_reset_after_event_id>$2
 UNION ALL SELECT MAX(purge_reset_after_event_id) AS cursor FROM project_purge_log WHERE project_id=$1 AND purge_reset_after_event_id>$2
 UNION ALL SELECT CAST(value AS BIGINT) AS cursor FROM meta WHERE key=$3 AND CAST(value AS BIGINT)>$2) resets),
 COALESCE((SELECT MAX(id) FROM events),0)`, project.ID, after, ProjectStreamResetMetadataPrefix+project.UID).Scan(&reset, &newest)
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
		if RequireEventFeatures(in.Features, IssueDateZoneEventFeature) != nil {
			if err := checkFederationReplayFeatures(in.Features, binding.ReplayHorizonEventID, readEvents); err != nil {
				return out, err
			}
		}
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),$2) FROM events WHERE project_id=$1 AND id >= $2 AND type IN (`+FederationSnapshotEventTypesSQL()+`)`, project.ID, binding.ReplayHorizonEventID).Scan(&out.BaselineThroughEventID)
		return out, err
	}
	// A retained issue's explicit history is independent of purge gaps for
	// other issues in the destination stream. Ownership was checked above in
	// this same read transaction; a purged subject cannot pass that check.
	if reset.Int64 > 0 && in.IssueUID == "" {
		out.ResetAfterID = reset.Int64
		out.NextAfterID = reset.Int64
		return out, nil
	}
	out.Events, err = readEvents(in.AfterID, in.Limit)
	if err != nil {
		return out, err
	}
	if len(out.Events) == 0 && in.IssueUID == "" {
		// An empty page read every event through the snapshot's newest, and
		// event IDs follow commit order, so no feed event at or below it can
		// appear later. Moving the cursor there keeps the next poll's read to
		// what the hub writes after this one. A move into the project later
		// exposes older history filed elsewhere; spokes read that through the
		// moved issue's own history, whatever their cursor.
		out.NextAfterID = max(in.AfterID, newest)
	}
	// Inspect the returned page itself, including when a store was imported from
	// an earlier snapshot with a missing protocol latch. Never trust a pre-read.
	for _, event := range out.Events {
		required, err := requireFederationEventFeatures(in.Features, event)
		if err != nil {
			return out, err
		}
		if required != "" {
			out.RequiredFeatures = MergeEventFeatures(out.RequiredFeatures, required)
		}
		out.NextAfterID = event.ID
	}
	return out, nil
}

// WithRelayPeerEventFeatures carries the requesting peer's advertised formats.
func WithRelayPeerEventFeatures(ctx context.Context, features string) context.Context {
	return context.WithValue(ctx, relayPeerEventFeaturesKey{}, features)
}

type relayPeerEventFeaturesKey struct{}

// RelayPeerEventFeatures returns the requesting peer's advertised formats.
func RelayPeerEventFeatures(ctx context.Context) string {
	features, _ := ctx.Value(relayPeerEventFeaturesKey{}).(string)
	return features
}

// RelayEventFeaturePrefix returns the deliverable prefix supported by the peer.
func RelayEventFeaturePrefix(ctx context.Context, backend interface {
	AuthorizeRelayReleasePrefix(context.Context, string, []RemoteEvent) (int, error)
}, projectUID string, envelopes []RelayEnvelope) ([]RemoteEvent, error) {
	events := make([]RemoteEvent, 0, len(envelopes))
	for _, envelope := range envelopes {
		event, err := DecodeRelaySourceEvent(envelope.Body)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	count, err := backend.AuthorizeRelayReleasePrefix(ctx, projectUID, events)
	if errors.Is(err, ErrUnsupportedEventFeatures) && count > 0 {
		return events[:count], nil
	}
	if err != nil {
		return nil, err
	}
	return events[:count], nil
}

// A move in the replay can trigger a backfill from before the replay horizon.
func checkFederationReplayFeatures(features string, horizon int64, readEvents func(int64, int) ([]Event, error)) error {
	const limit = 1000
	moved := map[string]bool{}
	for cursor := horizon - 1; ; {
		page, err := readEvents(cursor, limit)
		if err != nil {
			return err
		}
		for _, event := range page {
			if _, err := requireFederationEventFeatures(features, event); err != nil {
				return err
			}
			if event.Type == "issue.moved" {
				moved[EventIssueUID(event.IssueUID)] = true
			}
			cursor = event.ID
		}
		if len(page) < limit {
			break
		}
	}
	for cursor := int64(0); len(moved) > 0; {
		page, err := readEvents(cursor, limit)
		if err != nil {
			return err
		}
		for _, event := range page {
			if event.ID >= horizon {
				return nil
			}
			if moved[EventIssueUID(event.IssueUID)] {
				if _, err := requireFederationEventFeatures(features, event); err != nil {
					return err
				}
			}
			cursor = event.ID
		}
		if len(page) < limit {
			break
		}
	}
	return nil
}

func requireFederationEventFeatures(features string, event Event) (string, error) {
	required, err := PayloadRequiredFeatures(event.Type, event.Payload)
	if err != nil {
		return "", err
	}
	return required, RequireEventFeatures(features, required)
}
