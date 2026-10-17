package db

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
)

// FederationProjectReset invalidates one local project without exposing issue
// or destination identity. It is transient publication data, not wire state.
type FederationProjectReset struct {
	ProjectID int64
	EventID   int64
}

// FederationMoveResetCapture retains the resets from a successful materializer.
// Each transaction attempt uses its own capture and merges only after commit.
type FederationMoveResetCapture struct {
	Resets []FederationProjectReset
}

type federationMoveResetCaptureKey struct{}

// WithFederationMoveResetCapture starts a request-local publication capture.
func WithFederationMoveResetCapture(ctx context.Context) (context.Context, *FederationMoveResetCapture) {
	capture := &FederationMoveResetCapture{}
	return context.WithValue(ctx, federationMoveResetCaptureKey{}, capture), capture
}

func (c *FederationMoveResetCapture) add(reset FederationProjectReset) {
	for i := range c.Resets {
		if c.Resets[i].ProjectID == reset.ProjectID {
			c.Resets[i].EventID = max(c.Resets[i].EventID, reset.EventID)
			return
		}
	}
	c.Resets = append(c.Resets, reset)
}

// CommitFederationMoveResets returns only committed resets to an outer caller.
func CommitFederationMoveResets(ctx context.Context, resets []FederationProjectReset) {
	if capture, ok := ctx.Value(federationMoveResetCaptureKey{}).(*FederationMoveResetCapture); ok {
		for _, reset := range resets {
			capture.add(reset)
		}
	}
}

// FederationMovedReference retains a receiver-local departing reference when
// it differs from the signed move. The corresponding real move must remain in
// the journal for this local reset control to resolve or reserve the reference.
type FederationMovedReference struct {
	IssueUID       string `json:"-"`
	FromProjectUID string `json:"from_project_uid"`
	FromShortID    string `json:"from_short_id"`
	MoveEventUID   string `json:"move_event_uid"`
}

func recordFederationMoveDisplacement(ctx context.Context, tx *sql.Tx, source Project, targetUID string, winner FoldEvent, accepted []string, reference *FederationMovedReference, insertControl func(Project, *FederationMovedReference) (int64, error)) error {
	payload := PayloadMap(winner.Payload)
	from, _ := StringValue(payload["from_project_uid"])
	to, _ := StringValue(payload["to_project_uid"])
	atomic := to == targetUID && slices.Contains(accepted, winner.UID)
	if atomic && reference != nil {
		cursor, err := insertControl(source, reference)
		if err != nil {
			return err
		}
		if source.UID == from {
			if capture, ok := ctx.Value(federationMoveResetCaptureKey{}).(*FederationMoveResetCapture); ok {
				capture.add(FederationProjectReset{ProjectID: source.ID, EventID: cursor})
			}
		}
	}
	if source.UID == from {
		if atomic {
			return nil // Atomic native ingest publishes the applied move itself.
		}
		// Pull appends before applying, so both project snapshots may already
		// contain the move's cursor with their previous ownership. Invalidate
		// them at a fresh local cursor without resetting portable history.
		cursor, err := insertControl(source, reference)
		if err != nil {
			return err
		}
		var target Project
		if err := tx.QueryRowContext(ctx, `SELECT id,uid,name FROM projects WHERE uid=$1`, targetUID).Scan(&target.ID, &target.UID, &target.Name); err != nil {
			return err
		}
		for _, project := range []Project{source, target} {
			if err := recordFederationMoveUIReset(ctx, tx, project, cursor); err != nil {
				return err
			}
		}
		return nil
	}
	var cursor int64
	var err error
	if atomic {
		err = tx.QueryRowContext(ctx, `SELECT id FROM events WHERE uid=$1`, winner.UID).Scan(&cursor)
	} else {
		// Trusted pull may already have exposed the retained move's cursor
		// before materialization. A real local control advances the apply
		// cursor, including when replay/reattachment causes a delayed rehome.
		cursor, err = insertControl(source, reference)
	}
	if err != nil {
		return err
	}
	if !atomic {
		// A competing pull can rehome from a different project than the
		// declared source. The destination still needs an apply refresh;
		// its portable history must remain available.
		var target Project
		if err := tx.QueryRowContext(ctx, `SELECT id,uid,name FROM projects WHERE uid=$1`, targetUID).Scan(&target.ID, &target.UID, &target.Name); err != nil {
			return err
		}
		if err := recordFederationMoveUIReset(ctx, tx, target, cursor); err != nil {
			return err
		}
	}
	if err := RecordFederationPurgeResets(ctx, tx, []string{source.UID}, cursor); err != nil {
		return err
	}
	if capture, ok := ctx.Value(federationMoveResetCaptureKey{}).(*FederationMoveResetCapture); ok {
		capture.add(FederationProjectReset{ProjectID: source.ID, EventID: cursor})
	}
	return nil
}

func recordFederationMoveUIReset(ctx context.Context, tx *sql.Tx, project Project, cursor int64) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2)
 ON CONFLICT(key) DO UPDATE SET value=excluded.value
 WHERE CAST(meta.value AS BIGINT)<CAST(excluded.value AS BIGINT)`, AttributionUIResetMetadataPrefix+project.UID, strconv.FormatInt(cursor, 10)); err != nil {
		return err
	}
	if capture, ok := ctx.Value(federationMoveResetCaptureKey{}).(*FederationMoveResetCapture); ok {
		capture.add(FederationProjectReset{ProjectID: project.ID, EventID: cursor})
	}
	return nil
}
