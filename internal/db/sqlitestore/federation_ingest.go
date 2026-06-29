package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

type preparedFederationIngestEvent struct {
	SourceEventID int64
	Event         db.RemoteEvent
	Duplicate     bool
}

// IngestFederationEvents validates and stores a spoke push batch. The batch is
// all-or-nothing: any invalid event, conflicting duplicate, or materialization
// failure rolls back every insert from the batch.
func (d *Store) IngestFederationEvents(
	ctx context.Context,
	p db.FederationIngestParams,
) (db.FederationIngestResult, error) {
	var result db.FederationIngestResult
	err := d.RetryTransient(ctx, func() error {
		var err error
		result, err = d.ingestFederationEventsOnce(ctx, p)
		return err
	})
	return result, err
}

func (d *Store) ingestFederationEventsOnce(
	ctx context.Context,
	p db.FederationIngestParams,
) (db.FederationIngestResult, error) {
	if len(p.Events) == 0 {
		return db.FederationIngestResult{}, nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.FederationIngestResult{}, fmt.Errorf("begin federation ingest: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	projectUID, projectName, err := requireFederationIngestHub(ctx, tx, p.ProjectID)
	if err != nil {
		return db.FederationIngestResult{}, err
	}
	knownIssueUIDs, err := currentFederatedIssueUIDSet(ctx, tx, p.ProjectID)
	if err != nil {
		return db.FederationIngestResult{}, err
	}
	batchCreateSnapshotUIDs, err := federationIngestCreateSnapshotUIDSet(p.Events)
	if err != nil {
		return db.FederationIngestResult{}, err
	}
	adoptionSnapshotAuthorState, err := computeFederationIngestAdoptionSnapshotAuthorState(ctx, tx,
		p.ProjectID, p.FederationEnrollmentID, p.SpokeInstanceUID,
		p.AllowSnapshotAuthorPreservation, p.AdoptionBaseline, p.AdoptionBaselineEndSourceEventID, p.Events)
	if err != nil {
		return db.FederationIngestResult{}, err
	}
	prepared := make([]preparedFederationIngestEvent, 0, len(p.Events))
	result := db.FederationIngestResult{}
	seenBatch := map[string]string{}
	freshSnapshotSeen := false
	boundActor := strings.TrimSpace(p.BoundActor)
	for _, in := range p.Events {
		if in.SourceEventID <= 0 {
			return db.FederationIngestResult{}, fmt.Errorf("%w: source event id must be positive", db.ErrFederationIngestValidation)
		}
		if in.SourceEventID > result.PushCursorEventID {
			result.PushCursorEventID = in.SourceEventID
		}
		ev := in.Event
		if len(ev.Payload) == 0 {
			ev.Payload = json.RawMessage(`{}`)
		}
		if err := validateFederationProjectEvent(projectUID, p.SpokeInstanceUID, ev,
			knownIssueUIDs, batchCreateSnapshotUIDs, adoptionSnapshotAuthorState.allowFutureSnapshotLinks); err != nil {
			return db.FederationIngestResult{}, err
		}
		if boundActor != "" && ev.Actor != boundActor {
			return db.FederationIngestResult{}, fmt.Errorf("%w: event %s actor %q does not match bound actor",
				db.ErrFederationIngestValidation, ev.EventUID, ev.Actor)
		}
		if err := validateFederationEventHash(ev); err != nil {
			return db.FederationIngestResult{}, err
		}
		if existingHash, ok := seenBatch[ev.EventUID]; ok {
			matches, err := federationEventHashMatches(ev, existingHash, boundActor, adoptionSnapshotAuthorState.overrideSnapshotAuthors)
			if err != nil {
				return db.FederationIngestResult{}, err
			}
			if !matches {
				return db.FederationIngestResult{}, fmt.Errorf("%w: event %s", db.ErrRemoteEventConflict, ev.EventUID)
			}
			result.Duplicates++
			prepared = append(prepared, preparedFederationIngestEvent{
				SourceEventID: in.SourceEventID,
				Event:         ev,
				Duplicate:     true,
			})
			continue
		}
		existingHash, err := federationEventHashByUID(ctx, tx, ev.EventUID)
		if err == nil {
			matches, err := federationEventHashMatches(ev, existingHash, boundActor, adoptionSnapshotAuthorState.overrideSnapshotAuthors)
			if err != nil {
				return db.FederationIngestResult{}, err
			}
			if !matches {
				return db.FederationIngestResult{}, fmt.Errorf("%w: event %s", db.ErrRemoteEventConflict, ev.EventUID)
			}
			result.Duplicates++
			rememberIngestIssueUIDs(ev, knownIssueUIDs)
			prepared = append(prepared, preparedFederationIngestEvent{
				SourceEventID: in.SourceEventID,
				Event:         ev,
				Duplicate:     true,
			})
			continue
		}
		if !errors.Is(err, db.ErrNotFound) {
			return db.FederationIngestResult{}, err
		}
		if adoptionSnapshotAuthorState.duplicateOnly {
			return db.FederationIngestResult{}, fmt.Errorf("%w: adoption baseline retry contains fresh event %s",
				db.ErrFederationIngestValidation, ev.EventUID)
		}
		if adoptionSnapshotAuthorState.overrideSnapshotAuthors {
			var err error
			ev, err = canonicalizeFederationSnapshotAuthors(ev, boundActor)
			if err != nil {
				return db.FederationIngestResult{}, err
			}
		}
		if err := validateFederationBoundActorPayload(ev, boundActor,
			adoptionSnapshotAuthorState.allowAuthorPreservation); err != nil {
			return db.FederationIngestResult{}, err
		}
		if freshSnapshotSeen && ev.Type != "issue.snapshot" {
			return db.FederationIngestResult{}, fmt.Errorf("%w: non-snapshot event %s follows snapshot baseline in same batch",
				db.ErrFederationIngestValidation, ev.EventUID)
		}
		if err := rejectFreshCreateSnapshotForKnownIssue(ev, knownIssueUIDs); err != nil {
			return db.FederationIngestResult{}, err
		}
		if ev.Type == "issue.snapshot" {
			freshSnapshotSeen = true
		}
		seenBatch[ev.EventUID] = ev.ContentHash
		rememberIngestIssueUIDs(ev, knownIssueUIDs)
		prepared = append(prepared, preparedFederationIngestEvent{
			SourceEventID: in.SourceEventID,
			Event:         ev,
		})
	}

	for _, in := range prepared {
		if in.Duplicate {
			continue
		}
		ev := in.Event
		inserted, err := insertFederationEventTx(ctx, tx, p.ProjectID, projectName, ev)
		if err != nil {
			return db.FederationIngestResult{}, err
		}
		if !inserted {
			result.Duplicates++
			continue
		}
		// claim.violated is best-effort audit metadata evaluated against
		// current hub claim state at ingest time. It is not a causally precise
		// historical authorization judgment for offline work.
		auditEvents, err := d.annotateFederationIngestClaimWorkTx(ctx, tx, p.ProjectID, projectName, ev)
		if err != nil {
			return db.FederationIngestResult{}, err
		}
		result.Accepted++
		result.InsertedEventUIDs = append(result.InsertedEventUIDs, ev.EventUID)
		for _, auditEvent := range auditEvents {
			result.InsertedEventUIDs = append(result.InsertedEventUIDs, auditEvent.UID)
		}
	}
	if result.Accepted > 0 {
		if err := d.materializeFederatedProjectTx(ctx, tx, p.ProjectID); err != nil {
			return db.FederationIngestResult{}, err
		}
		if adoptionSnapshotAuthorState.verifySnapshotLinks {
			if err := validateFederationAdoptionSnapshotLinksResolved(ctx, tx, p.ProjectID, p.SpokeInstanceUID); err != nil {
				return db.FederationIngestResult{}, err
			}
		}
		if !adoptionSnapshotAuthorState.shouldDeferMarker {
			if err := consumeFederationAdoptionSnapshotAuthorMarker(ctx, tx,
				p.ProjectID, p.FederationEnrollmentID, p.SpokeInstanceUID); err != nil {
				return db.FederationIngestResult{}, err
			}
		} else if err := recordFederationAdoptionBaselineProgress(ctx, tx,
			p.ProjectID, p.FederationEnrollmentID, p.SpokeInstanceUID,
			adoptionSnapshotAuthorState.nextSourceEventID,
			adoptionSnapshotAuthorState.endSourceEventID,
			adoptionSnapshotAuthorState.deferAuthorPreservationGrant); err != nil {
			return db.FederationIngestResult{}, err
		}
	}
	if err := federationFailpoint("before_federation_ingest_commit"); err != nil {
		return db.FederationIngestResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return db.FederationIngestResult{}, fmt.Errorf("commit federation ingest: %w", err)
	}
	return result, nil
}

func validateFederationBoundActorPayload(
	ev db.RemoteEvent,
	boundActor string,
	allowSnapshotAuthorPreservation bool,
) error {
	boundActor = strings.TrimSpace(boundActor)
	if boundActor == "" {
		return nil
	}
	switch ev.Type {
	case "issue.snapshot":
		if allowSnapshotAuthorPreservation {
			return nil
		}
		if err := validateFederationPayloadAuthor(ev, boundActor); err != nil {
			return err
		}
		if err := validateFederationPayloadCommentAuthors(ev, boundActor); err != nil {
			return err
		}
		return validateFederationPayloadLinkAuthors(ev, boundActor)
	case "issue.created":
		if err := validateFederationPayloadAuthor(ev, boundActor); err != nil {
			return err
		}
		if err := validateFederationPayloadCommentAuthors(ev, boundActor); err != nil {
			return err
		}
		return validateFederationPayloadLinkAuthors(ev, boundActor)
	case "issue.commented":
		return validateFederationPayloadAuthor(ev, boundActor)
	}
	return nil
}

type federationIngestAdoptionSnapshotAuthorState struct {
	allowAuthorPreservation      bool
	allowFutureSnapshotLinks     bool
	verifySnapshotLinks          bool
	shouldDeferMarker            bool
	deferAuthorPreservationGrant bool
	overrideSnapshotAuthors      bool
	duplicateOnly                bool
	nextSourceEventID            int64
	endSourceEventID             int64
}

func computeFederationIngestAdoptionSnapshotAuthorState(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	enrollmentID int64,
	spokeInstanceUID string,
	allowExplicit bool,
	adoptionBaseline string,
	adoptionBaselineEndSourceEventID int64,
	events []db.FederationIngestEvent,
) (federationIngestAdoptionSnapshotAuthorState, error) {
	// Adoption emits an initial baseline: optional project metadata followed by
	// issue.snapshot events that preserve historical issue/comment authors. That
	// exception must be explicitly attached to the enrollment token and is
	// consumed with the accepted baseline. Chunk-aware spokes mark non-terminal
	// baseline chunks so the hub can keep the one-time marker open until the
	// terminal chunk arrives.
	state := federationIngestAdoptionSnapshotAuthorState{}
	if enrollmentID <= 0 {
		return state, nil
	}
	baselineShape := federationIngestAdoptionBaselineShape(events)
	if !allowExplicit {
		return computeFederationIngestConsumedAdoptionBaselineRetryState(
			adoptionBaseline, adoptionBaselineEndSourceEventID, baselineShape)
	}
	marker, err := federationIngestAdoptionSnapshotAuthorMarkerState(ctx, tx,
		projectID, enrollmentID, spokeInstanceUID)
	if err != nil {
		return state, err
	}
	if !marker.allowSnapshotAuthors && !marker.baselineOpen {
		return computeFederationIngestConsumedAdoptionBaselineRetryState(
			adoptionBaseline, adoptionBaselineEndSourceEventID, baselineShape)
	}
	if marker.baselineOpen && adoptionBaseline == "" {
		return state, fmt.Errorf("%w: adoption baseline continuation is open and requires adoption_baseline marker",
			db.ErrFederationIngestValidation)
	}
	if !baselineShape.valid {
		if adoptionBaseline != "" || marker.baselineOpen {
			return state, fmt.Errorf("%w: adoption baseline chunk contains non-baseline event",
				db.ErrFederationIngestValidation)
		}
		return state, nil
	}
	switch adoptionBaseline {
	case db.FederationAdoptionBaselineOpen:
		return computeFederationIngestOpenAdoptionBaselineState(ctx, tx,
			projectID, spokeInstanceUID, marker, baselineShape, adoptionBaselineEndSourceEventID)
	case db.FederationAdoptionBaselineComplete:
		return computeFederationIngestCompleteAdoptionBaselineState(ctx, tx,
			projectID, spokeInstanceUID, marker, baselineShape, adoptionBaselineEndSourceEventID)
	}
	prior, err := federationIngestHasPriorEvents(ctx, tx, projectID, spokeInstanceUID)
	if err != nil {
		return state, err
	}
	if prior {
		return state, nil
	}

	state.allowAuthorPreservation = true
	return state, nil
}

func computeFederationIngestConsumedAdoptionBaselineRetryState(
	adoptionBaseline string,
	adoptionBaselineEndSourceEventID int64,
	baselineShape federationIngestBaselineShape,
) (federationIngestAdoptionSnapshotAuthorState, error) {
	state := federationIngestAdoptionSnapshotAuthorState{}
	switch adoptionBaseline {
	case "":
		return state, nil
	case db.FederationAdoptionBaselineOpen, db.FederationAdoptionBaselineComplete:
	default:
		return state, nil
	}
	if !baselineShape.valid {
		return state, fmt.Errorf("%w: adoption baseline retry contains non-baseline event",
			db.ErrFederationIngestValidation)
	}
	nonTerminal := adoptionBaseline == db.FederationAdoptionBaselineOpen
	if err := validateFederationIngestAdoptionBaselineCursor(federationIngestAdoptionMarkerState{},
		baselineShape, adoptionBaselineEndSourceEventID, nonTerminal); err != nil {
		return federationIngestAdoptionSnapshotAuthorState{}, err
	}
	return federationIngestAdoptionSnapshotAuthorState{
		allowFutureSnapshotLinks: true,
		overrideSnapshotAuthors:  baselineShape.hasSnapshot,
		duplicateOnly:            true,
	}, nil
}

type federationIngestAdoptionMarkerState struct {
	allowSnapshotAuthors bool
	baselineOpen         bool
	nextSourceEventID    int64
	endSourceEventID     int64
}

func computeFederationIngestOpenAdoptionBaselineState(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	spokeInstanceUID string,
	marker federationIngestAdoptionMarkerState,
	baselineShape federationIngestBaselineShape,
	adoptionBaselineEndSourceEventID int64,
) (federationIngestAdoptionSnapshotAuthorState, error) {
	state := federationIngestAdoptionSnapshotAuthorState{
		allowAuthorPreservation:      baselineShape.hasSnapshot && marker.allowSnapshotAuthors,
		allowFutureSnapshotLinks:     true,
		shouldDeferMarker:            true,
		deferAuthorPreservationGrant: marker.allowSnapshotAuthors && !baselineShape.hasSnapshot,
		overrideSnapshotAuthors:      baselineShape.hasSnapshot && marker.baselineOpen && !marker.allowSnapshotAuthors,
		nextSourceEventID:            baselineShape.maxSourceEventID + 1,
		endSourceEventID:             adoptionBaselineEndSourceEventID,
	}
	if err := validateFederationIngestAdoptionBaselineCursor(marker, baselineShape, adoptionBaselineEndSourceEventID, true); err != nil {
		return federationIngestAdoptionSnapshotAuthorState{}, err
	}
	if marker.baselineOpen {
		if err := validateFederationIngestAdoptionBaselineBoundary(ctx, tx, projectID, spokeInstanceUID, baselineShape.events); err != nil {
			return federationIngestAdoptionSnapshotAuthorState{}, err
		}
		if baselineShape.minSourceEventID < marker.nextSourceEventID {
			state.duplicateOnly = true
		}
		return state, nil
	}
	prior, err := federationIngestHasPriorEvents(ctx, tx, projectID, spokeInstanceUID)
	if err != nil {
		return federationIngestAdoptionSnapshotAuthorState{}, err
	}
	if prior {
		return federationIngestAdoptionSnapshotAuthorState{}, fmt.Errorf("%w: adoption baseline open chunk has no recorded baseline continuation",
			db.ErrFederationIngestValidation)
	}
	return state, nil
}

func computeFederationIngestCompleteAdoptionBaselineState(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	spokeInstanceUID string,
	marker federationIngestAdoptionMarkerState,
	baselineShape federationIngestBaselineShape,
	adoptionBaselineEndSourceEventID int64,
) (federationIngestAdoptionSnapshotAuthorState, error) {
	state := federationIngestAdoptionSnapshotAuthorState{
		allowAuthorPreservation: baselineShape.hasSnapshot && marker.allowSnapshotAuthors,
		verifySnapshotLinks:     baselineShape.hasSnapshot || marker.baselineOpen,
		overrideSnapshotAuthors: baselineShape.hasSnapshot && marker.baselineOpen && !marker.allowSnapshotAuthors,
		endSourceEventID:        adoptionBaselineEndSourceEventID,
	}
	if err := validateFederationIngestAdoptionBaselineCursor(marker, baselineShape, adoptionBaselineEndSourceEventID, false); err != nil {
		return federationIngestAdoptionSnapshotAuthorState{}, err
	}
	if marker.baselineOpen {
		if err := validateFederationIngestAdoptionBaselineBoundary(ctx, tx, projectID, spokeInstanceUID, baselineShape.events); err != nil {
			return federationIngestAdoptionSnapshotAuthorState{}, err
		}
		if baselineShape.minSourceEventID < marker.nextSourceEventID {
			state.duplicateOnly = true
			state.allowFutureSnapshotLinks = true
		}
		return state, nil
	}
	prior, err := federationIngestHasPriorEvents(ctx, tx, projectID, spokeInstanceUID)
	if err != nil {
		return federationIngestAdoptionSnapshotAuthorState{}, err
	}
	if prior {
		return federationIngestAdoptionSnapshotAuthorState{}, fmt.Errorf("%w: adoption baseline complete chunk has no recorded baseline continuation",
			db.ErrFederationIngestValidation)
	}
	return state, nil
}

func validateFederationIngestAdoptionBaselineCursor(
	marker federationIngestAdoptionMarkerState,
	baselineShape federationIngestBaselineShape,
	adoptionBaselineEndSourceEventID int64,
	nonTerminal bool,
) error {
	if baselineShape.minSourceEventID <= 0 {
		return nil
	}
	if adoptionBaselineEndSourceEventID <= 0 {
		return fmt.Errorf("%w: adoption baseline terminal source event cursor is missing",
			db.ErrFederationIngestValidation)
	}
	if !baselineShape.contiguousSourceEventIDs {
		return fmt.Errorf("%w: adoption baseline source event cursor is not contiguous",
			db.ErrFederationIngestValidation)
	}
	if baselineShape.maxSourceEventID > adoptionBaselineEndSourceEventID {
		return fmt.Errorf("%w: adoption baseline chunk exceeds terminal source event %d",
			db.ErrFederationIngestValidation, adoptionBaselineEndSourceEventID)
	}
	if nonTerminal && baselineShape.maxSourceEventID >= adoptionBaselineEndSourceEventID {
		return fmt.Errorf("%w: adoption baseline open chunk reaches terminal source event %d",
			db.ErrFederationIngestValidation, adoptionBaselineEndSourceEventID)
	}
	if !nonTerminal && baselineShape.maxSourceEventID != adoptionBaselineEndSourceEventID {
		return fmt.Errorf("%w: adoption baseline complete chunk ends at source event %d before terminal source event %d",
			db.ErrFederationIngestValidation, baselineShape.maxSourceEventID, adoptionBaselineEndSourceEventID)
	}
	if !marker.baselineOpen {
		return nil
	}
	if marker.nextSourceEventID <= 0 {
		return fmt.Errorf("%w: adoption baseline continuation cursor is missing",
			db.ErrFederationIngestValidation)
	}
	if marker.endSourceEventID > 0 && adoptionBaselineEndSourceEventID != marker.endSourceEventID {
		return fmt.Errorf("%w: adoption baseline terminal source event %d does not match recorded terminal source event %d",
			db.ErrFederationIngestValidation, adoptionBaselineEndSourceEventID, marker.endSourceEventID)
	}
	if baselineShape.minSourceEventID <= marker.nextSourceEventID {
		return nil
	}
	stage := "complete"
	if nonTerminal {
		stage = "open"
	}
	return fmt.Errorf("%w: adoption baseline %s chunk starts at source event %d after expected %d",
		db.ErrFederationIngestValidation, stage, baselineShape.minSourceEventID, marker.nextSourceEventID)
}

type federationIngestBaselineShape struct {
	valid                    bool
	hasSnapshot              bool
	contiguousSourceEventIDs bool
	minSourceEventID         int64
	maxSourceEventID         int64
	events                   []db.FederationIngestEvent
}

func federationIngestAdoptionBaselineShape(events []db.FederationIngestEvent) federationIngestBaselineShape {
	shape := federationIngestBaselineShape{valid: true, contiguousSourceEventIDs: true}
	for i, in := range events {
		if i == 0 || in.SourceEventID < shape.minSourceEventID {
			shape.minSourceEventID = in.SourceEventID
		}
		if i > 0 && in.SourceEventID != events[i-1].SourceEventID+1 {
			shape.contiguousSourceEventIDs = false
		}
		if in.SourceEventID > shape.maxSourceEventID {
			shape.maxSourceEventID = in.SourceEventID
		}
		shape.events = append(shape.events, in)
		switch in.Event.Type {
		case "project.metadata_updated":
			if shape.hasSnapshot {
				shape.valid = false
				return shape
			}
		case "issue.snapshot":
			shape.hasSnapshot = true
		default:
			shape.valid = false
			return shape
		}
	}
	return shape
}

func validateFederationIngestAdoptionBaselineBoundary(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	spokeInstanceUID string,
	events []db.FederationIngestEvent,
) error {
	var (
		hlcPhysicalMS int64
		hlcCounter    int64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT hlc_physical_ms, hlc_counter
		  FROM events
		 WHERE project_id = ?
		   AND origin_instance_uid = ?
		   AND type IN ('project.metadata_updated', 'issue.snapshot')
		 ORDER BY id ASC
		 LIMIT 1`,
		projectID, spokeInstanceUID).Scan(&hlcPhysicalMS, &hlcCounter)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: adoption baseline continuation has no recorded baseline boundary",
			db.ErrFederationIngestValidation)
	}
	if err != nil {
		return fmt.Errorf("lookup adoption baseline boundary: %w", err)
	}
	for _, in := range events {
		if in.Event.HLCPhysicalMS == hlcPhysicalMS && in.Event.HLCCounter == hlcCounter {
			continue
		}
		return fmt.Errorf("%w: adoption baseline event %s is outside recorded baseline boundary",
			db.ErrFederationIngestValidation, in.Event.EventUID)
	}
	return nil
}

func federationIngestAdoptionSnapshotAuthorMarkerState(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	enrollmentID int64,
	spokeInstanceUID string,
) (federationIngestAdoptionMarkerState, error) {
	var (
		state        federationIngestAdoptionMarkerState
		allow        int
		baselineOpen int
	)
	err := tx.QueryRowContext(ctx, `
			SELECT allow_adoption_snapshot_authors,
			       adoption_baseline_open,
			       adoption_baseline_next_source_event_id,
			       adoption_baseline_end_source_event_id
			  FROM federation_enrollments
			 WHERE id = ?
			   AND spoke_instance_uid = ?
			   AND revoked_at IS NULL
			   AND project_id = ?`,
		enrollmentID, spokeInstanceUID, projectID).Scan(
		&allow, &baselineOpen, &state.nextSourceEventID, &state.endSourceEventID)
	if errors.Is(err, sql.ErrNoRows) {
		return federationIngestAdoptionMarkerState{}, nil
	}
	if err != nil {
		return federationIngestAdoptionMarkerState{}, fmt.Errorf("lookup federation adoption snapshot author marker: %w", err)
	}
	state.allowSnapshotAuthors = allow != 0
	state.baselineOpen = baselineOpen != 0
	return state, nil
}

func federationIngestHasPriorEvents(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	spokeInstanceUID string,
) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `
		SELECT 1
		  FROM events
		 WHERE project_id = ?
		   AND origin_instance_uid = ?
		 LIMIT 1`,
		projectID, spokeInstanceUID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lookup prior federation ingest events: %w", err)
	}
	return true, nil
}

func consumeFederationAdoptionSnapshotAuthorMarker(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	enrollmentID int64,
	spokeInstanceUID string,
) error {
	if enrollmentID <= 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE federation_enrollments
		   SET allow_adoption_snapshot_authors = 0,
		       adoption_baseline_open = 0,
		       adoption_baseline_next_source_event_id = 0,
		       adoption_baseline_end_source_event_id = 0,
		       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ?
		   AND spoke_instance_uid = ?
		   AND revoked_at IS NULL
		   AND project_id = ?`,
		enrollmentID, spokeInstanceUID, projectID)
	if err != nil {
		return fmt.Errorf("consume federation adoption snapshot author marker: %w", err)
	}
	return nil
}

func recordFederationAdoptionBaselineProgress(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	enrollmentID int64,
	spokeInstanceUID string,
	nextSourceEventID int64,
	endSourceEventID int64,
	deferAuthorPreservationGrant bool,
) error {
	if enrollmentID <= 0 || nextSourceEventID <= 0 || endSourceEventID <= 0 {
		return nil
	}
	allowSnapshotAuthors := 0
	if deferAuthorPreservationGrant {
		allowSnapshotAuthors = 1
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE federation_enrollments
		   SET allow_adoption_snapshot_authors = ?,
		       adoption_baseline_open = 1,
		       adoption_baseline_next_source_event_id = ?,
		       adoption_baseline_end_source_event_id = ?,
		       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ?
		   AND spoke_instance_uid = ?
		   AND revoked_at IS NULL
		   AND project_id = ?`,
		allowSnapshotAuthors, nextSourceEventID, endSourceEventID, enrollmentID, spokeInstanceUID, projectID)
	if err != nil {
		return fmt.Errorf("record federation adoption baseline progress: %w", err)
	}
	return nil
}

func federationEventHashMatches(
	ev db.RemoteEvent,
	storedHash string,
	boundActor string,
	allowCanonicalSnapshotAuthors bool,
) (bool, error) {
	if storedHash == ev.ContentHash {
		return true, nil
	}
	if !allowCanonicalSnapshotAuthors {
		return false, nil
	}
	canonical, err := canonicalizeFederationSnapshotAuthors(ev, boundActor)
	if err != nil {
		return false, err
	}
	return storedHash == canonical.ContentHash, nil
}

func canonicalizeFederationSnapshotAuthors(ev db.RemoteEvent, boundActor string) (db.RemoteEvent, error) {
	boundActor = strings.TrimSpace(boundActor)
	if ev.Type != "issue.snapshot" || boundActor == "" {
		return ev, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return db.RemoteEvent{}, fmt.Errorf("%w: event %s issue.snapshot payload is invalid JSON",
			db.ErrFederationIngestValidation, ev.EventUID)
	}
	if payload == nil {
		return db.RemoteEvent{}, fmt.Errorf("%w: event %s issue.snapshot payload must be a JSON object",
			db.ErrFederationIngestValidation, ev.EventUID)
	}
	actorJSON, err := json.Marshal(boundActor)
	if err != nil {
		return db.RemoteEvent{}, fmt.Errorf("marshal federation snapshot author: %w", err)
	}
	payload["author"] = actorJSON
	if raw, ok := payload["comments"]; ok && len(raw) > 0 && string(raw) != "null" {
		var comments []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &comments); err != nil {
			return db.RemoteEvent{}, fmt.Errorf("%w: event %s issue.snapshot comments payload is invalid JSON",
				db.ErrFederationIngestValidation, ev.EventUID)
		}
		for i := range comments {
			if comments[i] == nil {
				return db.RemoteEvent{}, fmt.Errorf("%w: event %s issue.snapshot comments payload entry must be a JSON object",
					db.ErrFederationIngestValidation, ev.EventUID)
			}
			comments[i]["author"] = actorJSON
		}
		normalized, err := json.Marshal(comments)
		if err != nil {
			return db.RemoteEvent{}, fmt.Errorf("marshal federation snapshot comments: %w", err)
		}
		payload["comments"] = normalized
	}
	if raw, ok := payload["links"]; ok && len(raw) > 0 && string(raw) != "null" {
		var links []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &links); err != nil {
			return db.RemoteEvent{}, fmt.Errorf("%w: event %s issue.snapshot links payload is invalid JSON",
				db.ErrFederationIngestValidation, ev.EventUID)
		}
		for i := range links {
			if links[i] == nil {
				return db.RemoteEvent{}, fmt.Errorf("%w: event %s issue.snapshot links payload entry must be a JSON object",
					db.ErrFederationIngestValidation, ev.EventUID)
			}
			links[i]["author"] = actorJSON
		}
		normalized, err := json.Marshal(links)
		if err != nil {
			return db.RemoteEvent{}, fmt.Errorf("marshal federation snapshot links: %w", err)
		}
		payload["links"] = normalized
	}
	normalizedPayload, err := json.Marshal(payload)
	if err != nil {
		return db.RemoteEvent{}, fmt.Errorf("marshal federation snapshot payload: %w", err)
	}
	ev.Payload = normalizedPayload
	hash, err := db.EventContentHash(db.EventHashInput{
		UID:               ev.EventUID,
		OriginInstanceUID: ev.OriginInstanceUID,
		ProjectUID:        ev.ProjectUID,
		ProjectName:       ev.ProjectName,
		IssueUID:          ev.IssueUID,
		RelatedIssueUID:   ev.RelatedIssueUID,
		Type:              ev.Type,
		Actor:             ev.Actor,
		HLCPhysicalMS:     ev.HLCPhysicalMS,
		HLCCounter:        ev.HLCCounter,
		CreatedAt:         ev.CreatedAt.UTC().Format(sqliteTimeFormat),
		Payload:           ev.Payload,
	})
	if err != nil {
		return db.RemoteEvent{}, fmt.Errorf("hash canonical federation snapshot: %w", err)
	}
	ev.ContentHash = hash
	return ev, nil
}

func validateFederationPayloadAuthor(ev db.RemoteEvent, boundActor string) error {
	payload := db.PayloadMap(ev.Payload)
	author, ok := db.StringValue(payload["author"])
	if !ok || strings.TrimSpace(author) != boundActor {
		return fmt.Errorf("%w: event %s %s payload author %q does not match bound actor",
			db.ErrFederationIngestValidation, ev.EventUID, ev.Type, author)
	}
	return nil
}

func validateFederationPayloadCommentAuthors(ev db.RemoteEvent, boundActor string) error {
	var payload struct {
		Comments []struct {
			Author string `json:"author"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return fmt.Errorf("%w: event %s %s payload is invalid JSON",
			db.ErrFederationIngestValidation, ev.EventUID, ev.Type)
	}
	for _, comment := range payload.Comments {
		if strings.TrimSpace(comment.Author) != boundActor {
			return fmt.Errorf("%w: event %s %s comment payload author %q does not match bound actor",
				db.ErrFederationIngestValidation, ev.EventUID, ev.Type, comment.Author)
		}
	}
	return nil
}

func validateFederationPayloadLinkAuthors(ev db.RemoteEvent, boundActor string) error {
	var payload struct {
		Links []struct {
			Author string `json:"author"`
		} `json:"links"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return fmt.Errorf("%w: event %s %s payload is invalid JSON",
			db.ErrFederationIngestValidation, ev.EventUID, ev.Type)
	}
	for _, link := range payload.Links {
		author := strings.TrimSpace(link.Author)
		if author == "" {
			continue
		}
		if author != boundActor {
			return fmt.Errorf("%w: event %s %s link payload author %q does not match bound actor",
				db.ErrFederationIngestValidation, ev.EventUID, ev.Type, link.Author)
		}
	}
	return nil
}

func insertFederationEventTx(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	projectName string,
	ev db.RemoteEvent,
) (bool, error) {
	storedProjectName := ev.ProjectName
	if storedProjectName == "" {
		storedProjectName = projectName
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO events(
		   uid, origin_instance_uid, project_id, project_name,
		   issue_id, issue_uid, related_issue_id, related_issue_uid,
		   type, actor, payload, hlc_physical_ms, hlc_counter, content_hash, created_at
		 )
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(uid) DO NOTHING`,
		ev.EventUID, ev.OriginInstanceUID,
		projectID, storedProjectName,
		nil, stringPtrValue(ev.IssueUID),
		nil, stringPtrValue(ev.RelatedIssueUID),
		ev.Type, ev.Actor, string(ev.Payload),
		ev.HLCPhysicalMS, ev.HLCCounter, ev.ContentHash,
		ev.CreatedAt.UTC().Format(sqliteTimeFormat))
	if err != nil {
		return false, fmt.Errorf("insert federation event: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("insert federation event rows affected: %w", err)
	}
	if affected > 0 {
		return true, nil
	}
	existingHash, err := federationEventHashByUID(ctx, tx, ev.EventUID)
	if err != nil {
		return false, err
	}
	if existingHash != ev.ContentHash {
		return false, fmt.Errorf("%w: event %s", db.ErrRemoteEventConflict, ev.EventUID)
	}
	return false, nil
}

func requireFederationIngestHub(ctx context.Context, tx *sql.Tx, projectID int64) (string, string, error) {
	var projectUID, projectName, role string
	var enabled int
	err := tx.QueryRowContext(ctx, `
		SELECT p.uid, p.name, fb.role, fb.enabled
		  FROM projects p
		  JOIN federation_bindings fb ON fb.project_id = p.id
		 WHERE p.id = ? AND p.deleted_at IS NULL`, projectID).
		Scan(&projectUID, &projectName, &role, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", db.ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("lookup federation ingest hub: %w", err)
	}
	if role != string(db.FederationRoleHub) || enabled != 1 {
		return "", "", fmt.Errorf("%w: project is not an enabled federation hub", db.ErrFederationIngestValidation)
	}
	return projectUID, projectName, nil
}

func validateFederationEventHash(ev db.RemoteEvent) error {
	expectedHash, err := db.EventContentHash(db.EventHashInput{
		UID:               ev.EventUID,
		OriginInstanceUID: ev.OriginInstanceUID,
		ProjectUID:        ev.ProjectUID,
		ProjectName:       ev.ProjectName,
		IssueUID:          ev.IssueUID,
		RelatedIssueUID:   ev.RelatedIssueUID,
		Type:              ev.Type,
		Actor:             ev.Actor,
		HLCPhysicalMS:     ev.HLCPhysicalMS,
		HLCCounter:        ev.HLCCounter,
		CreatedAt:         ev.CreatedAt.UTC().Format(sqliteTimeFormat),
		Payload:           ev.Payload,
	})
	if err != nil {
		return fmt.Errorf("federation event content hash: %w", err)
	}
	if !strings.EqualFold(expectedHash, ev.ContentHash) {
		return fmt.Errorf("%w: event %s", db.ErrRemoteEventHashMismatch, ev.EventUID)
	}
	return nil
}

func federationEventHashByUID(ctx context.Context, tx *sql.Tx, eventUID string) (string, error) {
	var hash string
	err := tx.QueryRowContext(ctx,
		`SELECT content_hash FROM events WHERE uid = ?`, eventUID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", db.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lookup federation event duplicate: %w", err)
	}
	return hash, nil
}

func validateFederationProjectEvent(
	projectUID, spokeInstanceUID string,
	ev db.RemoteEvent,
	knownIssueUIDs map[string]struct{},
	batchCreateSnapshotUIDs map[string]struct{},
	allowFutureSnapshotLinks bool,
) error {
	if ev.ProjectUID != projectUID {
		return fmt.Errorf("%w: event %s targets project %s", db.ErrFederationIngestValidation, ev.EventUID, ev.ProjectUID)
	}
	if ev.OriginInstanceUID != spokeInstanceUID {
		return fmt.Errorf("%w: event %s origin mismatch", db.ErrFederationIngestValidation, ev.EventUID)
	}
	if ev.EventUID == "" || ev.HLCPhysicalMS <= 0 || ev.HLCCounter < 0 || strings.TrimSpace(ev.Actor) == "" {
		return fmt.Errorf("%w: event %s has invalid envelope", db.ErrFederationIngestValidation, ev.EventUID)
	}
	if strings.HasPrefix(ev.Type, "recurrence.") || ev.Type == "issue.moved" {
		return fmt.Errorf("%w: event type %s unsupported in phase 2", db.ErrFederationIngestValidation, ev.Type)
	}
	payload := db.PayloadMap(ev.Payload)
	if ev.Type == "project.metadata_updated" {
		if payloadProjectUID, ok := db.StringValue(payload["project_uid"]); ok && payloadProjectUID != projectUID {
			return fmt.Errorf("%w: project metadata payload targets %s", db.ErrFederationIngestValidation, payloadProjectUID)
		}
		return nil
	}
	issueUID, err := payloadIssueUID(ev, payload)
	if err != nil {
		return err
	}
	switch ev.Type {
	case "issue.created", "issue.snapshot":
		if issueUID == "" {
			return fmt.Errorf("%w: %s missing issue uid", db.ErrFederationIngestValidation, ev.Type)
		}
	case "issue.updated", "issue.assigned", "issue.unassigned",
		"issue.priority_set", "issue.priority_cleared",
		"issue.closed", "issue.reopened", "issue.soft_deleted", "issue.restored",
		"issue.commented", "issue.comment_edited", "issue.labeled", "issue.unlabeled",
		"issue.linked", "issue.unlinked", "issue.links_changed", "issue.metadata_updated":
		if issueUID == "" {
			return fmt.Errorf("%w: %s missing issue uid", db.ErrFederationIngestValidation, ev.Type)
		}
		if _, ok := knownIssueUIDs[issueUID]; !ok {
			return fmt.Errorf("%w: %s references unknown issue %s", db.ErrFederationIngestValidation, ev.Type, issueUID)
		}
	default:
		return fmt.Errorf("%w: unsupported event type %s", db.ErrFederationIngestValidation, ev.Type)
	}
	snapshotLinkRefs := map[string]struct{}{}
	deferredSnapshotLinks := map[string]struct{}{}
	if ev.Type == "issue.snapshot" {
		for _, ref := range payloadLinkIssueUIDs(ev) {
			snapshotLinkRefs[ref] = struct{}{}
			if _, ok := batchCreateSnapshotUIDs[ref]; ok {
				deferredSnapshotLinks[ref] = struct{}{}
			}
		}
	}
	for _, ref := range payloadReferencedIssueUIDs(ev, payload) {
		if ref == issueUID {
			continue
		}
		if _, ok := knownIssueUIDs[ref]; !ok {
			if _, deferred := deferredSnapshotLinks[ref]; deferred {
				continue
			}
			if allowFutureSnapshotLinks && ev.Type == "issue.snapshot" {
				if _, ok := snapshotLinkRefs[ref]; !ok {
					return fmt.Errorf("%w: event %s references unknown issue %s", db.ErrFederationIngestValidation, ev.EventUID, ref)
				}
				continue
			}
			return fmt.Errorf("%w: event %s references unknown issue %s", db.ErrFederationIngestValidation, ev.EventUID, ref)
		}
	}
	return nil
}

func federationIngestCreateSnapshotUIDSet(events []db.FederationIngestEvent) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for _, in := range events {
		ev := in.Event
		if len(ev.Payload) == 0 {
			ev.Payload = json.RawMessage(`{}`)
		}
		switch ev.Type {
		case "issue.created", "issue.snapshot":
		default:
			continue
		}
		uid, err := payloadIssueUID(ev, db.PayloadMap(ev.Payload))
		if err != nil {
			return nil, err
		}
		if uid != "" {
			out[uid] = struct{}{}
		}
	}
	return out, nil
}

func rejectFreshCreateSnapshotForKnownIssue(ev db.RemoteEvent, knownIssueUIDs map[string]struct{}) error {
	switch ev.Type {
	case "issue.created", "issue.snapshot":
	default:
		return nil
	}
	issueUID, err := payloadIssueUID(ev, db.PayloadMap(ev.Payload))
	if err != nil {
		return err
	}
	if _, ok := knownIssueUIDs[issueUID]; ok {
		return fmt.Errorf("%w: fresh %s targets existing issue %s",
			db.ErrFederationIngestValidation, ev.Type, issueUID)
	}
	return nil
}

func payloadIssueUID(ev db.RemoteEvent, payload map[string]json.RawMessage) (string, error) {
	var payloadUID string
	if uid, ok := db.StringValue(payload["issue_uid"]); ok {
		payloadUID = uid
	}
	if uid, ok := db.StringValue(payload["uid"]); ok {
		if payloadUID != "" && payloadUID != uid {
			return "", fmt.Errorf("%w: payload issue uid disagreement", db.ErrFederationIngestValidation)
		}
		payloadUID = uid
	}
	if ev.IssueUID != nil {
		if payloadUID != "" && payloadUID != *ev.IssueUID {
			return "", fmt.Errorf("%w: envelope/payload issue uid disagreement", db.ErrFederationIngestValidation)
		}
		return *ev.IssueUID, nil
	}
	return payloadUID, nil
}

func payloadReferencedIssueUIDs(ev db.RemoteEvent, payload map[string]json.RawMessage) []string {
	var refs []string
	if ev.RelatedIssueUID != nil && *ev.RelatedIssueUID != "" {
		refs = append(refs, *ev.RelatedIssueUID)
	}
	for _, key := range []string{
		"from_uid", "to_uid", "from_issue_uid", "to_issue_uid",
		"parent_set_uid", "parent_removed_uid",
	} {
		if uid, ok := db.StringValue(payload[key]); ok {
			refs = append(refs, uid)
		}
	}
	for _, key := range []string{
		"blocks_added_uids", "blocks_removed_uids",
		"blocked_by_added_uids", "blocked_by_removed_uids",
		"related_added_uids", "related_removed_uids",
	} {
		refs = append(refs, db.StringSlice(payload[key])...)
	}
	refs = append(refs, payloadLinkIssueUIDs(ev)...)
	return refs
}

func payloadLinkIssueUIDs(ev db.RemoteEvent) []string {
	var created struct {
		Links []struct {
			ToIssueUID string `json:"to_issue_uid"`
		} `json:"links"`
	}
	_ = json.Unmarshal(ev.Payload, &created)
	var refs []string
	for _, link := range created.Links {
		if link.ToIssueUID != "" {
			refs = append(refs, link.ToIssueUID)
		}
	}
	return refs
}

func validateFederationAdoptionSnapshotLinksResolved(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	spokeInstanceUID string,
) error {
	knownIssueUIDs, err := materializedIssueUIDSet(ctx, tx, projectID)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT uid, payload
		  FROM events
		 WHERE project_id = ?
		   AND origin_instance_uid = ?
		   AND type = 'issue.snapshot'
		 ORDER BY id ASC`,
		projectID, spokeInstanceUID)
	if err != nil {
		return fmt.Errorf("list adoption snapshot links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			eventUID string
			payload  string
		)
		if err := rows.Scan(&eventUID, &payload); err != nil {
			return fmt.Errorf("scan adoption snapshot link: %w", err)
		}
		for _, ref := range payloadLinkIssueUIDs(db.RemoteEvent{Payload: json.RawMessage(payload)}) {
			if _, ok := knownIssueUIDs[ref]; ok {
				continue
			}
			return fmt.Errorf("%w: adoption baseline unresolved snapshot link %s references unknown issue %s",
				db.ErrFederationIngestValidation, eventUID, ref)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate adoption snapshot links: %w", err)
	}
	return nil
}

func currentFederatedIssueUIDSet(ctx context.Context, tx *sql.Tx, projectID int64) (map[string]struct{}, error) {
	out, err := materializedIssueUIDSet(ctx, tx, projectID)
	if err != nil {
		return nil, err
	}
	eventRows, err := tx.QueryContext(ctx, `
		SELECT issue_uid FROM events WHERE project_id = ? AND issue_uid IS NOT NULL
		UNION
		SELECT related_issue_uid FROM events WHERE project_id = ? AND related_issue_uid IS NOT NULL`,
		projectID, projectID)
	if err != nil {
		return nil, fmt.Errorf("list event issue uids: %w", err)
	}
	defer func() { _ = eventRows.Close() }()
	for eventRows.Next() {
		var uid string
		if err := eventRows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("scan event issue uid: %w", err)
		}
		out[uid] = struct{}{}
	}
	return out, eventRows.Err()
}

func materializedIssueUIDSet(ctx context.Context, tx *sql.Tx, projectID int64) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	rows, err := tx.QueryContext(ctx, `SELECT uid FROM issues WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list current issue uids: %w", err)
	}
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan current issue uid: %w", err)
		}
		out[uid] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close current issue uids: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate current issue uids: %w", err)
	}
	return out, nil
}

func rememberIngestIssueUIDs(ev db.RemoteEvent, known map[string]struct{}) {
	payload := db.PayloadMap(ev.Payload)
	if uid, err := payloadIssueUID(ev, payload); err == nil && uid != "" {
		switch ev.Type {
		case "issue.created", "issue.snapshot":
			known[uid] = struct{}{}
		}
	}
}
