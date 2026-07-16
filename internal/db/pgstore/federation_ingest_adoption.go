package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

type federationIngestAdoptionSnapshotAuthorState struct {
	allowAuthorPreservation      bool
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
	state := federationIngestAdoptionSnapshotAuthorState{}
	if enrollmentID <= 0 {
		return state, nil
	}
	shape := federationIngestAdoptionBaselineShape(events)
	if !allowExplicit {
		return computeFederationIngestConsumedAdoptionBaselineRetryState(
			adoptionBaseline, adoptionBaselineEndSourceEventID, shape)
	}
	marker, err := federationIngestAdoptionSnapshotAuthorMarkerState(
		ctx, tx, projectID, enrollmentID, spokeInstanceUID,
	)
	if err != nil {
		return state, err
	}
	if !marker.allowSnapshotAuthors && !marker.baselineOpen {
		return computeFederationIngestConsumedAdoptionBaselineRetryState(
			adoptionBaseline, adoptionBaselineEndSourceEventID, shape)
	}
	if marker.baselineOpen && adoptionBaseline == "" {
		return state, fmt.Errorf("%w: adoption baseline continuation is open and requires adoption_baseline marker",
			db.ErrFederationIngestValidation)
	}
	if !shape.valid {
		if adoptionBaseline != "" || marker.baselineOpen {
			return state, fmt.Errorf("%w: adoption baseline chunk contains non-baseline event",
				db.ErrFederationIngestValidation)
		}
		return state, nil
	}
	switch adoptionBaseline {
	case db.FederationAdoptionBaselineOpen:
		return computeFederationIngestOpenAdoptionBaselineState(ctx, tx,
			projectID, spokeInstanceUID, marker, shape, adoptionBaselineEndSourceEventID)
	case db.FederationAdoptionBaselineComplete:
		return computeFederationIngestCompleteAdoptionBaselineState(ctx, tx,
			projectID, spokeInstanceUID, marker, shape, adoptionBaselineEndSourceEventID)
	}
	prior, err := federationIngestHasPriorEvents(ctx, tx, projectID, spokeInstanceUID)
	if err != nil {
		return state, err
	}
	if !prior {
		state.allowAuthorPreservation = true
	}
	return state, nil
}

func computeFederationIngestConsumedAdoptionBaselineRetryState(
	adoptionBaseline string,
	adoptionBaselineEndSourceEventID int64,
	shape federationIngestBaselineShape,
) (federationIngestAdoptionSnapshotAuthorState, error) {
	state := federationIngestAdoptionSnapshotAuthorState{}
	switch adoptionBaseline {
	case "":
		return state, nil
	case db.FederationAdoptionBaselineOpen, db.FederationAdoptionBaselineComplete:
	default:
		return state, nil
	}
	if !shape.valid {
		return state, fmt.Errorf("%w: adoption baseline retry contains non-baseline event",
			db.ErrFederationIngestValidation)
	}
	nonTerminal := adoptionBaseline == db.FederationAdoptionBaselineOpen
	if err := validateFederationIngestAdoptionBaselineCursor(
		federationIngestAdoptionMarkerState{}, shape,
		adoptionBaselineEndSourceEventID, nonTerminal,
	); err != nil {
		return state, err
	}
	state.overrideSnapshotAuthors = shape.hasSnapshot
	state.duplicateOnly = true
	return state, nil
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
	shape federationIngestBaselineShape,
	endSourceEventID int64,
) (federationIngestAdoptionSnapshotAuthorState, error) {
	state := federationIngestAdoptionSnapshotAuthorState{
		allowAuthorPreservation:      shape.hasSnapshot && marker.allowSnapshotAuthors,
		shouldDeferMarker:            true,
		deferAuthorPreservationGrant: marker.allowSnapshotAuthors && !shape.hasSnapshot,
		overrideSnapshotAuthors:      shape.hasSnapshot && marker.baselineOpen && !marker.allowSnapshotAuthors,
		nextSourceEventID:            shape.maxSourceEventID + 1,
		endSourceEventID:             endSourceEventID,
	}
	if err := validateFederationIngestAdoptionBaselineCursor(marker, shape, endSourceEventID, true); err != nil {
		return federationIngestAdoptionSnapshotAuthorState{}, err
	}
	if marker.baselineOpen {
		if err := validateFederationIngestAdoptionBaselineBoundary(
			ctx, tx, projectID, spokeInstanceUID, shape.events,
		); err != nil {
			return federationIngestAdoptionSnapshotAuthorState{}, err
		}
		if shape.minSourceEventID < marker.nextSourceEventID {
			state.duplicateOnly = true
		}
		return state, nil
	}
	prior, err := federationIngestHasPriorEvents(ctx, tx, projectID, spokeInstanceUID)
	if err != nil {
		return federationIngestAdoptionSnapshotAuthorState{}, err
	}
	if prior {
		return federationIngestAdoptionSnapshotAuthorState{}, fmt.Errorf(
			"%w: adoption baseline open chunk has no recorded baseline continuation",
			db.ErrFederationIngestValidation,
		)
	}
	return state, nil
}

func computeFederationIngestCompleteAdoptionBaselineState(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	spokeInstanceUID string,
	marker federationIngestAdoptionMarkerState,
	shape federationIngestBaselineShape,
	endSourceEventID int64,
) (federationIngestAdoptionSnapshotAuthorState, error) {
	state := federationIngestAdoptionSnapshotAuthorState{
		allowAuthorPreservation: shape.hasSnapshot && marker.allowSnapshotAuthors,
		overrideSnapshotAuthors: shape.hasSnapshot && marker.baselineOpen && !marker.allowSnapshotAuthors,
		endSourceEventID:        endSourceEventID,
	}
	if err := validateFederationIngestAdoptionBaselineCursor(marker, shape, endSourceEventID, false); err != nil {
		return federationIngestAdoptionSnapshotAuthorState{}, err
	}
	if marker.baselineOpen {
		if err := validateFederationIngestAdoptionBaselineBoundary(
			ctx, tx, projectID, spokeInstanceUID, shape.events,
		); err != nil {
			return federationIngestAdoptionSnapshotAuthorState{}, err
		}
		if shape.minSourceEventID < marker.nextSourceEventID {
			state.duplicateOnly = true
		}
		return state, nil
	}
	prior, err := federationIngestHasPriorEvents(ctx, tx, projectID, spokeInstanceUID)
	if err != nil {
		return federationIngestAdoptionSnapshotAuthorState{}, err
	}
	if prior {
		return federationIngestAdoptionSnapshotAuthorState{}, fmt.Errorf(
			"%w: adoption baseline complete chunk has no recorded baseline continuation",
			db.ErrFederationIngestValidation,
		)
	}
	return state, nil
}

func validateFederationIngestAdoptionBaselineCursor(
	marker federationIngestAdoptionMarkerState,
	shape federationIngestBaselineShape,
	endSourceEventID int64,
	nonTerminal bool,
) error {
	if shape.minSourceEventID <= 0 {
		return nil
	}
	if endSourceEventID <= 0 {
		return fmt.Errorf("%w: adoption baseline terminal source event cursor is missing",
			db.ErrFederationIngestValidation)
	}
	if !shape.contiguousSourceEventIDs {
		return fmt.Errorf("%w: adoption baseline source event cursor is not contiguous",
			db.ErrFederationIngestValidation)
	}
	if shape.maxSourceEventID > endSourceEventID {
		return fmt.Errorf("%w: adoption baseline chunk exceeds terminal source event %d",
			db.ErrFederationIngestValidation, endSourceEventID)
	}
	if nonTerminal && shape.maxSourceEventID >= endSourceEventID {
		return fmt.Errorf("%w: adoption baseline open chunk reaches terminal source event %d",
			db.ErrFederationIngestValidation, endSourceEventID)
	}
	if !nonTerminal && shape.maxSourceEventID != endSourceEventID {
		return fmt.Errorf("%w: adoption baseline complete chunk ends at source event %d before terminal source event %d",
			db.ErrFederationIngestValidation, shape.maxSourceEventID, endSourceEventID)
	}
	if !marker.baselineOpen {
		return nil
	}
	if marker.nextSourceEventID <= 0 {
		return fmt.Errorf("%w: adoption baseline continuation cursor is missing",
			db.ErrFederationIngestValidation)
	}
	if marker.endSourceEventID > 0 && endSourceEventID != marker.endSourceEventID {
		return fmt.Errorf("%w: adoption baseline terminal source event %d does not match recorded terminal source event %d",
			db.ErrFederationIngestValidation, endSourceEventID, marker.endSourceEventID)
	}
	if shape.minSourceEventID <= marker.nextSourceEventID {
		return nil
	}
	stage := "complete"
	if nonTerminal {
		stage = "open"
	}
	return fmt.Errorf("%w: adoption baseline %s chunk starts at source event %d after expected %d",
		db.ErrFederationIngestValidation, stage, shape.minSourceEventID, marker.nextSourceEventID)
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
	for index, event := range events {
		if index == 0 || event.SourceEventID < shape.minSourceEventID {
			shape.minSourceEventID = event.SourceEventID
		}
		if index > 0 && event.SourceEventID != events[index-1].SourceEventID+1 {
			shape.contiguousSourceEventIDs = false
		}
		if event.SourceEventID > shape.maxSourceEventID {
			shape.maxSourceEventID = event.SourceEventID
		}
		shape.events = append(shape.events, event)
		switch event.Event.Type {
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
	var physicalMS, counter int64
	err := tx.QueryRowContext(ctx, `SELECT hlc_physical_ms,hlc_counter FROM events
WHERE project_id=$1 AND origin_instance_uid=$2
  AND type IN ('project.metadata_updated','issue.snapshot')
ORDER BY id ASC LIMIT 1`, projectID, spokeInstanceUID).Scan(&physicalMS, &counter)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: adoption baseline continuation has no recorded baseline boundary",
			db.ErrFederationIngestValidation)
	}
	if err != nil {
		return mapSQLError(err, nil)
	}
	for _, event := range events {
		if event.Event.HLCPhysicalMS != physicalMS || event.Event.HLCCounter != counter {
			return fmt.Errorf("%w: adoption baseline event %s is outside recorded baseline boundary",
				db.ErrFederationIngestValidation, event.Event.EventUID)
		}
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
	var state federationIngestAdoptionMarkerState
	var allow, open int
	err := tx.QueryRowContext(ctx, `SELECT allow_adoption_snapshot_authors,
adoption_baseline_open,adoption_baseline_next_source_event_id,
adoption_baseline_end_source_event_id FROM federation_enrollments
WHERE id=$1 AND spoke_instance_uid=$2 AND revoked_at IS NULL AND project_id=$3
FOR UPDATE`, enrollmentID, spokeInstanceUID, projectID).
		Scan(&allow, &open, &state.nextSourceEventID, &state.endSourceEventID)
	if errors.Is(err, sql.ErrNoRows) {
		return federationIngestAdoptionMarkerState{}, nil
	}
	if err != nil {
		return federationIngestAdoptionMarkerState{}, mapSQLError(err, nil)
	}
	state.allowSnapshotAuthors = allow != 0
	state.baselineOpen = open != 0
	return state, nil
}

func federationIngestHasPriorEvents(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	spokeInstanceUID string,
) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events
WHERE project_id=$1 AND origin_instance_uid=$2)`, projectID, spokeInstanceUID).Scan(&exists)
	return exists, mapSQLError(err, nil)
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
	_, err := tx.ExecContext(ctx, `UPDATE federation_enrollments SET
allow_adoption_snapshot_authors=0,adoption_baseline_open=0,
adoption_baseline_next_source_event_id=0,adoption_baseline_end_source_event_id=0,
updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
WHERE id=$1 AND spoke_instance_uid=$2 AND revoked_at IS NULL AND project_id=$3`,
		enrollmentID, spokeInstanceUID, projectID)
	return mapSQLError(err, nil)
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
	allow := 0
	if deferAuthorPreservationGrant {
		allow = 1
	}
	_, err := tx.ExecContext(ctx, `UPDATE federation_enrollments SET
allow_adoption_snapshot_authors=$1,adoption_baseline_open=1,
adoption_baseline_next_source_event_id=$2,adoption_baseline_end_source_event_id=$3,
updated_at=to_char(now() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
WHERE id=$4 AND spoke_instance_uid=$5 AND revoked_at IS NULL AND project_id=$6`,
		allow, nextSourceEventID, endSourceEventID, enrollmentID, spokeInstanceUID, projectID)
	return mapSQLError(err, nil)
}

func federationEventHashMatches(
	event db.RemoteEvent,
	storedHash string,
	boundActor string,
	allowCanonicalSnapshotAuthors bool,
) (bool, error) {
	if storedHash == event.ContentHash {
		return true, nil
	}
	if !allowCanonicalSnapshotAuthors {
		return false, nil
	}
	canonical, err := db.CanonicalizeFederationSnapshotAuthors(event, boundActor)
	if err != nil {
		return false, err
	}
	return storedHash == canonical.ContentHash, nil
}
