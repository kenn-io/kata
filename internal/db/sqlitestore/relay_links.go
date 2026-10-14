package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"go.kenn.io/kata/internal/db"
)

func validateRelayLinkEndpointsInProject(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	event db.RemoteEvent,
) error {
	if !db.FederationEventAffectsLinks(event.Type) {
		return nil
	}
	payload := db.PayloadMap(event.Payload)
	primaryUID, err := payloadIssueUID(event, payload)
	if err != nil {
		return err
	}
	refs, err := payloadReferencedIssueUIDs(event, payload)
	if err != nil {
		return err
	}
	for _, issueUID := range refs {
		if issueUID == "" || issueUID == primaryUID {
			continue
		}
		var endpointProjectID int64
		err := tx.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE uid=?`, issueUID).Scan(&endpointProjectID)
		if err == sql.ErrNoRows {
			continue // Same-project peers may arrive later in the relay stream.
		}
		if err != nil {
			return fmt.Errorf("resolve relay link endpoint %s: %w", issueUID, err)
		}
		if endpointProjectID != projectID {
			return fmt.Errorf("%w: relay link endpoint %s belongs to another project", db.ErrFederationIngestValidation, issueUID)
		}
	}
	return nil
}

// relayOriginClassifier decides from durable per-event provenance whether a
// group event entered its project over a negotiated relay hop, without reading
// delivery rows. Relay ingest stores the source's own origin instance on the
// event, and legacy direct-spoke ingest stores a foreign origin too, so the
// origin alone cannot name the transport. The root hub signs a receipt for
// every relay delivery in the accepting transaction, naming the delivering
// peer as ingress, while legacy ingest signs none; on a hub a foreign-origin
// event with a foreign-ingress receipt is therefore relay-sourced. A project
// bound as a relay spoke receives foreign events only over relay, so there
// the binding decides, exactly as it does for local link writes in
// ensureRelayLinkBoundaryTx. Receipts and bindings outlive revocation and the
// removal of delivery rows, and a revoked grant leaves the write rule, so the
// fold and local writes follow the same boundary at every point in time.
type relayOriginClassifier struct {
	instanceUID string
	relaySpokes map[string]struct{}
	receipts    map[string]struct{}
}

func (c relayOriginClassifier) relaySourced(event db.FoldEvent) bool {
	if event.OriginInstanceUID == c.instanceUID {
		return false
	}
	if _, ok := c.relaySpokes[event.ProjectUID]; ok {
		return true
	}
	_, ok := c.receipts[event.ProjectUID+"\x00"+event.UID]
	return ok
}

func loadRelayOriginClassifier(ctx context.Context, tx *sql.Tx, projectIDs []int64) (relayOriginClassifier, error) {
	c := relayOriginClassifier{relaySpokes: map[string]struct{}{}, receipts: map[string]struct{}{}}
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='instance_uid'`).Scan(&c.instanceUID); err != nil {
		return c, fmt.Errorf("read instance uid: %w", err)
	}
	placeholders, args := projectIDPlaceholders(projectIDs)
	//nolint:gosec // The IN clause contains only count-derived placeholders; project IDs stay bound.
	spokeRows, err := tx.QueryContext(ctx, `
		SELECT p.uid
		  FROM projects p JOIN federation_bindings b ON b.project_id=p.id
		 WHERE p.id IN (`+placeholders+`) AND b.role=? AND b.relay_config IS NOT NULL`,
		append(append([]any(nil), args...), string(db.FederationRoleSpoke))...)
	if err != nil {
		return c, fmt.Errorf("list relay spoke projects: %w", err)
	}
	if err := scanRelayClassifierKeys(spokeRows, c.relaySpokes, 1); err != nil {
		return c, fmt.Errorf("scan relay spoke projects: %w", err)
	}
	//nolint:gosec // The IN clause contains only count-derived placeholders; project IDs stay bound.
	receiptRows, err := tx.QueryContext(ctx, `
		SELECT r.project_uid, r.event_uid
		  FROM federation_event_provenance r
		 WHERE r.project_uid IN (SELECT p.uid FROM projects p WHERE p.id IN (`+placeholders+`))
		   AND json_extract(r.receipt,'$.ingress_instance_uid')<>?`,
		append(append([]any(nil), args...), c.instanceUID)...)
	if err != nil {
		return c, fmt.Errorf("list relay receipts: %w", err)
	}
	if err := scanRelayClassifierKeys(receiptRows, c.receipts, 2); err != nil {
		return c, fmt.Errorf("scan relay receipts: %w", err)
	}
	return c, nil
}

// scanRelayClassifierKeys collects one or two text columns per row into keys,
// joining two columns with a NUL byte, and closes rows.
func scanRelayClassifierKeys(rows *sql.Rows, keys map[string]struct{}, columns int) error {
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var first, second string
		var err error
		if columns == 1 {
			err = rows.Scan(&first)
		} else {
			err = rows.Scan(&first, &second)
			first += "\x00" + second
		}
		if err != nil {
			return err
		}
		keys[first] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return rows.Close()
}

// filterRelayCrossProjectLinkEvents keeps deferred relay link additions from
// resolving against issues in another local project during a later group
// materialization. Only the crossing additions are removed; a composite
// event keeps its same-project effects and every removal is applied.
func filterRelayCrossProjectLinkEvents(
	ctx context.Context,
	tx *sql.Tx,
	projectIDs []int64,
	events []db.FoldEvent,
) ([]db.FoldEvent, error) {
	if len(projectIDs) == 0 || len(events) == 0 {
		return events, nil
	}
	classifier, err := loadRelayOriginClassifier(ctx, tx, projectIDs)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(events, classifier.relaySourced) {
		return events, nil
	}
	placeholders, args := projectIDPlaceholders(projectIDs)
	//nolint:gosec // The IN clause contains only count-derived placeholders; project IDs stay bound.
	issueRows, err := tx.QueryContext(ctx, `
		SELECT i.uid, p.uid
		  FROM issues i JOIN projects p ON p.id=i.project_id
		 WHERE p.id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("list relay link endpoint projects: %w", err)
	}
	issueProjects := map[string]string{}
	for issueRows.Next() {
		var issueUID, projectUID string
		if err := issueRows.Scan(&issueUID, &projectUID); err != nil {
			_ = issueRows.Close()
			return nil, fmt.Errorf("scan relay link endpoint project: %w", err)
		}
		issueProjects[issueUID] = projectUID
	}
	if err := issueRows.Err(); err != nil {
		_ = issueRows.Close()
		return nil, fmt.Errorf("iterate relay link endpoint projects: %w", err)
	}
	if err := issueRows.Close(); err != nil {
		return nil, fmt.Errorf("close relay link endpoint projects: %w", err)
	}
	filtered := make([]db.FoldEvent, 0, len(events))
	for _, event := range events {
		if !classifier.relaySourced(event) {
			filtered = append(filtered, event)
			continue
		}
		crossProject := func(peerUID string) bool {
			projectUID, ok := issueProjects[peerUID]
			return ok && projectUID != event.ProjectUID
		}
		stripped, keep, err := db.StripCrossProjectLinkAdditions(event, crossProject)
		if err != nil {
			return nil, err
		}
		if keep {
			filtered = append(filtered, stripped)
		}
	}
	return filtered, nil
}
