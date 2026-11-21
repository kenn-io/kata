package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
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
	if !db.FederationEventAffectsLinks(event.Type) || event.Type == "issue.unlinked" {
		return nil
	}
	payload := db.PayloadMap(event.Payload)
	if event.Type == "issue.links_changed" {
		event.RelatedIssueUID = nil
		for _, key := range []string{"parent_removed_uid", "blocks_removed_uids", "blocked_by_removed_uids", "related_removed_uids"} {
			delete(payload, key)
		}
	}
	primaryUID := db.EventIssueUID(event.IssueUID)
	refs, err := payloadReferencedIssueUIDs(event, payload)
	if err != nil {
		return err
	}
	for _, issueUID := range refs {
		if issueUID == "" || issueUID == primaryUID {
			continue
		}
		var endpointProjectID int64
		var archived bool
		err := tx.QueryRowContext(ctx, `SELECT i.project_id, p.deleted_at IS NOT NULL FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=?`, issueUID).Scan(&endpointProjectID, &archived)
		if err == sql.ErrNoRows {
			continue // Same-project peers may arrive later in the relay stream.
		}
		if err != nil {
			return fmt.Errorf("resolve relay link endpoint %s: %w", issueUID, err)
		}
		if endpointProjectID == projectID {
			continue
		}
		allowed, err := relayCrossProjectEndpointAllowed(ctx, tx, projectID, endpointProjectID, archived, event)
		if err != nil {
			return err
		}
		if !allowed {
			return fmt.Errorf("%w: relay link endpoint %s belongs to another project", db.ErrFederationIngestValidation, issueUID)
		}
	}
	return nil
}

// relayCrossProjectEndpointAllowed accepts a link endpoint in another project
// only for a root-authored event: an archived endpoint project is skipped, and
// a live one must share the event project's relay audience.
func relayCrossProjectEndpointAllowed(
	ctx context.Context,
	tx *sql.Tx,
	projectID, endpointProjectID int64,
	archived bool,
	event db.RemoteEvent,
) (bool, error) {
	pin, err := rootPinTx(ctx, tx, event.ProjectUID, "")
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if event.OriginInstanceUID != pin.AuthorityUID {
		return false, nil
	}
	if archived {
		return true, nil
	}
	return relaySharedProjectsTx(ctx, tx, projectID, endpointProjectID, false)
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
// event keeps its same-project effects and root-authored removals.
func filterRelayCrossProjectLinkEvents(
	ctx context.Context,
	tx *sql.Tx,
	projectIDs []int64,
	events []db.FoldEvent,
) ([]db.FoldEvent, relayOriginClassifier, error) {
	if len(projectIDs) == 0 {
		return events, relayOriginClassifier{}, nil
	}
	classifier, err := loadRelayOriginClassifier(ctx, tx, projectIDs)
	if err != nil {
		return nil, classifier, err
	}
	if !slices.ContainsFunc(events, classifier.relaySourced) {
		return events, classifier, nil
	}
	issueProjects, err := relayLinkEndpointProjects(ctx, tx, events)
	if err != nil {
		return nil, classifier, err
	}
	filtered, err := filterRelayLinkEvents(ctx, tx, events, classifier, issueProjects)
	return filtered, classifier, err
}

func filterRelayLinkEvents(ctx context.Context, tx *sql.Tx, events []db.FoldEvent, classifier relayOriginClassifier, issueProjects map[string]string) ([]db.FoldEvent, error) {
	filtered := make([]db.FoldEvent, 0, len(events))
	for _, event := range events {
		if !classifier.relaySourced(event) {
			filtered = append(filtered, event)
			continue
		}
		pin, err := rootPinTx(ctx, tx, event.ProjectUID, "")
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, err
		}
		rootOrigin := err == nil && pin.AuthorityUID == event.OriginInstanceUID
		var lookupErr error
		shared := map[string]bool{}
		crosses := func(uid string) bool {
			if lookupErr != nil {
				return true
			}
			peer := issueProjects[uid]
			if peer == "" || peer == event.ProjectUID {
				return false
			}
			if !rootOrigin {
				return true
			}
			allowed, ok := shared[peer]
			if !ok {
				allowed, lookupErr = relayFoldPeerAllowed(ctx, tx, event.ProjectUID, peer)
				shared[peer] = allowed
			}
			return !allowed
		}
		stripped, keep, err := db.StripCrossProjectLinkAdditions(event, crosses)
		if err != nil {
			return nil, err
		}
		if lookupErr != nil {
			return nil, lookupErr
		}
		if keep {
			filtered = append(filtered, stripped)
		}
	}
	return filtered, nil
}

// relayFoldPeerAllowed applies the admission rule of
// relayCrossProjectEndpointAllowed to a root-authored link at fold time: a link
// with an archived endpoint project keeps its row, which readers hide until the
// project is restored, and two live projects must share the relay audience.
func relayFoldPeerAllowed(ctx context.Context, tx *sql.Tx, sourceUID, peerUID string) (bool, error) {
	var sourceID, peerID int64
	var archived bool
	err := tx.QueryRowContext(ctx, `SELECT s.id, p.id, s.deleted_at IS NOT NULL OR p.deleted_at IS NOT NULL FROM projects s, projects p WHERE s.uid=? AND p.uid=?`, sourceUID, peerUID).Scan(&sourceID, &peerID, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || archived {
		return archived, err
	}
	return relaySharedProjectsTx(ctx, tx, sourceID, peerID, false)
}

// relayLinkEndpointProjects resolves only issue UIDs referenced by this fold.
func relayLinkEndpointProjects(ctx context.Context, tx *sql.Tx, events []db.FoldEvent) (map[string]string, error) {
	refs := map[string]struct{}{}
	for _, event := range events {
		if event.IssueUID != "" {
			refs[event.IssueUID] = struct{}{}
		}
		var related *string
		if event.RelatedIssueUID != "" {
			related = &event.RelatedIssueUID
		}
		remote := db.RemoteEvent{EventUID: event.UID, Type: event.Type, RelatedIssueUID: related, Payload: event.Payload}
		peers, err := payloadReferencedIssueUIDs(remote, db.PayloadMap(event.Payload))
		if err != nil {
			return nil, err
		}
		for _, peer := range peers {
			if peer != "" {
				refs[peer] = struct{}{}
			}
		}
	}
	ids := make([]string, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	owners := make(map[string]string, len(ids))
	rows, err := tx.QueryContext(ctx, `SELECT i.uid,p.uid FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid IN (SELECT value FROM json_each(?))`, string(encoded))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var issue, project string
		if err := rows.Scan(&issue, &project); err != nil {
			return nil, err
		}
		owners[issue] = project
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return owners, nil
}
