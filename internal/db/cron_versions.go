package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
)

// CronVersionList selects retained local history for one definition.
type CronVersionList struct {
	ProjectID      int64
	UID            string
	Limit          int
	BeforeEventUID string
}

// CronDefinitionVersion retains an edit's document and original clock. A
// baseline has no known edit actor; its envelope actor created the snapshot.
type CronDefinitionVersion struct {
	EventUID      string            `json:"event_uid"`
	Operation     string            `json:"operation"`
	Actor         *string           `json:"actor"`
	DefinitionHLC CronDefinitionHLC `json:"definition_hlc"`
	Name          string            `json:"name"`
	Definition    jsontext.Value    `json:"definition"`
	UpdatedAt     time.Time         `json:"updated_at"`
	DeletedAt     *time.Time        `json:"deleted_at,omitempty"`
	Current       bool              `json:"current"`
}

// CronVersionPage is a live page; concurrent imports may change later pages.
type CronVersionPage struct {
	Versions           []CronDefinitionVersion `json:"versions"`
	NextBeforeEventUID string                  `json:"next_before_event_uid,omitempty"`
	SkippedEventUIDs   []string                `json:"skipped_event_uids,omitempty"`
}

// DefinitionVersions reads the row's winner and its events in one statement,
// so a concurrent edit cannot mark an inconsistent current version. job selects
// a fixed table and event prefix, never a caller-supplied SQL identifier.
func (a CronSQL) DefinitionVersions(ctx context.Context, in CronVersionList, job bool) (CronVersionPage, error) {
	if in.Limit < 0 || in.Limit > 100 {
		return CronVersionPage{}, fmt.Errorf("%w: limit must be between 0 and 100", cron.ErrInvalid)
	}
	if in.Limit == 0 {
		in.Limit = 20
	}
	in.UID = strings.ToUpper(in.UID)
	in.BeforeEventUID = strings.ToUpper(in.BeforeEventUID)
	if in.BeforeEventUID != "" && !cronUID(in.BeforeEventUID) {
		return CronVersionPage{}, fmt.Errorf("%w: invalid version cursor", cron.ErrInvalid)
	}
	prefix := "cron.workflow."
	if job {
		prefix = "cron.job."
	}
	payloadUID := `json_extract(e.payload,'$.uid')`
	if a.Postgres {
		payloadUID = `(e.payload::jsonb ->> 'uid')`
	}
	// The explicit LIKE matches the existing partial cron index. No schema or
	// event-log scan outside the selected project's cron events is needed.
	query := `SELECT d.definition_event_uid,e.uid,COALESCE(e.type,''),COALESCE(e.actor,''),COALESCE(e.origin_instance_uid,''),COALESCE(e.hlc_physical_ms,0),COALESCE(e.hlc_counter,0),COALESCE(e.payload,'{}') FROM ` + cronTable(job) + ` d LEFT JOIN events e ON e.project_id=d.project_id AND e.type LIKE 'cron.%' AND e.type IN ($3,$4,$5,$6,$7) AND ` + payloadUID + `=d.uid WHERE d.project_id=$1 AND d.uid=$2`
	rows, err := a.Query.QueryContext(ctx, query, in.ProjectID, in.UID, prefix+"created", prefix+"updated", prefix+"deleted", prefix+"restored", prefix+"snapshot")
	if err != nil {
		return CronVersionPage{}, err
	}
	defer func() { _ = rows.Close() }()
	events := []FoldEvent{}
	winner := ""
	for rows.Next() {
		var event FoldEvent
		var eventUID sql.NullString
		var payload string
		if err := rows.Scan(&winner, &eventUID, &event.Type, &event.Actor, &event.OriginInstanceUID, &event.HLCPhysicalMS, &event.HLCCounter, &payload); err != nil {
			return CronVersionPage{}, err
		}
		if eventUID.Valid {
			event.UID = eventUID.String
			event.Payload = jsontext.Value(payload)
			// Project merge can retain an earlier project_uid inside the event.
			// SQL scopes the local project; parse validates the retained payload UID.
			events = append(events, event)
		}
	}
	if err := rows.Err(); err != nil {
		return CronVersionPage{}, err
	}
	if winner == "" {
		return CronVersionPage{}, ErrNotFound
	}
	versions, skipped := cronVersionCandidates(events, winner)
	start := 0
	if in.BeforeEventUID != "" {
		index := slices.IndexFunc(versions, func(v CronDefinitionVersion) bool { return v.EventUID == in.BeforeEventUID })
		if index < 0 {
			return CronVersionPage{}, fmt.Errorf("%w: version not found", cron.ErrInvalid)
		}
		start = index + 1
	}
	end := min(start+in.Limit, len(versions))
	out := CronVersionPage{Versions: versions[start:end], SkippedEventUIDs: skipped}
	if end < len(versions) {
		out.NextBeforeEventUID = versions[end-1].EventUID
	}
	return out, nil
}

func cronVersionCandidates(events []FoldEvent, winner string) ([]CronDefinitionVersion, []string) {
	type candidate struct {
		version  CronDefinitionVersion
		clock    FoldClock
		envelope FoldClock
	}
	chosen := map[string]candidate{}
	skipped := []string{}
	for _, event := range events {
		in, value, err := parseCronDefinitionEvent(event)
		if err == nil {
			err = validateCronDefinitionDocument(strings.HasPrefix(event.Type, "cron.job."), true, in.Definition)
		}
		if err != nil {
			skipped = append(skipped, event.UID)
			continue
		}
		snapshot := strings.HasSuffix(event.Type, ".snapshot")
		next := candidate{version: CronDefinitionVersion{EventUID: value.DefinitionEventUID, Operation: event.Type[strings.LastIndex(event.Type, ".")+1:], Actor: &event.Actor, DefinitionHLC: value.DefinitionHLC, Name: value.Name, Definition: in.Definition.Clone(), UpdatedAt: value.UpdatedAt, DeletedAt: value.DeletedAt, Current: value.DefinitionEventUID == winner}, clock: cronDefinitionClock(value), envelope: clockOf(event)}
		if snapshot {
			next.version.Operation = "baseline"
			next.version.Actor = nil
		}
		previous, exists := chosen[value.DefinitionEventUID]
		if !exists || compareClock(next.clock, previous.clock) > 0 || (compareClock(next.clock, previous.clock) == 0 && compareClock(next.envelope, previous.envelope) < 0) {
			chosen[value.DefinitionEventUID] = next
		}
	}
	ordered := make([]candidate, 0, len(chosen))
	for _, value := range chosen {
		ordered = append(ordered, value)
	}
	slices.SortFunc(ordered, func(a, b candidate) int { return compareClock(b.clock, a.clock) })
	versions := make([]CronDefinitionVersion, 0, len(ordered))
	for _, value := range ordered {
		versions = append(versions, value.version)
	}
	slices.Sort(skipped)
	return versions, skipped[:min(20, len(skipped))]
}
