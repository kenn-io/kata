package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"go.kenn.io/kata/internal/cron"
)

// CronRunObservation is attributed portable evidence with immutable run identity.
type CronRunObservation struct {
	ProjectUID          string             `json:"project_uid"`
	ObservationEventUID string             `json:"observation_event_uid,omitempty"`
	ObservationHLC      *CronDefinitionHLC `json:"observation_hlc,omitempty"`

	UID                        string       `json:"uid"`
	JobUID                     *string      `json:"job_uid,omitempty"`
	DefinitionEventUID         *string      `json:"definition_event_uid,omitempty"`
	WorkflowUID                *string      `json:"workflow_uid,omitempty"`
	WorkflowDefinitionEventUID *string      `json:"workflow_definition_event_uid,omitempty"`
	OccurrenceKey              *string      `json:"occurrence_key,omitempty"`
	IssueUID                   *string      `json:"issue_uid,omitempty"`
	Actor                      string       `json:"actor"`
	Teammate                   *string      `json:"teammate,omitempty"`
	ExecutorLabel              *string      `json:"executor_label,omitempty"`
	Status                     string       `json:"status"`
	Summary                    cron.Summary `json:"summary"`
	Revision                   int64        `json:"revision"`
	CreatedAt                  time.Time    `json:"created_at"`
	StartedAt                  *time.Time   `json:"started_at,omitempty"`
	EndedAt                    *time.Time   `json:"ended_at,omitempty"`
	UpdatedAt                  time.Time    `json:"updated_at"`
}

// NewCronRunObservation attaches portable project identity to a stored run.
func NewCronRunObservation(run CronRun, projectUID string) CronRunObservation {
	return CronRunObservation{ProjectUID: projectUID, UID: run.UID, JobUID: run.JobUID, DefinitionEventUID: run.DefinitionEventUID, WorkflowUID: run.WorkflowUID, WorkflowDefinitionEventUID: run.WorkflowDefinitionEventUID, OccurrenceKey: run.OccurrenceKey, IssueUID: run.IssueUID, Actor: run.Actor, Teammate: run.Teammate, ExecutorLabel: run.ExecutorLabel, Status: run.Status, Summary: run.Summary, Revision: run.Revision, CreatedAt: run.CreatedAt, StartedAt: run.StartedAt, EndedAt: run.EndedAt, UpdatedAt: run.UpdatedAt}
}

// Run returns the observation fields without backend-local row identifiers.
func (v CronRunObservation) Run() CronRun {
	return CronRun{UID: v.UID, JobUID: v.JobUID, DefinitionEventUID: v.DefinitionEventUID, WorkflowUID: v.WorkflowUID, WorkflowDefinitionEventUID: v.WorkflowDefinitionEventUID, OccurrenceKey: v.OccurrenceKey, IssueUID: v.IssueUID, Actor: v.Actor, Teammate: v.Teammate, ExecutorLabel: v.ExecutorLabel, Status: v.Status, Summary: v.Summary, Revision: v.Revision, CreatedAt: v.CreatedAt, StartedAt: v.StartedAt, EndedAt: v.EndedAt, UpdatedAt: v.UpdatedAt}
}

// FoldCronRun retains original observation provenance through snapshots.
type FoldCronRun struct {
	CronRun
	ProjectUID          string
	ObservationEventUID string
	ObservationHLC      CronDefinitionHLC
}

func parseCronRunObservation(e FoldEvent) (FoldCronRun, error) {
	if len(e.Payload) > CronObservationLimit {
		return FoldCronRun{}, fmt.Errorf("run observation exceeds byte limit")
	}
	var in CronRunObservation
	if err := json.Unmarshal(e.Payload, &in, json.RejectUnknownMembers(true)); err != nil {
		return FoldCronRun{}, err
	}
	value := in.Run()
	value.ID = 1
	value.ProjectID = 1
	record := CronRunExport(value)
	if err := ValidateCronRecord(&record); err != nil {
		return FoldCronRun{}, err
	}
	if !cronUID(in.ProjectUID) || in.ProjectUID != e.ProjectUID {
		return FoldCronRun{}, fmt.Errorf("run project identity mismatch")
	}
	clock := CronDefinitionHLC{Version: 1, PhysicalMS: e.HLCPhysicalMS, Counter: e.HLCCounter, OriginInstanceUID: e.OriginInstanceUID}
	winner := e.UID
	if e.Type == "cron.run.snapshot" {
		if in.ObservationHLC == nil || !cronUID(in.ObservationEventUID) {
			return FoldCronRun{}, fmt.Errorf("run snapshot requires original observation clock")
		}
		clock = *in.ObservationHLC
		winner = in.ObservationEventUID
	} else if in.ObservationHLC != nil || in.ObservationEventUID != "" {
		return FoldCronRun{}, fmt.Errorf("run observation cannot override envelope clock")
	}
	if err := clock.Validate(); err != nil {
		return FoldCronRun{}, err
	}
	value.ID = 0
	value.ProjectID = 0
	return FoldCronRun{CronRun: value, ProjectUID: in.ProjectUID, ObservationEventUID: winner, ObservationHLC: clock}, nil
}
func runObservationClock(v FoldCronRun) FoldClock {
	return FoldClock{HLCPhysicalMS: v.ObservationHLC.PhysicalMS, HLCCounter: v.ObservationHLC.Counter, OriginInstanceUID: v.ObservationHLC.OriginInstanceUID, EventUID: v.ObservationEventUID}
}

func sameCronRunIdentity(a, b CronRun) bool {
	return a.UID == b.UID && reflect.DeepEqual(a.JobUID, b.JobUID) && reflect.DeepEqual(a.DefinitionEventUID, b.DefinitionEventUID) && reflect.DeepEqual(a.WorkflowUID, b.WorkflowUID) && reflect.DeepEqual(a.WorkflowDefinitionEventUID, b.WorkflowDefinitionEventUID) && reflect.DeepEqual(a.OccurrenceKey, b.OccurrenceKey) && reflect.DeepEqual(a.IssueUID, b.IssueUID) && a.Actor == b.Actor && reflect.DeepEqual(a.Teammate, b.Teammate) && reflect.DeepEqual(a.ExecutorLabel, b.ExecutorLabel) && a.CreatedAt.Equal(b.CreatedAt)
}

// Validate preserves identity constraints using only this transaction's facts.
func (v *CronReplayValidator) Validate(ctx context.Context, projectID int64, event RemoteEvent) error {
	if event.Type != "cron.run.observed" && event.Type != "cron.run.snapshot" && !isCronDefinitionEvent(event.Type) {
		_, err := v.idx.event(projectID, FoldEvent{UID: event.EventUID, Type: event.Type})
		return classifyCronReferenceError(err, ErrFederationIngestValidation)
	}
	e := FoldEvent{UID: event.EventUID, ProjectUID: event.ProjectUID, OriginInstanceUID: event.OriginInstanceUID, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, Payload: event.Payload, Type: event.Type}
	if isCronDefinitionEvent(event.Type) {
		_, definition, err := parseCronDefinitionEvent(e)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrFederationIngestValidation, err)
		}
		kind := "workflow"
		if strings.HasPrefix(event.Type, "cron.job.") {
			kind = "job"
		}
		if err := v.load(ctx, cronDefinitionReference{projectID: projectID, kind: kind, uid: definition.UID, event: definition.DefinitionEventUID}); err != nil {
			return classifyCronReferenceError(err, ErrFederationIngestValidation)
		}
		_, err = v.idx.event(projectID, e)
		return classifyCronReferenceError(err, ErrFederationIngestValidation)
	}
	incoming, err := parseCronRunObservation(e)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFederationIngestValidation, err)
	}
	refs, err := cronRunReferences(projectID, incoming.CronRun)
	if err != nil {
		return classifyCronReferenceError(err, ErrFederationIngestValidation)
	}
	if err := v.references(ctx, refs); err != nil {
		return classifyCronReferenceError(err, ErrFederationIngestValidation)
	}
	if !v.runsLoaded[projectID] {
		// Pending events may precede materialization. Read that project's run history
		// once per batch so identity changes cannot hide behind ordering or compaction.
		rows, err := v.tx.QueryContext(ctx, `SELECT uid,origin_instance_uid,type,hlc_physical_ms,hlc_counter,payload FROM events WHERE project_id=$1 AND type IN ('cron.run.observed','cron.run.snapshot')`, projectID)
		if err != nil {
			return err
		}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var previous FoldEvent
				var payload string
				if err := rows.Scan(&previous.UID, &previous.OriginInstanceUID, &previous.Type, &previous.HLCPhysicalMS, &previous.HLCCounter, &payload); err != nil {
					return err
				}
				previous.Payload = []byte(payload)
				var body CronRunObservation
				if err := json.Unmarshal(previous.Payload, &body); err != nil {
					return fmt.Errorf("%w: %w", ErrFederationIngestValidation, err)
				}
				previous.ProjectUID = body.ProjectUID
				value, err := parseCronRunObservation(previous)
				if err != nil {
					return fmt.Errorf("%w: %w", ErrFederationIngestValidation, err)
				}
				run := value.CronRun
				run.ProjectID = projectID
				if old, ok := v.runs[run.UID]; ok && (old.ProjectID != projectID || !sameCronRunIdentity(old, run)) {
					return fmt.Errorf("%w: run identity changed", ErrFederationIngestValidation)
				}
				v.runs[run.UID] = run
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
		v.runsLoaded[projectID] = true
	}
	incoming.ProjectID = projectID
	previous, known := v.runs[incoming.UID]
	if !known {
		row, err := scanCronRunRow(v.tx.QueryRowContext(ctx, "SELECT "+cronRunColumns+" FROM cron_runs WHERE uid=$1", incoming.UID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			previous = CronRun(row)
			known = true
		}
	}
	if known && (previous.ProjectID != projectID || !sameCronRunIdentity(previous, incoming.CronRun)) {
		return fmt.Errorf("%w: run identity changed", ErrFederationIngestValidation)
	}
	v.runs[incoming.UID] = incoming.CronRun
	// Observation envelopes themselves cannot be used as definition versions.
	if actual, known := v.idx.events[e.UID]; known && actual.kind != "" {
		return fmt.Errorf("%w: definition version UID names an unrelated event", ErrFederationIngestValidation)
	}
	v.idx.events[e.UID] = cronDefinitionReference{projectID: projectID}
	return nil
}
func (p *FoldProjection) applyCronRun(e FoldEvent) {
	value, err := parseCronRunObservation(e)
	if err != nil {
		p.Warnings = append(p.Warnings, err.Error())
		return
	}
	current, exists := p.CronRuns[value.UID]
	if !exists || compareClock(runObservationClock(value), runObservationClock(current)) > 0 {
		p.CronRuns[value.UID] = value
	}
}

func cronRunSnapshots(ctx context.Context, tx *sql.Tx, project Project) ([]CronEvent, error) {
	// A baseline must not invent newer provenance for an existing observation.
	rows, err := tx.QueryContext(ctx, `SELECT uid,origin_instance_uid,type,hlc_physical_ms,hlc_counter,payload FROM events WHERE project_id=$1 AND type IN ('cron.run.observed','cron.run.snapshot')`, project.ID)
	if err != nil {
		return nil, err
	}
	events := []FoldEvent{}
	for rows.Next() {
		var event FoldEvent
		var payload string
		event.ProjectUID = project.UID
		if err := rows.Scan(&event.UID, &event.OriginInstanceUID, &event.Type, &event.HLCPhysicalMS, &event.HLCCounter, &payload); err != nil {
			_ = rows.Close()
			return nil, err
		}
		event.Payload = []byte(payload)
		events = append(events, event)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	folded := FoldEvents(events)
	var instanceUID string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='instance_uid'`).Scan(&instanceUID); err != nil {
		return nil, err
	}
	result := []CronEvent{}
	for record, err := range ExportCronRunsSQL(ctx, tx.QueryContext, ExportFilter{ProjectID: &project.ID, IncludeDeleted: true}) {
		if err != nil {
			return nil, err
		}
		in := NewCronRunObservation(CronRun(record), project.UID)
		// Imported historical rows can predate observation events. Their original
		// update time/instance/UID supplies stable baseline provenance, independent
		// of the freshly generated envelope. Later observations use their original HLC.
		in.ObservationEventUID = record.UID
		clock := CronDefinitionHLC{Version: 1, PhysicalMS: record.UpdatedAt.UnixMilli(), OriginInstanceUID: instanceUID}
		if original, ok := folded.CronRuns[record.UID]; ok {
			in.ObservationEventUID = original.ObservationEventUID
			clock = original.ObservationHLC
		}
		in.ObservationHLC = &clock
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		result = append(result, CronEvent{ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name, Type: "cron.run.snapshot", Payload: string(raw)})
	}
	return result, nil
}

func materializeCronRuns(ctx context.Context, tx *sql.Tx, projectID int64, projectUID string, p FoldProjection, validator *CronReplayValidator) error {
	if len(p.CronRuns) == 0 {
		return nil
	}
	allRefs := []cronDefinitionReference{}
	for _, folded := range p.CronRuns {
		refs, err := cronRunReferences(projectID, folded.CronRun)
		if err != nil {
			return classifyCronReferenceError(err, ErrFederationIngestValidation)
		}
		allRefs = append(allRefs, refs...)
	}
	if err := validator.prepareReferences(ctx, allRefs); err != nil {
		return classifyCronReferenceError(err, ErrFederationIngestValidation)
	}
	for _, folded := range p.CronRuns {
		run := folded.CronRun
		run.ProjectID = projectID
		if folded.ProjectUID != projectUID {
			return fmt.Errorf("%w: run project mismatch", ErrFederationIngestValidation)
		}
		refs, err := cronRunReferences(projectID, run)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrFederationIngestValidation, err)
		}
		if err := validator.references(ctx, refs); err != nil {
			return classifyCronReferenceError(err, ErrFederationIngestValidation)
		}
		old, err := scanCronRunRow(tx.QueryRowContext(ctx, "SELECT "+cronRunColumns+" FROM cron_runs WHERE uid=$1", run.UID))
		if errors.Is(err, sql.ErrNoRows) {
			if err := insertCronRun(ctx, tx, run, false, false); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		existing := CronRun(old)
		if existing.ProjectID != projectID || !sameCronRunIdentity(existing, run) {
			return fmt.Errorf("%w: run identity changed", ErrFederationIngestValidation)
		}
		if sameCronRunEvidence(existing, run) {
			// A replica may have advanced its local CAS revision when a winning
			// peer observation replaced different evidence at the same source
			// revision. Keep that token across idempotent materialization.
			if run.Revision < existing.Revision {
				run.Revision = existing.Revision
			}
		} else {
			// Source revisions are not globally unique: two peers can both
			// replace revision 1 with different revision-2 evidence. When the
			// later winning observation changes this row, advance the local CAS
			// token even when its source revision has not advanced.
			if existing.Revision == 1<<63-1 {
				return fmt.Errorf("run revision exhausted")
			}
			if run.Revision <= existing.Revision {
				run.Revision = existing.Revision + 1
			}
		}
		if sameCronRunEvidence(existing, run) && existing.Revision == run.Revision && existing.UpdatedAt.Equal(run.UpdatedAt) {
			continue
		}
		summary, err := json.Marshal(run.Summary)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE cron_runs SET status=$1,summary_json=$2,revision=$3,started_at=$4,ended_at=$5,updated_at=$6 WHERE uid=$7 AND project_id=$8`, run.Status, string(summary), run.Revision, cronTimeValue(run.StartedAt), cronTimeValue(run.EndedAt), run.UpdatedAt.UTC().Format(time.RFC3339Nano), run.UID, projectID)
		if err != nil {
			return err
		}
	}
	return nil
}
