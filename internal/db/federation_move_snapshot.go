package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
)

// FederationMoveSnapshotEvent exposes a move's document to the existing issue
// validators without modifying the retained event or its hash.
func FederationMoveSnapshotEvent(event RemoteEvent) RemoteEvent {
	if event.Type == "issue.moved" {
		event.Type = "issue.snapshot"
		event.Payload = PayloadMap(event.Payload)["snapshot"]
	}
	return event
}

// FederationMoveSnapshotValidator checks attribution only when a fresh pushed
// snapshot can supply the fold's fallback baseline. Retained and staged real
// baselines, and earlier move snapshots, preserve their historical attribution.
type FederationMoveSnapshotValidator struct {
	tx                 *sql.Tx
	snapshotExpression string
	baselines          map[string]bool
	firstMoves         map[string]FoldClock
	loaded             map[string]bool
}

// NewFederationMoveSnapshotValidator creates request-local baseline lookups.
func NewFederationMoveSnapshotValidator(tx *sql.Tx, snapshotExpression string, events []FederationIngestEvent) *FederationMoveSnapshotValidator {
	v := &FederationMoveSnapshotValidator{tx: tx, snapshotExpression: snapshotExpression, baselines: map[string]bool{}, firstMoves: map[string]FoldClock{}, loaded: map[string]bool{}}
	// FoldEvents discovers every real baseline before choosing a fallback,
	// regardless of delivery order or the events' clocks.
	for _, input := range events {
		event := input.Event
		uid := FederationEventIssueUID(event.IssueUID, string(event.Payload))
		if event.Type == "issue.created" || event.Type == "issue.snapshot" {
			v.baselines[uid] = true
		}
		if event.Type == "issue.moved" && len(PayloadMap(event.Payload)["snapshot"]) != 0 {
			clock := moveSnapshotClock(event)
			first, known := v.firstMoves[uid]
			if !known || compareClock(clock, first) < 0 {
				v.firstMoves[uid] = clock
			}
		}
	}
	return v
}

func moveSnapshotClock(event RemoteEvent) FoldClock {
	return FoldClock{EventUID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter}
}

// Validate checks fresh fallback authors after duplicate recognition.
func (v *FederationMoveSnapshotValidator) Validate(ctx context.Context, event RemoteEvent, boundActor string) error {
	if boundActor == "" {
		return nil
	}
	canSeed, err := v.CanSeed(ctx, event)
	if err != nil || !canSeed {
		return err
	}
	// An adoption author-preservation grant applies to its baseline,
	// never to a fresh move that introduces otherwise unretained content.
	return ValidateFederationBoundActorPayload(ctx, FederationMoveSnapshotEvent(event), FederationBoundActorCheck{BoundActor: boundActor})
}

// CanSeed reports whether this snapshot supplies the fold's fallback baseline.
// Ignored snapshots introduce neither attribution nor new project references.
func (v *FederationMoveSnapshotValidator) CanSeed(ctx context.Context, event RemoteEvent) (bool, error) {
	if event.Type != "issue.moved" {
		return false, nil
	}
	uid := FederationEventIssueUID(event.IssueUID, string(event.Payload))
	if _, known := v.baselines[uid]; !known {
		var baseline bool
		if err := v.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE issue_uid=$1 AND type IN ('issue.created','issue.snapshot'))`, uid).Scan(&baseline); err != nil {
			return false, err
		}
		v.baselines[uid] = baseline
	}
	if v.baselines[uid] {
		return false, nil
	}
	if !v.loaded[uid] {
		var retained FoldClock
		err := v.tx.QueryRowContext(ctx, `SELECT uid,origin_instance_uid,hlc_physical_ms,hlc_counter FROM events
 WHERE issue_uid=$1 AND type='issue.moved' AND `+v.snapshotExpression+` IS NOT NULL
 ORDER BY hlc_physical_ms,hlc_counter,origin_instance_uid,uid LIMIT 1`, uid).Scan(&retained.EventUID, &retained.OriginInstanceUID, &retained.HLCPhysicalMS, &retained.HLCCounter)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		first, known := v.firstMoves[uid]
		if err == nil && (!known || compareClock(retained, first) < 0) {
			v.firstMoves[uid] = retained
		}
		v.loaded[uid] = true
	}
	return compareClock(moveSnapshotClock(event), v.firstMoves[uid]) == 0, nil
}

// FederationMoveSnapshotReplyIssues resolves known reply targets to their
// current issue so native push applies its existing project authority checks.
// Unknown targets remain deferred under the existing comment replay contract.
func FederationMoveSnapshotReplyIssues(ctx context.Context, tx *sql.Tx, event RemoteEvent) ([]string, error) {
	if event.Type != "issue.moved" {
		return nil, nil
	}
	var snapshot struct {
		Comments []struct {
			ReplyToUID string `json:"reply_to_uid"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(FederationMoveSnapshotEvent(event).Payload, &snapshot); err != nil {
		return nil, err
	}
	var refs []string
	for _, comment := range snapshot.Comments {
		if comment.ReplyToUID == "" {
			continue
		}
		var uid string
		err := tx.QueryRowContext(ctx, `SELECT i.uid FROM comments c JOIN issues i ON i.id=c.issue_id WHERE c.uid=$1`, comment.ReplyToUID).Scan(&uid)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		refs = append(refs, uid)
	}
	return refs, nil
}
