package pgstore

import (
	"context"
	"database/sql"
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
	if !db.FederationEventAffectsLinks(event.Type) {
		return nil
	}
	payload := db.PayloadMap(event.Payload)
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
		err := tx.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE uid=$1`, issueUID).Scan(&endpointProjectID)
		if err == sql.ErrNoRows {
			continue // Same-project peers may arrive later in the relay stream.
		}
		if err != nil {
			return mapSQLError(err, nil)
		}
		if endpointProjectID != projectID {
			pin, err := rootPinTx(ctx, tx, event.ProjectUID, "")
			if err != nil && !errors.Is(err, db.ErrNotFound) {
				return err
			}
			if err == nil && event.OriginInstanceUID == pin.AuthorityUID {
				shared, err := relaySharedProjectsTx(ctx, tx, projectID, endpointProjectID, false)
				if err != nil {
					return err
				}
				if shared {
					continue
				}
			}
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
		return c, mapSQLError(err, nil)
	}
	placeholders, args := postgresIDPlaceholders(projectIDs, 1)
	extra := len(args) + 1
	spokeRows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT p.uid
		  FROM projects p JOIN federation_bindings b ON b.project_id=p.id
		 WHERE p.id IN (%s) AND b.role=$%d AND b.relay_config IS NOT NULL`, placeholders, extra),
		append(append([]any(nil), args...), string(db.FederationRoleSpoke))...)
	if err != nil {
		return c, mapSQLError(err, nil)
	}
	if err := scanRelayClassifierKeys(spokeRows, c.relaySpokes, 1); err != nil {
		return c, mapSQLError(err, nil)
	}
	receiptRows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT r.project_uid, r.event_uid
		  FROM federation_event_provenance r
		 WHERE r.project_uid IN (SELECT p.uid FROM projects p WHERE p.id IN (%s))
		   AND r.receipt::jsonb->>'ingress_instance_uid'<>$%d`, placeholders, extra),
		append(append([]any(nil), args...), c.instanceUID)...)
	if err != nil {
		return c, mapSQLError(err, nil)
	}
	if err := scanRelayClassifierKeys(receiptRows, c.receipts, 2); err != nil {
		return c, mapSQLError(err, nil)
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
) ([]db.FoldEvent, db.FoldParentRemovalCheck, error) {
	if len(projectIDs) == 0 || len(events) == 0 {
		return events, nil, nil
	}
	classifier, err := loadRelayOriginClassifier(ctx, tx, projectIDs)
	if err != nil {
		return nil, nil, err
	}
	if !slices.ContainsFunc(events, classifier.relaySourced) {
		return events, nil, nil
	}
	issueProjects, err := relayLinkEndpointProjects(ctx, tx, events)
	if err != nil {
		return nil, nil, err
	}

	for _, event := range events {
		if event.IssueUID != "" && issueProjects[event.IssueUID] == "" {
			issueProjects[event.IssueUID] = event.ProjectUID
		}
	}
	authority, err := relayFoldAuthorityTx(ctx, tx, issueProjects)
	if err != nil {
		return nil, nil, err
	}
	filtered := make([]db.FoldEvent, 0, len(events))
	relayEvents := make(map[string]bool)
	for _, event := range events {
		// Local edits on a relay spoke must also respect root-owned parents.
		if _, relaySpoke := classifier.relaySpokes[event.ProjectUID]; relaySpoke {
			relayEvents[event.UID] = true
		}
		if !classifier.relaySourced(event) {
			filtered = append(filtered, event)
			continue
		}
		relayEvents[event.UID] = true
		stripped, keep, err := db.StripCrossProjectLinkAdditions(event, authority.Policy(event))
		if err != nil {
			return nil, nil, err
		}
		if keep {
			filtered = append(filtered, stripped)
		}
	}
	return filtered, func(child, parent string, previous, next db.FoldClock) bool {
		return !relayEvents[next.EventUID] || authority.AllowParentRemoval(child, parent, previous, next)
	}, nil
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
	owners := make(map[string]string, len(ids))
	rows, err := tx.QueryContext(ctx, `SELECT i.uid,p.uid FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid = ANY($1::text[])`, ids)
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

// relayFoldAuthorityTx loads live project membership for relay link folding.
func relayFoldAuthorityTx(ctx context.Context, tx *sql.Tx, owners map[string]string) (db.RelayFoldAuthority, error) {
	a := db.RelayFoldAuthority{Projects: owners, Roots: map[string]string{}, Groups: map[string]map[string]bool{}}
	ids := map[string]int64{}
	uids := map[int64]string{}
	for _, uid := range owners {
		if uid == "" {
			continue
		}
		if _, exists := ids[uid]; exists {
			continue
		}
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE uid=$1 AND deleted_at IS NULL`, uid).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return a, err
		}
		ids[uid], uids[id] = id, uid
	}
	for uid, id := range ids {
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, id))
		if errors.Is(err, db.ErrNotFound) {
			continue
		}
		if err != nil {
			return a, err
		}
		if !binding.Enabled {
			continue
		}
		root, err := relayProjectRootTx(ctx, tx, uid, binding)
		if err != nil {
			return a, err
		}
		if root == "" {
			continue
		}
		a.Roots[uid] = root
		group, err := federationBindingGroupProjectIDsUnlocked(ctx, tx, binding)
		if err != nil {
			return a, err
		}
		a.Groups[uid] = map[string]bool{}
		for _, peer := range group {
			if peerUID := uids[peer]; peerUID != "" {
				a.Groups[uid][peerUID] = true
			}
		}
	}
	return a, nil
}
