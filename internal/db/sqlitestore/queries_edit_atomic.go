package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// EditIssueAtomic applies field updates, priority change, and link delta in
// one transaction. Either every requested mutation succeeds or none do.
//
// Events emitted (post-commit broadcast is the caller's responsibility):
//   - issue.updated  if changed of Title/Body/Owner actually changed
//   - issue.priority_set or issue.priority_cleared if priority actually changed
//   - issue.links_changed if changed link op actually changed (single aggregated)
//
// Idempotent no-ops do not emit their event.
func (d *Store) EditIssueAtomic(ctx context.Context, p db.EditIssueAtomicParams) (db.EditIssueAtomicResult, error) {
	return retryWrite1(ctx, d, func() (db.EditIssueAtomicResult, error) {
		return d.editIssueAtomic(ctx, p)
	})
}

func (d *Store) editIssueAtomic(ctx context.Context, p db.EditIssueAtomicParams) (db.EditIssueAtomicResult, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return db.EditIssueAtomicResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, p.IssueID)
	if err != nil {
		return db.EditIssueAtomicResult{}, err
	}
	p.Actor, err = d.effectiveLocalMutationActorTx(ctx, tx, issue.ProjectID, p.Actor)
	if err != nil {
		return db.EditIssueAtomicResult{}, err
	}

	var (
		events    []db.Event
		changes   db.AtomicEditChanges
		anyChange bool
	)

	// A single timestamp for the whole atomic edit: each sub-mutation's row
	// bump and event payload share it so replay reproduces one updated_at.
	ts := nowTimestamp()

	// 1. Field changes (title/body/owner). Compare each requested value
	// against the loaded row first and skip the UPDATE + issue.updated
	// event entirely when every requested field already matches reality.
	// Without this no-op detection, a request like
	// `kata edit 1 --title "$(current title)" --remove-blocks 2` would
	// fire issue.updated and increment hook/digest activity even when
	// no field actually changed.
	sets, args, payload, fieldsChanged, err := issueFieldUpdatePlan(issue, p.Title, p.Body, p.Owner, ts)
	if err != nil {
		return db.EditIssueAtomicResult{}, err
	}
	if fieldsChanged {
		sets = append([]string{`updated_at = ?`}, sets...)
		args = append([]any{ts}, args...)
		args = append(args, p.IssueID)
		// `sets` only contains fixed string literals; user values are bound
		// via `args`. Concatenation is safe.
		q := `UPDATE issues SET ` + joinComma(sets) + ` WHERE id = ?` // #nosec G202
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return db.EditIssueAtomicResult{}, fmt.Errorf("update issue fields: %w", err)
		}
		evt, err := d.insertEventTx(ctx, tx, eventInsert{
			ProjectID:   issue.ProjectID,
			ProjectName: projectName,
			IssueID:     &issue.ID,
			Type:        "issue.updated",
			Actor:       p.Actor,
			Payload:     payload,
		})
		if err != nil {
			return db.EditIssueAtomicResult{}, err
		}
		events = append(events, evt)
		anyChange = true
	}

	// 2. Priority. Same shape as the standalone UpdatePriority but inline so
	// we share the surrounding TX. Idempotent no-op when value is unchanged.
	if p.SetPriority != nil || p.ClearPriority {
		var newPrio *int64
		if !p.ClearPriority {
			newPrio = p.SetPriority
		}
		if !priorityEqual(issue.Priority, newPrio) {
			if _, err := tx.ExecContext(ctx,
				`UPDATE issues SET priority = ?, updated_at = ? WHERE id = ?`,
				newPrio, ts, p.IssueID); err != nil {
				return db.EditIssueAtomicResult{}, fmt.Errorf("update priority: %w", err)
			}
			eventType, payload, err := priorityEventPayload(issue.Priority, newPrio, ts)
			if err != nil {
				return db.EditIssueAtomicResult{}, err
			}
			evt, err := d.insertEventTx(ctx, tx, eventInsert{
				ProjectID:   issue.ProjectID,
				ProjectName: projectName,
				IssueID:     &issue.ID,
				Type:        eventType,
				Actor:       p.Actor,
				Payload:     payload,
			})
			if err != nil {
				return db.EditIssueAtomicResult{}, err
			}
			events = append(events, evt)
			anyChange = true
		}
	}

	// 3. Link delta. Any error here rolls back the entire TX, including
	// the field/priority changes above.
	linkChanged, err := d.applyLinksDeltaTx(ctx, tx, issue, p, &changes, ts)
	if err != nil {
		return db.EditIssueAtomicResult{}, err
	}
	if linkChanged {
		bs, err := linksChangedPayload(changes, ts)
		if err != nil {
			return db.EditIssueAtomicResult{}, fmt.Errorf("marshal links_changed payload: %w", err)
		}
		// When exactly one distinct peer is referenced across the entire
		// aggregated change, preserve envelope-level peer metadata so
		// consumers that route on related_issue_id / related_issue_uid
		// (the per-link issue.linked / issue.unlinked envelope shape)
		// retain peer identity. Multi-peer edits leave them NULL — the
		// payload's *_uids slices are authoritative.
		peerID, peerUID, err := singlePeerForLinksChangedTx(ctx, tx, changes)
		if err != nil {
			return db.EditIssueAtomicResult{}, err
		}
		evt, err := d.insertEventTx(ctx, tx, eventInsert{
			ProjectID:       issue.ProjectID,
			ProjectName:     projectName,
			IssueID:         &issue.ID,
			RelatedIssueID:  peerID,
			RelatedIssueUID: peerUID,
			Type:            "issue.links_changed",
			Actor:           p.Actor,
			Payload:         string(bs),
		})
		if err != nil {
			return db.EditIssueAtomicResult{}, err
		}
		events = append(events, evt)
		anyChange = true
	}

	updated, err := issueByIDTx(ctx, tx, p.IssueID)
	if err != nil {
		return db.EditIssueAtomicResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return db.EditIssueAtomicResult{}, fmt.Errorf("commit: %w", err)
	}
	return db.EditIssueAtomicResult{
		Issue:     updated,
		Events:    events,
		Changes:   changes,
		AnyChange: anyChange,
	}, nil
}

// linksChangedWirePayload is the legacy JSON shape for issue.links_changed
// event payloads. Field order matches the pre-PeerIdentity AtomicEditChanges
// declaration order exactly so marshaled bytes remain identical to the old
// embedded-struct emission (json.Marshal preserves struct field order).
type linksChangedWirePayload struct {
	ParentSet            *string  `json:"parent_set,omitempty"`
	ParentSetUID         *string  `json:"parent_set_uid,omitempty"`
	ParentRemoved        *string  `json:"parent_removed,omitempty"`
	ParentRemovedUID     *string  `json:"parent_removed_uid,omitempty"`
	BlocksAdded          []string `json:"blocks_added,omitempty"`
	BlocksAddedUIDs      []string `json:"blocks_added_uids,omitempty"`
	BlocksRemoved        []string `json:"blocks_removed,omitempty"`
	BlocksRemovedUIDs    []string `json:"blocks_removed_uids,omitempty"`
	BlockedByAdded       []string `json:"blocked_by_added,omitempty"`
	BlockedByAddedUIDs   []string `json:"blocked_by_added_uids,omitempty"`
	BlockedByRemoved     []string `json:"blocked_by_removed,omitempty"`
	BlockedByRemovedUIDs []string `json:"blocked_by_removed_uids,omitempty"`
	RelatedAdded         []string `json:"related_added,omitempty"`
	RelatedAddedUIDs     []string `json:"related_added_uids,omitempty"`
	RelatedRemoved       []string `json:"related_removed,omitempty"`
	RelatedRemovedUIDs   []string `json:"related_removed_uids,omitempty"`
	UpdatedAt            string   `json:"updated_at"`
}

// linksChangedPayload marshals c plus ts into the legacy wire bytes.
// Short-IDs go into the plain-name fields; UIDs into the *_uid / *_uids fields.
// Nil/empty inputs produce omitted keys, matching the old omitempty behavior.
func linksChangedPayload(c db.AtomicEditChanges, ts string) ([]byte, error) {
	p := linksChangedWirePayload{
		BlocksAdded:          peerShortIDs(c.BlocksAdded),
		BlocksAddedUIDs:      peerUIDs(c.BlocksAdded),
		BlocksRemoved:        peerShortIDs(c.BlocksRemoved),
		BlocksRemovedUIDs:    peerUIDs(c.BlocksRemoved),
		BlockedByAdded:       peerShortIDs(c.BlockedByAdded),
		BlockedByAddedUIDs:   peerUIDs(c.BlockedByAdded),
		BlockedByRemoved:     peerShortIDs(c.BlockedByRemoved),
		BlockedByRemovedUIDs: peerUIDs(c.BlockedByRemoved),
		RelatedAdded:         peerShortIDs(c.RelatedAdded),
		RelatedAddedUIDs:     peerUIDs(c.RelatedAdded),
		RelatedRemoved:       peerShortIDs(c.RelatedRemoved),
		RelatedRemovedUIDs:   peerUIDs(c.RelatedRemoved),
		UpdatedAt:            ts,
	}
	if c.ParentSet != nil {
		p.ParentSet = &c.ParentSet.ShortID
		p.ParentSetUID = &c.ParentSet.UID
	}
	if c.ParentRemoved != nil {
		p.ParentRemoved = &c.ParentRemoved.ShortID
		p.ParentRemovedUID = &c.ParentRemoved.UID
	}
	return json.Marshal(p)
}

// peerShortIDs extracts the ShortID from each PeerIdentity. Returns nil when
// the slice is empty so omitempty tags suppress the field.
func peerShortIDs(ps []db.PeerIdentity) []string {
	if len(ps) == 0 {
		return nil
	}
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ShortID
	}
	return out
}

// peerUIDs extracts the UID from each PeerIdentity. Returns nil when the
// slice is empty so omitempty tags suppress the field.
func peerUIDs(ps []db.PeerIdentity) []string {
	if len(ps) == 0 {
		return nil
	}
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.UID
	}
	return out
}

// applyLinksDeltaTx is the per-TX worker that performs every link mutation.
// Returns true when at least one row in `links` was inserted or deleted.
// Touches the issue's updated_at exactly once at the end if changed link changed.
func (d *Store) applyLinksDeltaTx(ctx context.Context, tx *sql.Tx, issue db.Issue, p db.EditIssueAtomicParams, changes *db.AtomicEditChanges, ts string) (bool, error) {
	changed := false

	// set_parent: replaces an existing parent if present. No-op when the
	// existing parent already points at the requested target. Cycle check
	// rejects an edit that would create a parent loop (#1 → #2 → #1).
	if p.SetParent != nil {
		// Parent targets may live in another project (storage v16 links are
		// project-independent edges), so use the project-agnostic lookup like
		// the add/remove edge paths. Soft-deleted rows stay excluded — a new
		// parent must not be a hidden issue.
		target, err := lookupIssueByIDTx(ctx, tx, *p.SetParent)
		if errors.Is(err, db.ErrNotFound) {
			return changed, &db.LinkTargetNotFoundError{Number: *p.SetParent}
		}
		if err != nil {
			return changed, err
		}
		if target.ID == issue.ID {
			return changed, db.ErrSelfLink
		}
		if err := assertNoParentCycleTx(ctx, tx, issue.ID, target.ID); err != nil {
			return changed, err
		}
		existing, perr := lookupParentOfTx(ctx, tx, issue.ID)
		if perr != nil && !errors.Is(perr, db.ErrNotFound) {
			return changed, perr
		}
		hasExisting := !errors.Is(perr, db.ErrNotFound)
		if !hasExisting || existing.ToIssueID != target.ID {
			recordedRemoval := false
			if hasExisting {
				// Capture the OLD parent's identity so the change payload
				// surfaces a parent_removed entry. Use the soft-delete-
				// tolerant lookup: the peer of an existing link may have
				// been soft-deleted, but we still own the link row and
				// need its endpoint identity to describe the removal.
				oldIdentity, lerr := peerIdentityTx(ctx, tx, existing.ToIssueID)
				if lerr != nil {
					return changed, lerr
				}
				res, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, existing.ID)
				if err != nil {
					return changed, fmt.Errorf("delete existing parent: %w", err)
				}
				rows, err := res.RowsAffected()
				if err != nil {
					return changed, fmt.Errorf("delete existing parent rows affected: %w", err)
				}
				// rows == 0 means a concurrent transaction already
				// removed the link we expected to delete. Don't claim
				// credit for a removal we didn't perform; just continue
				// to the insert (the end-state user wanted is still
				// reachable).
				if rows > 0 {
					changes.ParentRemoved = &oldIdentity
					recordedRemoval = true
				}
			}
			err := insertLinkRowTx(ctx, tx, issue.ID, target.ID, "parent", p.Actor)
			switch {
			case errors.Is(err, db.ErrLinkExists):
				// A concurrent edit set the same parent we wanted —
				// idempotent no-op. If we already recorded a removal
				// above, the net change is "removed old, no new added,"
				// which is a real mutation; keep ParentRemoved. If we
				// didn't record a removal, the call is a pure no-op.
				if recordedRemoval {
					changed = true
				}
			case err != nil:
				return changed, err
			default:
				newIdentity, ierr := peerIdentityTx(ctx, tx, target.ID)
				if ierr != nil {
					return changed, ierr
				}
				changes.ParentSet = &newIdentity
				changed = true
			}
		}
	}

	// remove_parent: strict — assert must match current parent's number.
	if p.RemoveParent != nil {
		existing, perr := lookupParentOfTx(ctx, tx, issue.ID)
		if errors.Is(perr, db.ErrNotFound) {
			return changed, db.ErrParentMismatch
		}
		if perr != nil {
			return changed, perr
		}
		// Soft-delete-tolerant: the parent peer may have been soft-deleted
		// since this issue was last edited; the link row still exists and
		// the user can still ask to clean it up.
		parentIssue, err := lookupIssueByIDTxIncludingDeleted(ctx, tx, existing.ToIssueID)
		if err != nil {
			return changed, err
		}
		// RemoveParent's int64 ref is interpreted as the parent's row id
		// for now (Task 10 migrates the public param to short_id).
		if parentIssue.ID != *p.RemoveParent {
			return changed, db.ErrParentMismatch
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, existing.ID)
		if err != nil {
			return changed, fmt.Errorf("delete parent: %w", err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return changed, fmt.Errorf("delete parent rows affected: %w", err)
		}
		// rows == 0 means a concurrent edit removed the parent link we
		// thought we'd just verified. The strict assertion ("the parent
		// IS #N right now") is no longer satisfied — surface the same
		// 409 the no-parent case produces, so the user knows their view
		// of the world was stale.
		if rows == 0 {
			return changed, db.ErrParentMismatch
		}
		removedIdentity, ierr := peerIdentityTx(ctx, tx, parentIssue.ID)
		if ierr != nil {
			return changed, ierr
		}
		changes.ParentRemoved = &removedIdentity
		changed = true
	}

	// add_blocks: URL issue → N (type=blocks).
	for _, n := range p.AddBlocks {
		added, peer, err := addEdgeTx(ctx, tx, issue, n, "blocks", p.Actor, false)
		if err != nil {
			return changed, err
		}
		if added {
			changes.BlocksAdded = append(changes.BlocksAdded, peer)
			changed = true
		}
	}
	// add_blocked_by: N → URL issue (type=blocks, reversed).
	for _, n := range p.AddBlockedBy {
		added, peer, err := addEdgeTx(ctx, tx, issue, n, "blocks", p.Actor, true)
		if err != nil {
			return changed, err
		}
		if added {
			changes.BlockedByAdded = append(changes.BlockedByAdded, peer)
			changed = true
		}
	}
	// add_related: URL issue ↔ N (type=related, canonicalized).
	for _, n := range p.AddRelated {
		added, peer, err := addEdgeTx(ctx, tx, issue, n, "related", p.Actor, false)
		if err != nil {
			return changed, err
		}
		if added {
			changes.RelatedAdded = append(changes.RelatedAdded, peer)
			changed = true
		}
	}

	// remove_*: idempotent.
	for _, n := range p.RemoveBlocks {
		removed, peer, err := removeEdgeTx(ctx, tx, issue, n, "blocks", false)
		if err != nil {
			return changed, err
		}
		if removed {
			changes.BlocksRemoved = append(changes.BlocksRemoved, peer)
			changed = true
		}
	}
	for _, n := range p.RemoveBlockedBy {
		removed, peer, err := removeEdgeTx(ctx, tx, issue, n, "blocks", true)
		if err != nil {
			return changed, err
		}
		if removed {
			changes.BlockedByRemoved = append(changes.BlockedByRemoved, peer)
			changed = true
		}
	}
	for _, n := range p.RemoveRelated {
		removed, peer, err := removeEdgeTx(ctx, tx, issue, n, "related", false)
		if err != nil {
			return changed, err
		}
		if removed {
			changes.RelatedRemoved = append(changes.RelatedRemoved, peer)
			changed = true
		}
	}

	if changed {
		if _, err := tx.ExecContext(ctx,
			`UPDATE issues SET updated_at = ? WHERE id = ?`,
			ts, issue.ID); err != nil {
			return changed, fmt.Errorf("touch issue: %w", err)
		}
	}
	return changed, nil
}

// peerIdentityTx reads a peer's display identity inside the mutating
// transaction, including its project name (links may span projects since
// storage v16). Reads regardless of deleted_at so soft-deleted peers on
// removal paths are still identifiable.
func peerIdentityTx(ctx context.Context, tx *sql.Tx, issueID int64) (db.PeerIdentity, error) {
	var p db.PeerIdentity
	err := tx.QueryRowContext(ctx, `
		SELECT i.short_id, i.uid, pr.name
		  FROM issues i JOIN projects pr ON pr.id = i.project_id
		 WHERE i.id = ?`, issueID).Scan(&p.ShortID, &p.UID, &p.Project)
	if errors.Is(err, sql.ErrNoRows) {
		return db.PeerIdentity{}, fmt.Errorf("peer identity for issue %d: %w", issueID, db.ErrNotFound)
	}
	if err != nil {
		return db.PeerIdentity{}, fmt.Errorf("peer identity for issue %d: %w", issueID, err)
	}
	return p, nil
}

// addEdgeTx inserts a link of the given type within the existing TX. When
// reverseDirection is true, the URL issue becomes the link's target and the
// numbered issue becomes the source (used for blocked_by). Idempotent on
// duplicate. Self-link returns ErrSelfLink. Links may span projects since
// storage v16; targetID is a globally unique issue row id.
func addEdgeTx(ctx context.Context, tx *sql.Tx, urlIssue db.Issue, targetID int64, linkType, actor string, reverseDirection bool) (bool, db.PeerIdentity, error) {
	target, err := lookupIssueByIDTx(ctx, tx, targetID)
	if errors.Is(err, db.ErrNotFound) {
		return false, db.PeerIdentity{}, &db.LinkTargetNotFoundError{Number: targetID}
	}
	if err != nil {
		return false, db.PeerIdentity{}, err
	}
	if target.ID == urlIssue.ID {
		return false, db.PeerIdentity{}, db.ErrSelfLink
	}
	from, to := urlIssue.ID, target.ID
	if reverseDirection {
		from, to = to, from
	}
	if linkType == "related" && from > to {
		from, to = to, from
	}
	// Detect duplicate before INSERT to make the no-op path cheap and to
	// avoid relying on a UNIQUE-violation error path.
	if _, err := lookupLinkByEndpointsTx(ctx, tx, from, to, linkType); err == nil {
		return false, db.PeerIdentity{}, nil
	} else if !errors.Is(err, db.ErrNotFound) {
		return false, db.PeerIdentity{}, err
	}
	if err := insertLinkRowTx(ctx, tx, from, to, linkType, actor); err != nil {
		// A concurrent edit may have inserted the same link between the
		// pre-insert lookup above and our INSERT. Treat that race as the
		// same idempotent no-op the lookup would have produced — the
		// resulting graph state is exactly what the caller asked for, just
		// committed by someone else first. The dedicated link endpoint
		// (used by the TUI) has the same behavior; mapping ErrLinkExists
		// to a 500 here would be a regression.
		if errors.Is(err, db.ErrLinkExists) {
			return false, db.PeerIdentity{}, nil
		}
		return false, db.PeerIdentity{}, err
	}
	identity, ierr := peerIdentityTx(ctx, tx, target.ID)
	if ierr != nil {
		return false, db.PeerIdentity{}, ierr
	}
	return true, identity, nil
}

// removeEdgeTx deletes a link of the given type within the existing TX.
//
// Behavior matrix:
//   - target exists, link exists → delete the link, return (true, peer, nil)
//   - target exists, link absent → idempotent no-op, return (false, {}, nil)
//   - target does not exist (typo, never created, or hard-purged) →
//     idempotent no-op, return (false, {}, nil). The contract is "no
//     link to N"; if there's no N at all, the desired end state already
//     holds, so the request succeeds. CLI-side resolution already
//     short-circuits this for UID/prefix refs (which never reach the
//     daemon when they don't resolve); this branch covers numeric refs
//     that bypass CLI resolution.
//
// Soft-delete-tolerant: a soft-deleted target's row still exists, so its
// number resolves and the link can be removed. The lookup uses the
// includes-deleted variant so a hidden peer doesn't mask the link row.
// Links may span projects since storage v16; targetID is a globally
// unique issue row id.
func removeEdgeTx(ctx context.Context, tx *sql.Tx, urlIssue db.Issue, targetID int64, linkType string, reverseDirection bool) (bool, db.PeerIdentity, error) {
	target, err := lookupIssueByIDTxIncludingDeleted(ctx, tx, targetID)
	if errors.Is(err, db.ErrNotFound) {
		return false, db.PeerIdentity{}, nil
	}
	if err != nil {
		return false, db.PeerIdentity{}, err
	}
	from, to := urlIssue.ID, target.ID
	if reverseDirection {
		from, to = to, from
	}
	if linkType == "related" && from > to {
		from, to = to, from
	}
	link, err := lookupLinkByEndpointsTx(ctx, tx, from, to, linkType)
	if errors.Is(err, db.ErrNotFound) {
		return false, db.PeerIdentity{}, nil
	}
	if err != nil {
		return false, db.PeerIdentity{}, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, link.ID)
	if err != nil {
		return false, db.PeerIdentity{}, fmt.Errorf("delete link: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, db.PeerIdentity{}, fmt.Errorf("delete link rows affected: %w", err)
	}
	// rows == 0 means a concurrent edit deleted the link between our
	// lookup and our DELETE — treat as the same idempotent no-op the
	// missing-link branch above handles. Returning true here would let
	// the caller append a phantom entry to the change payload for a
	// removal that didn't actually happen this transaction.
	if rows == 0 {
		return false, db.PeerIdentity{}, nil
	}
	identity, ierr := peerIdentityTx(ctx, tx, target.ID)
	if ierr != nil {
		return false, db.PeerIdentity{}, ierr
	}
	return true, identity, nil
}

// insertLinkRowTx inserts one row into the `links` table within an existing
// TX. Maps the standard schema errors (duplicate, parent-already-set,
// self-link) onto the typed sentinels.
//
// Race-window disambiguation for parent: the partial-parent UNIQUE produces
// the same error text whether the conflicting row points at the same
// target (concurrent identical insert → idempotent no-op) or at a different
// parent (real "parent already set" rejection). This mirrors the existing
// CreateLinkAndEvent path: re-query under the same TX to tell them apart
// and surface ErrLinkExists for the same-target case so callers can
// short-circuit to a no-op rather than 409 the user.
func insertLinkRowTx(ctx context.Context, tx *sql.Tx, fromID, toID int64, linkType, author string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO links(from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
		 VALUES(?, ?, (SELECT uid FROM issues WHERE id = ?), (SELECT uid FROM issues WHERE id = ?), ?, ?)`,
		fromID, toID, fromID, toID, linkType, author)
	if err != nil {
		classified := classifyLinkInsertError(err)
		if errors.Is(classified, db.ErrParentAlreadySet) && linkType == "parent" {
			var n int
			qErr := tx.QueryRowContext(ctx,
				`SELECT 1 FROM links WHERE from_issue_id = ? AND to_issue_id = ? AND type = ?`,
				fromID, toID, linkType).Scan(&n)
			if qErr == nil {
				return db.ErrLinkExists
			}
		}
		return classified
	}
	return nil
}

// lookupIssueByIDTx fetches one issue by its row id within a TX,
// excluding soft-deleted rows. Used by add-link paths that accept
// cross-project issue IDs (storage v16+).
func lookupIssueByIDTx(ctx context.Context, tx *sql.Tx, id int64) (db.Issue, error) {
	row := tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = ? AND i.deleted_at IS NULL`, id)
	return scanIssue(row)
}

// lookupIssueByIDTxIncludingDeleted fetches one issue by id within a TX,
// including soft-deleted rows. Used when reading the peer of an existing
// link, where the link row is still valid even if the peer issue has
// been soft-deleted.
func lookupIssueByIDTxIncludingDeleted(ctx context.Context, tx *sql.Tx, id int64) (db.Issue, error) {
	row := tx.QueryRowContext(ctx, issueSelect+` WHERE i.id = ?`, id)
	return scanIssue(row)
}

// lookupParentOfTx returns the parent link for child (or ErrNotFound) within
// a TX. Mirrors DB.ParentOf's query but uses tx.
func lookupParentOfTx(ctx context.Context, tx *sql.Tx, childIssueID int64) (db.Link, error) {
	row := tx.QueryRowContext(ctx,
		linkSelect+` WHERE from_issue_id = ? AND type = 'parent'`,
		childIssueID)
	return scanLink(row)
}

// lookupLinkByEndpointsTx finds a link row matching (from, to, type) within
// a TX. Mirrors DB.LinkByEndpoints but uses tx.
func lookupLinkByEndpointsTx(ctx context.Context, tx *sql.Tx, fromID, toID int64, linkType string) (db.Link, error) {
	row := tx.QueryRowContext(ctx,
		linkSelect+` WHERE from_issue_id = ? AND to_issue_id = ? AND type = ?`,
		fromID, toID, linkType)
	return scanLink(row)
}

// assertNoParentCycleTx walks up newParentID's parent chain looking for
// editingID. If found, the requested set_parent edit would create a loop;
// returns ErrParentCycle. The walk is bounded by db.MaxParentDepth — shared
// with the daemon's parent --replace pre-flight, which must refuse any chain
// this guard would refuse — so a corrupted graph (which the schema's
// UNIQUE-on-from partial index should already prevent) cannot wedge the
// transaction.
//
// Runs inside the same TX as the rest of the link delta so the check sees
// changed prior mutations the same edit has staged (e.g. a remove_parent on
// the new parent, which would already be visible after that branch ran).
func assertNoParentCycleTx(ctx context.Context, tx *sql.Tx, editingID, newParentID int64) error {
	current := newParentID
	for i := 0; i < db.MaxParentDepth; i++ {
		if current == editingID {
			return db.ErrParentCycle
		}
		var parent int64
		err := tx.QueryRowContext(ctx,
			`SELECT to_issue_id FROM links WHERE from_issue_id = ? AND type = 'parent'`,
			current).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // reached the root without finding editingID
		}
		if err != nil {
			return fmt.Errorf("walk parent chain: %w", err)
		}
		current = parent
	}
	return fmt.Errorf("parent chain exceeds depth limit %d (corrupted graph?)", db.MaxParentDepth)
}

// singlePeerForLinksChangedTx returns the lone peer's (id, uid) when the
// aggregated changes reference exactly one distinct peer UID. Returns
// nil/nil when zero or multiple peers are involved. The lookup ignores
// soft-delete state: an aggregated event can reference a peer that was
// soft-deleted (e.g. an idempotent --remove-blocks against a now-hidden
// peer), and the envelope should still point to it.
func singlePeerForLinksChangedTx(ctx context.Context, tx *sql.Tx, c db.AtomicEditChanges) (*int64, *string, error) {
	seen := map[string]struct{}{}
	add := func(uid string) {
		if uid != "" {
			seen[uid] = struct{}{}
		}
	}
	if c.ParentSet != nil {
		add(c.ParentSet.UID)
	}
	if c.ParentRemoved != nil {
		add(c.ParentRemoved.UID)
	}
	for _, lists := range [][]db.PeerIdentity{
		c.BlocksAdded, c.BlocksRemoved,
		c.BlockedByAdded, c.BlockedByRemoved,
		c.RelatedAdded, c.RelatedRemoved,
	} {
		for _, p := range lists {
			add(p.UID)
		}
	}
	if len(seen) != 1 {
		return nil, nil, nil
	}
	var only string
	for u := range seen {
		only = u
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM issues WHERE uid = ?`, only).Scan(&id); err != nil {
		return nil, nil, fmt.Errorf("resolve single peer uid %s: %w", only, err)
	}
	return &id, &only, nil
}
