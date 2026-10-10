package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

// This index is derived from ordinary documents inside a read/transaction.
// It records historical identity, never execution permission or current winners.
type cronDefinitionReference struct {
	projectID        int64
	kind, uid, event string
}
type cronDefinitionIdentity struct{ kind, uid string }
type cronReferenceIndex struct {
	owners map[cronDefinitionIdentity]int64
	events map[string]cronDefinitionReference
}

// Only relationship/document failures carry this marker. SQL query, scan,
// iteration and context errors retain their ordinary operational categories.
type cronReferenceValidationError struct{ cause error }

func (e *cronReferenceValidationError) Error() string { return e.cause.Error() }
func (e *cronReferenceValidationError) Unwrap() error { return e.cause }
func invalidCronReference(err error) error {
	return &cronReferenceValidationError{cause: err}
}
func classifyCronReferenceError(err, validation error) error {
	if _, ok := errors.AsType[*cronReferenceValidationError](err); ok {
		return fmt.Errorf("%w: %w", validation, err)
	}
	return err
}

func newCronReferenceIndex() *cronReferenceIndex {
	return &cronReferenceIndex{owners: map[cronDefinitionIdentity]int64{}, events: map[string]cronDefinitionReference{}}
}
func (idx *cronReferenceIndex) check(ref cronDefinitionReference) error {
	if owner, known := idx.owners[cronDefinitionIdentity{ref.kind, ref.uid}]; known && owner != ref.projectID {
		return invalidCronReference(fmt.Errorf("%s definition reference belongs to another project", ref.kind))
	}
	if actual, known := idx.events[ref.event]; known && actual != ref {
		return invalidCronReference(fmt.Errorf("%s definition event reference belongs to a different definition or project", ref.kind))
	}
	return nil
}
func (idx *cronReferenceIndex) add(ref cronDefinitionReference) error {
	if err := idx.check(ref); err != nil {
		return err
	}
	idx.owners[cronDefinitionIdentity{ref.kind, ref.uid}] = ref.projectID
	idx.events[ref.event] = ref
	return nil
}
func cronRunReferences(projectID int64, run CronRun) ([]cronDefinitionReference, error) {
	refs := []cronDefinitionReference{}
	for _, pair := range []struct {
		kind       string
		uid, event *string
	}{{"job", run.JobUID, run.DefinitionEventUID}, {"workflow", run.WorkflowUID, run.WorkflowDefinitionEventUID}} {
		if pair.uid == nil && pair.event == nil {
			continue
		}
		if pair.uid == nil || pair.event == nil {
			return nil, invalidCronReference(fmt.Errorf("incomplete run definition reference"))
		}
		refs = append(refs, cronDefinitionReference{projectID: projectID, kind: pair.kind, uid: *pair.uid, event: *pair.event})
	}
	return refs, nil
}

// Snapshots retain the original event identity; their new envelope UID is not
// a definition version. Ordinary unrelated event UIDs cannot serve as versions.
func (idx *cronReferenceIndex) event(projectID int64, e FoldEvent) ([]cronDefinitionReference, error) {
	if isCronDefinitionEvent(e.Type) {
		_, value, err := parseCronDefinitionEvent(e)
		if err != nil {
			return nil, invalidCronReference(err)
		}
		kind := "workflow"
		if strings.HasPrefix(e.Type, "cron.job.") {
			kind = "job"
		}
		if err := idx.add(cronDefinitionReference{projectID: projectID, kind: kind, uid: value.UID, event: value.DefinitionEventUID}); err != nil {
			return nil, err
		}
		if e.UID == value.DefinitionEventUID {
			return nil, nil
		}
	}
	if actual, exists := idx.events[e.UID]; exists && (actual.kind != "" || actual.projectID != projectID) {
		return nil, invalidCronReference(fmt.Errorf("definition version UID names an unrelated event"))
	}
	idx.events[e.UID] = cronDefinitionReference{projectID: projectID}
	if e.Type != "cron.run.observed" && e.Type != "cron.run.snapshot" {
		return nil, nil
	}
	// Database ownership is the row's project_id, including ordinary project
	// moves/adoption. The original portable envelope can precede that move.
	var body CronRunObservation
	if err := json.Unmarshal(e.Payload, &body); err != nil {
		return nil, invalidCronReference(err)
	}
	e.ProjectUID = body.ProjectUID
	value, err := parseCronRunObservation(e)
	if err != nil {
		return nil, invalidCronReference(err)
	}
	return cronRunReferences(projectID, value.CronRun)
}

func validateCronImportReferences(records []ImportRecord) error {
	idx := newCronReferenceIndex()
	runs := map[string]CronRun{}
	rememberRun := func(run CronRun) error {
		if old, exists := runs[run.UID]; exists && (old.ProjectID != run.ProjectID || !sameCronRunIdentity(old, run)) {
			return invalidCronReference(fmt.Errorf("run %s immutable identity changed in import", run.UID))
		}
		runs[run.UID] = run
		return nil
	}
	refs := []cronDefinitionReference{}
	for _, record := range records {
		switch value := record.(type) {
		case *CronJobExport:
			if err := idx.add(cronDefinitionReference{projectID: value.ProjectID, kind: "job", uid: value.UID, event: value.DefinitionEventUID}); err != nil {
				return err
			}
		case *CronWorkflowExport:
			if err := idx.add(cronDefinitionReference{projectID: value.ProjectID, kind: "workflow", uid: value.UID, event: value.DefinitionEventUID}); err != nil {
				return err
			}
		}
	}
	for _, record := range records {
		switch value := record.(type) {
		case *EventExport:
			if value.Type == "cron.run.observed" || value.Type == "cron.run.snapshot" {
				var body CronRunObservation
				if err := json.Unmarshal(value.Payload, &body); err != nil {
					return invalidCronReference(err)
				}
				run := body.Run()
				run.ProjectID = value.ProjectID
				if err := rememberRun(run); err != nil {
					return err
				}
			}
			incoming, err := idx.event(value.ProjectID, FoldEvent{UID: value.UID, Type: value.Type, OriginInstanceUID: value.OriginInstanceUID, HLCPhysicalMS: value.HLCPhysicalMS, HLCCounter: value.HLCCounter, Payload: value.Payload})
			if err != nil {
				return err
			}
			refs = append(refs, incoming...)
		case *CronRunExport:
			if err := rememberRun(CronRun(*value)); err != nil {
				return err
			}
			incoming, err := cronRunReferences(value.ProjectID, CronRun(*value))
			if err != nil {
				return err
			}
			refs = append(refs, incoming...)
		}
	}
	// Complete-input facts are collected before examining references. Missing
	// compacted versions remain valid, but known foreign/mismatched versions and
	// contradictory historical evidence fail before either replacement starts.
	for _, ref := range refs {
		if err := idx.add(ref); err != nil {
			return err
		}
	}
	return nil
}

// CronReplayValidator belongs to one write transaction. Its facts never survive
// commit/retry, and are historical identity evidence, not execution permission.
type CronReplayValidator struct {
	tx              *sql.Tx
	postgres        bool
	idx             *cronReferenceIndex
	loaded          map[cronDefinitionReference]bool
	historyPrepared map[cronDefinitionReference]bool
	ownersLoaded    map[cronDefinitionIdentity]bool
	eventsLoaded    map[string]bool
	runsLoaded      map[int64]bool
	runs            map[string]CronRun
}

// NewCronReplayValidator creates ephemeral transaction/batch reference data.
// SQLite is the default; PostgreSQL callers pass true for decoded JSON predicates.
// NewCronReplayValidator validates cron replay inside one transaction.
// postgres selects PostgreSQL SQL for the shared history queries.
func NewCronReplayValidator(tx *sql.Tx, postgres bool) *CronReplayValidator {
	return &CronReplayValidator{tx: tx, postgres: postgres, idx: newCronReferenceIndex(), loaded: map[cronDefinitionReference]bool{}, historyPrepared: map[cronDefinitionReference]bool{}, ownersLoaded: map[cronDefinitionIdentity]bool{}, eventsLoaded: map[string]bool{}, runsLoaded: map[int64]bool{}, runs: map[string]CronRun{}}
}

func (v *CronReplayValidator) loadKnown(ctx context.Context, ref cronDefinitionReference) error {
	identity := cronDefinitionIdentity{ref.kind, ref.uid}
	if !v.ownersLoaded[identity] {
		table := "cron_jobs"
		if ref.kind == "workflow" {
			table = "cron_workflows"
		}
		current := cronDefinitionReference{kind: ref.kind, uid: ref.uid}
		err := v.tx.QueryRowContext(ctx, "SELECT project_id,definition_event_uid FROM "+table+" WHERE uid=$1", ref.uid).Scan(&current.projectID, &current.event)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if err := v.idx.add(current); err != nil {
				return err
			}
		}
		v.ownersLoaded[identity] = true
	}
	if !v.eventsLoaded[ref.event] {
		var e FoldEvent
		var projectID int64
		var payload string
		err := v.tx.QueryRowContext(ctx, `SELECT project_id,uid,type,origin_instance_uid,hlc_physical_ms,hlc_counter,payload FROM events WHERE uid=$1`, ref.event).Scan(&projectID, &e.UID, &e.Type, &e.OriginInstanceUID, &e.HLCPhysicalMS, &e.HLCCounter, &payload)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			e.Payload = []byte(payload)
			refs, err := v.idx.event(projectID, e)
			if err != nil {
				return err
			}
			for _, r := range refs {
				if err := v.idx.add(r); err != nil {
					return err
				}
			}
		}
		v.eventsLoaded[ref.event] = true
	}
	if err := v.idx.check(ref); err != nil {
		return err
	}
	return nil
}
func (v *CronReplayValidator) load(ctx context.Context, ref cronDefinitionReference) error {
	if !v.loaded[ref] {
		if err := v.prepareReferences(ctx, []cronDefinitionReference{ref}); err != nil {
			return err
		}
		v.loaded[ref] = true
	}
	return v.idx.check(ref)
}

// PrepareEvents batches historical probes for incoming targets before insertion.
// Validation still runs for every event; prepared facts grant no admission.
func (v *CronReplayValidator) PrepareEvents(ctx context.Context, projectID int64, events []RemoteEvent) error {
	refs := []cronDefinitionReference{}
	for _, event := range events {
		e := FoldEvent{UID: event.EventUID, ProjectUID: event.ProjectUID, OriginInstanceUID: event.OriginInstanceUID, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, Payload: event.Payload, Type: event.Type}
		if isCronDefinitionEvent(event.Type) {
			_, value, err := parseCronDefinitionEvent(e)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrFederationIngestValidation, err)
			}
			kind := "workflow"
			if strings.HasPrefix(event.Type, "cron.job.") {
				kind = "job"
			}
			refs = append(refs, cronDefinitionReference{projectID: projectID, kind: kind, uid: value.UID, event: value.DefinitionEventUID})
		} else if event.Type == "cron.run.observed" || event.Type == "cron.run.snapshot" {
			value, err := parseCronRunObservation(e)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrFederationIngestValidation, err)
			}
			incoming, err := cronRunReferences(projectID, value.CronRun)
			if err != nil {
				return classifyCronReferenceError(err, ErrFederationIngestValidation)
			}
			refs = append(refs, incoming...)
		}
	}
	return classifyCronReferenceError(v.prepareReferences(ctx, refs), ErrFederationIngestValidation)
}

func (v *CronReplayValidator) prepareReferences(ctx context.Context, refs []cronDefinitionReference) error {
	needed := map[cronDefinitionReference]bool{}
	uids, events := map[string]bool{}, map[string]bool{}
	uidList, eventList := []string{}, []string{}
	for _, ref := range refs {
		if v.historyPrepared[ref] {
			if err := v.idx.check(ref); err != nil {
				return err
			}
			continue
		}
		if err := v.loadKnown(ctx, ref); err != nil {
			return err
		}
		if actual, known := v.idx.events[ref.event]; known && actual == ref {
			v.historyPrepared[ref] = true
			continue
		}
		needed[ref] = true
		if !uids[ref.uid] {
			uids[ref.uid] = true
			uidList = append(uidList, ref.uid)
		}
		if !events[ref.event] {
			events[ref.event] = true
			eventList = append(eventList, ref.event)
		}
	}
	if len(needed) == 0 {
		return nil
	}
	uidJSON, err := json.Marshal(uidList)
	if err != nil {
		return err
	}
	eventJSON, err := json.Marshal(eventList)
	if err != nil {
		return err
	}
	for stage, query := range cronHistoricalReferenceQueries(v.postgres) {
		args := []any{string(uidJSON), string(eventJSON)}
		if stage == 0 {
			args = []any{string(eventJSON)}
		}
		rows, err := v.tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				if stage < 2 {
					var ref cronDefinitionReference
					if err := rows.Scan(&ref.projectID, &ref.kind, &ref.uid, &ref.event); err != nil {
						return err
					}
					if err := v.idx.add(ref); err != nil {
						return err
					}
					continue
				}
				var e FoldEvent
				var projectID int64
				var payload string
				if err := rows.Scan(&projectID, &e.UID, &e.Type, &e.OriginInstanceUID, &e.HLCPhysicalMS, &e.HLCCounter, &payload); err != nil {
					return err
				}
				e.Payload = []byte(payload)
				incoming, err := v.idx.event(projectID, e)
				if err != nil {
					return err
				}
				for _, ref := range incoming {
					if uids[ref.uid] || events[ref.event] {
						if err := v.idx.add(ref); err != nil {
							return err
						}
					}
				}
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
	}
	for ref := range needed {
		if err := v.idx.check(ref); err != nil {
			return err
		}
		v.historyPrepared[ref] = true
	}
	return nil
}
func (v *CronReplayValidator) references(ctx context.Context, refs []cronDefinitionReference) error {
	if err := v.prepareReferences(ctx, refs); err != nil {
		return err
	}
	for _, ref := range refs {
		if err := v.load(ctx, ref); err != nil {
			return err
		}
		if err := v.idx.add(ref); err != nil {
			return err
		}
	}
	return nil
}

// Fixed JSON-set queries read each historical source once per requested batch.
// The leading type LIKE 'cron.%' term matches the partial idx_events_project_cron
// index, so the event search reads only cron events, not the whole event log.
func cronHistoricalReferenceQueries(postgres bool) []string {
	if postgres {
		return []string{
			`WITH requested_versions AS (SELECT jsonb_array_elements_text($1::jsonb) AS value)
SELECT project_id,'job',uid,definition_event_uid FROM cron_jobs WHERE definition_event_uid IN (SELECT value FROM requested_versions)
UNION ALL SELECT project_id,'workflow',uid,definition_event_uid FROM cron_workflows WHERE definition_event_uid IN (SELECT value FROM requested_versions)`,
			`WITH requested_uids AS (SELECT jsonb_array_elements_text($1::jsonb) AS value), requested_versions AS (SELECT jsonb_array_elements_text($2::jsonb) AS value)
SELECT DISTINCT project_id,'job',job_uid,definition_event_uid FROM cron_runs WHERE job_uid IN (SELECT value FROM requested_uids) OR definition_event_uid IN (SELECT value FROM requested_versions)
UNION SELECT DISTINCT project_id,'workflow',workflow_uid,workflow_definition_event_uid FROM cron_runs WHERE workflow_uid IN (SELECT value FROM requested_uids) OR workflow_definition_event_uid IN (SELECT value FROM requested_versions)`,
			`WITH requested_uids AS (SELECT jsonb_array_elements_text($1::jsonb) AS value), requested_versions AS (SELECT jsonb_array_elements_text($2::jsonb) AS value)
SELECT project_id,uid,type,origin_instance_uid,hlc_physical_ms,hlc_counter,payload FROM events
WHERE type LIKE 'cron.%' AND (((type LIKE 'cron.job.%' OR type LIKE 'cron.workflow.%') AND ((payload::jsonb ->> 'uid') IN (SELECT value FROM requested_uids) OR (payload::jsonb ->> 'definition_event_uid') IN (SELECT value FROM requested_versions))) OR (type IN ('cron.run.observed','cron.run.snapshot') AND ((payload::jsonb ->> 'job_uid') IN (SELECT value FROM requested_uids) OR (payload::jsonb ->> 'workflow_uid') IN (SELECT value FROM requested_uids) OR (payload::jsonb ->> 'definition_event_uid') IN (SELECT value FROM requested_versions) OR (payload::jsonb ->> 'workflow_definition_event_uid') IN (SELECT value FROM requested_versions))))`,
		}
	}
	return []string{
		`WITH requested_versions AS (SELECT value FROM json_each($1))
SELECT project_id,'job',uid,definition_event_uid FROM cron_jobs WHERE definition_event_uid IN (SELECT value FROM requested_versions)
UNION ALL SELECT project_id,'workflow',uid,definition_event_uid FROM cron_workflows WHERE definition_event_uid IN (SELECT value FROM requested_versions)`,
		`WITH requested_uids AS (SELECT value FROM json_each($1)), requested_versions AS (SELECT value FROM json_each($2))
SELECT DISTINCT project_id,'job',job_uid,definition_event_uid FROM cron_runs WHERE job_uid IN (SELECT value FROM requested_uids) OR definition_event_uid IN (SELECT value FROM requested_versions)
UNION SELECT DISTINCT project_id,'workflow',workflow_uid,workflow_definition_event_uid FROM cron_runs WHERE workflow_uid IN (SELECT value FROM requested_uids) OR workflow_definition_event_uid IN (SELECT value FROM requested_versions)`,
		`WITH requested_uids AS (SELECT value FROM json_each($1)), requested_versions AS (SELECT value FROM json_each($2))
SELECT project_id,uid,type,origin_instance_uid,hlc_physical_ms,hlc_counter,payload FROM events
WHERE type LIKE 'cron.%' AND (((type LIKE 'cron.job.%' OR type LIKE 'cron.workflow.%') AND (json_extract(payload,'$.uid') IN (SELECT value FROM requested_uids) OR json_extract(payload,'$.definition_event_uid') IN (SELECT value FROM requested_versions))) OR (type IN ('cron.run.observed','cron.run.snapshot') AND (json_extract(payload,'$.job_uid') IN (SELECT value FROM requested_uids) OR json_extract(payload,'$.workflow_uid') IN (SELECT value FROM requested_uids) OR json_extract(payload,'$.definition_event_uid') IN (SELECT value FROM requested_versions) OR json_extract(payload,'$.workflow_definition_event_uid') IN (SELECT value FROM requested_versions))))`,
	}
}
