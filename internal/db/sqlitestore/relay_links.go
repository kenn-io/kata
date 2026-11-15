package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

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
		err := tx.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE uid=?`, issueUID).Scan(&endpointProjectID)
		if err == sql.ErrNoRows {
			continue // Same-project peers may arrive later in the relay stream.
		}
		if err != nil {
			return fmt.Errorf("resolve relay link endpoint %s: %w", issueUID, err)
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
	accepted    map[string]bool
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
	c := relayOriginClassifier{relaySpokes: map[string]struct{}{}, receipts: map[string]struct{}{}, accepted: map[string]bool{}}
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
	//nolint:gosec // Project IDs are bound parameters.
	checkpointRows, err := tx.QueryContext(ctx, `SELECT p.uid,m.value FROM projects p JOIN meta m ON m.key=? || p.uid WHERE p.id IN (`+placeholders+`)`, append([]any{db.RelayResetMetadataPrefix}, args...)...)
	if err != nil {
		return c, err
	}
	defer func() { _ = checkpointRows.Close() }()
	for checkpointRows.Next() {
		var project, raw string
		if err := checkpointRows.Scan(&project, &raw); err != nil {
			return c, err
		}
		var checkpoint db.RelayResetCheckpoint
		if err := json.Unmarshal([]byte(raw), &checkpoint); err != nil {
			return c, err
		}
		accepted, err := db.RelayResetLegacySources(checkpoint.Snapshot)
		if err != nil {
			return c, err
		}
		for event := range accepted {
			c.accepted[project+"\x00"+event] = true
		}
	}
	if err := checkpointRows.Err(); err != nil {
		return c, err
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
) ([]db.FoldEvent, db.FoldParentRemovalCheck, relayOriginClassifier, error) {
	if len(projectIDs) == 0 {
		return events, nil, relayOriginClassifier{}, nil
	}
	classifier, err := loadRelayOriginClassifier(ctx, tx, projectIDs)
	if err != nil {
		return nil, nil, classifier, err
	}
	issueProjects, err := relayLinkEndpointProjects(ctx, tx, events)
	if err != nil {
		return nil, nil, classifier, err
	}
	filtered, check, err := filterRelayLinkEvents(ctx, tx, events, classifier, issueProjects)
	return filtered, check, classifier, err
}

func filterRelayLinkEvents(ctx context.Context, tx *sql.Tx, events []db.FoldEvent, classifier relayOriginClassifier, issueProjects map[string]string) ([]db.FoldEvent, db.FoldParentRemovalCheck, error) {
	for _, event := range events {
		if event.IssueUID != "" && issueProjects[event.IssueUID] == "" {
			issueProjects[event.IssueUID] = event.ProjectUID
		}
	}
	authority, err := relayFoldAuthorityTx(ctx, tx, issueProjects, classifier.instanceUID)
	if err != nil {
		return nil, nil, err
	}
	filtered := make([]db.FoldEvent, 0, len(events))
	for _, event := range events {
		if !classifier.relaySourced(event) {
			filtered = append(filtered, event)
			continue
		}
		policy := authority.Policy(event)
		policy.AcceptedRemoval = classifier.accepted[event.ProjectUID+"\x00"+event.UID]
		stripped, keep, err := db.StripCrossProjectLinkAdditions(event, policy)
		if err != nil {
			return nil, nil, err
		}
		if keep {
			filtered = append(filtered, stripped)
		}
	}
	return filtered, authority.AllowParentRemoval, nil
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

// relayFoldAuthorityTx loads live project membership for relay link folding.
func relayFoldAuthorityTx(ctx context.Context, tx *sql.Tx, owners map[string]string, instance string) (db.RelayFoldAuthority, error) {
	a := db.RelayFoldAuthority{Projects: owners, Roots: map[string]string{}, Groups: map[string]map[string]bool{}, Relayed: map[string]bool{}}
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
		err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE uid=? AND deleted_at IS NULL`, uid).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return a, err
		}
		ids[uid], uids[id] = id, uid
	}
	for uid, id := range ids {
		var relayed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_bindings b WHERE b.project_id=? AND (b.relay_config IS NOT NULL OR EXISTS(SELECT 1 FROM federation_enrollments e WHERE e.project_id=b.project_id AND e.relay_protocol_version=1)))`, id).Scan(&relayed); err != nil {
			return a, err
		}
		a.Relayed[uid] = relayed
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=?`, id))
		if errors.Is(err, db.ErrNotFound) {
			continue
		}
		if err != nil {
			return a, err
		}
		if !binding.Enabled {
			continue
		}
		root, err := relayProjectRootTx(ctx, tx, uid, binding, instance)
		if err != nil {
			return a, err
		}
		if root == "" {
			continue
		}
		a.Roots[uid] = root
		group, err := federationBindingGroupProjectIDs(ctx, tx, binding)
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
