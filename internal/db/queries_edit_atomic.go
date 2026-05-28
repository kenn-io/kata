package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// EditIssueAtomicParams carries the full set of mutations to apply to one
// issue in a single transaction. nil/false fields mean "no change."
type EditIssueAtomicParams struct {
	IssueID int64
	Actor   string

	// Field changes (nil = no change).
	Title *string
	Body  *string
	Owner *string

	// Priority. SetPriority != nil means set; ClearPriority means clear.
	// Both nil/false → no change. Mutually exclusive at this layer; the
	// handler enforces that contract before calling in.
	SetPriority   *int64
	ClearPriority bool

	// Parent slot. At most one of SetParent / RemoveParent may be non-nil
	// (the handler enforces). RemoveParent is strict: the asserted number
	// must equal the current parent's number, or ErrParentMismatch is
	// returned and the entire delta rolls back.
	SetParent    *int64
	RemoveParent *int64

	// Multi-valued link ops, framed from the URL issue's POV.
	//   AddBlocks:        URL issue → N (type=blocks)
	//   AddBlockedBy:     N → URL issue (type=blocks)
	//   AddRelated:       URL issue ↔ N (type=related, canonicalized)
	//   Remove* are idempotent — missing links are no-ops.
	AddBlocks       []int64
	AddBlockedBy    []int64
	AddRelated      []int64
	RemoveBlocks    []int64
	RemoveBlockedBy []int64
	RemoveRelated   []int64
}

// AtomicEditChanges describes which link mutations actually applied.
// Each entry pairs the peer's short_id (display snapshot) with its UID
// (stable identity) — the aggregated issue.links_changed event payload
// carries both forms so consumers can render display strings and key on
// UIDs for cross-cutover stability.
type AtomicEditChanges struct {
	ParentSet        *string `json:"parent_set,omitempty"`
	ParentSetUID     *string `json:"parent_set_uid,omitempty"`
	ParentRemoved    *string `json:"parent_removed,omitempty"`
	ParentRemovedUID *string `json:"parent_removed_uid,omitempty"`

	BlocksAdded     []string `json:"blocks_added,omitempty"`
	BlocksAddedUIDs []string `json:"blocks_added_uids,omitempty"`

	BlocksRemoved     []string `json:"blocks_removed,omitempty"`
	BlocksRemovedUIDs []string `json:"blocks_removed_uids,omitempty"`

	BlockedByAdded     []string `json:"blocked_by_added,omitempty"`
	BlockedByAddedUIDs []string `json:"blocked_by_added_uids,omitempty"`

	BlockedByRemoved     []string `json:"blocked_by_removed,omitempty"`
	BlockedByRemovedUIDs []string `json:"blocked_by_removed_uids,omitempty"`

	RelatedAdded     []string `json:"related_added,omitempty"`
	RelatedAddedUIDs []string `json:"related_added_uids,omitempty"`

	RelatedRemoved     []string `json:"related_removed,omitempty"`
	RelatedRemovedUIDs []string `json:"related_removed_uids,omitempty"`
}

// EditIssueAtomicResult is what the handler renders into a wire response.
type EditIssueAtomicResult struct {
	Issue     Issue
	Events    []Event           // 0..3: issue.updated, issue.priority_*, issue.links_changed
	Changes   AtomicEditChanges // for the response's "changes" block
	AnyChange bool
}

// ErrParentMismatch is returned by EditIssueAtomic when RemoveParent's
// asserted number does not match the current parent (including the
// no-parent case). Surfaced by the handler as 409 parent_mismatch.
var ErrParentMismatch = errors.New("parent mismatch")

// LinkTargetNotFoundError carries the offending project-scoped number
// when an add-edge or set-parent operation references an issue that
// doesn't exist. The handler renders a message that names the
// specific ref so multi-flag PATCHes (`--blocks 5 --blocks 99 --blocks 7`)
// can identify which target failed. Wraps ErrNotFound so existing
// errors.Is checks keep working.
type LinkTargetNotFoundError struct {
	Number int64
}

func (e *LinkTargetNotFoundError) Error() string {
	return fmt.Sprintf("link target #%d not found", e.Number)
}

func (e *LinkTargetNotFoundError) Unwrap() error { return ErrNotFound }

// EditIssueAtomic applies field updates, priority change, and link delta in
// one transaction. Either every requested mutation succeeds or none do.
//
// Events emitted (post-commit broadcast is the caller's responsibility):
//   - issue.updated  if changed of Title/Body/Owner actually changed
//   - issue.priority_set or issue.priority_cleared if priority actually changed
//   - issue.links_changed if changed link op actually changed (single aggregated)
//
// Idempotent no-ops do not emit their event.
func (d *DB) EditIssueAtomic(ctx context.Context, p EditIssueAtomicParams) (EditIssueAtomicResult, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return EditIssueAtomicResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	issue, projectName, err := lookupIssueForEvent(ctx, tx, p.IssueID)
	if err != nil {
		return EditIssueAtomicResult{}, err
	}

	var (
		events    []Event
		changes   AtomicEditChanges
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
		return EditIssueAtomicResult{}, err
	}
	if fieldsChanged {
		sets = append([]string{`updated_at = ?`}, sets...)
		args = append([]any{ts}, args...)
		args = append(args, p.IssueID)
		// `sets` only contains fixed string literals; user values are bound
		// via `args`. Concatenation is safe.
		q := `UPDATE issues SET ` + joinComma(sets) + ` WHERE id = ?` // #nosec G202
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return EditIssueAtomicResult{}, fmt.Errorf("update issue fields: %w", err)
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
			return EditIssueAtomicResult{}, err
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
				return EditIssueAtomicResult{}, fmt.Errorf("update priority: %w", err)
			}
			eventType, payload, err := priorityEventPayload(issue.Priority, newPrio, ts)
			if err != nil {
				return EditIssueAtomicResult{}, err
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
				return EditIssueAtomicResult{}, err
			}
			events = append(events, evt)
			anyChange = true
		}
	}

	// 3. Link delta. Any error here rolls back the entire TX, including
	// the field/priority changes above.
	linkChanged, err := d.applyLinksDeltaTx(ctx, tx, issue, p, &changes, ts)
	if err != nil {
		return EditIssueAtomicResult{}, err
	}
	if linkChanged {
		bs, err := json.Marshal(struct {
			AtomicEditChanges
			UpdatedAt string `json:"updated_at"`
		}{changes, ts})
		if err != nil {
			return EditIssueAtomicResult{}, fmt.Errorf("marshal links_changed payload: %w", err)
		}
		// When exactly one distinct peer is referenced across the entire
		// aggregated change, preserve envelope-level peer metadata so
		// consumers that route on related_issue_id / related_issue_uid
		// (the per-link issue.linked / issue.unlinked envelope shape)
		// retain peer identity. Multi-peer edits leave them NULL — the
		// payload's *_uids slices are authoritative.
		peerID, peerUID, err := singlePeerForLinksChangedTx(ctx, tx, changes)
		if err != nil {
			return EditIssueAtomicResult{}, err
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
			return EditIssueAtomicResult{}, err
		}
		events = append(events, evt)
		anyChange = true
	}

	if err := tx.Commit(); err != nil {
		return EditIssueAtomicResult{}, fmt.Errorf("commit: %w", err)
	}

	updated, err := d.IssueByID(ctx, p.IssueID)
	if err != nil {
		return EditIssueAtomicResult{}, err
	}
	return EditIssueAtomicResult{
		Issue:     updated,
		Events:    events,
		Changes:   changes,
		AnyChange: anyChange,
	}, nil
}

// applyLinksDeltaTx is the per-TX worker that performs every link mutation.
// Returns true when at least one row in `links` was inserted or deleted.
// Touches the issue's updated_at exactly once at the end if changed link changed.
func (d *DB) applyLinksDeltaTx(ctx context.Context, tx *sql.Tx, issue Issue, p EditIssueAtomicParams, changes *AtomicEditChanges, ts string) (bool, error) {
	changed := false

	// set_parent: replaces an existing parent if present. No-op when the
	// existing parent already points at the requested target. Cycle check
	// rejects an edit that would create a parent loop (#1 → #2 → #1).
	if p.SetParent != nil {
		target, err := lookupIssueByNumberTx(ctx, tx, issue.ProjectID, *p.SetParent)
		if errors.Is(err, ErrNotFound) {
			return changed, &LinkTargetNotFoundError{Number: *p.SetParent}
		}
		if err != nil {
			return changed, err
		}
		if target.ID == issue.ID {
			return changed, ErrSelfLink
		}
		if err := assertNoParentCycleTx(ctx, tx, issue.ID, target.ID); err != nil {
			return changed, err
		}
		existing, perr := lookupParentOfTx(ctx, tx, issue.ID)
		if perr != nil && !errors.Is(perr, ErrNotFound) {
			return changed, perr
		}
		hasExisting := !errors.Is(perr, ErrNotFound)
		if !hasExisting || existing.ToIssueID != target.ID {
			recordedRemoval := false
			if hasExisting {
				// Capture the OLD parent's short_id AND uid so the change
				// payload surfaces a parent_removed entry with both forms.
				// Use the soft-delete-tolerant lookup: the peer of an
				// existing link may have been soft-deleted, but we still
				// own the link row and need its endpoint identity to
				// describe the removal.
				oldParent, lerr := lookupIssueByIDTxIncludingDeleted(ctx, tx, existing.ToIssueID)
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
					oldShort := oldParent.ShortID
					oldUID := oldParent.UID
					changes.ParentRemoved = &oldShort
					changes.ParentRemovedUID = &oldUID
					recordedRemoval = true
				}
			}
			err := insertLinkRowTx(ctx, tx, issue.ProjectID, issue.ID, target.ID, "parent", p.Actor)
			switch {
			case errors.Is(err, ErrLinkExists):
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
				short := target.ShortID
				uid := target.UID
				changes.ParentSet = &short
				changes.ParentSetUID = &uid
				changed = true
			}
		}
	}

	// remove_parent: strict — assert must match current parent's number.
	if p.RemoveParent != nil {
		existing, perr := lookupParentOfTx(ctx, tx, issue.ID)
		if errors.Is(perr, ErrNotFound) {
			return changed, ErrParentMismatch
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
			return changed, ErrParentMismatch
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
			return changed, ErrParentMismatch
		}
		short := parentIssue.ShortID
		uid := parentIssue.UID
		changes.ParentRemoved = &short
		changes.ParentRemovedUID = &uid
		changed = true
	}

	// add_blocks: URL issue → N (type=blocks).
	for _, n := range p.AddBlocks {
		added, peer, err := addEdgeTx(ctx, tx, issue, p.ProjectIDFor(issue), n, "blocks", p.Actor, false)
		if err != nil {
			return changed, err
		}
		if added {
			changes.BlocksAdded = append(changes.BlocksAdded, peer.ShortID)
			changes.BlocksAddedUIDs = append(changes.BlocksAddedUIDs, peer.UID)
			changed = true
		}
	}
	// add_blocked_by: N → URL issue (type=blocks, reversed).
	for _, n := range p.AddBlockedBy {
		added, peer, err := addEdgeTx(ctx, tx, issue, p.ProjectIDFor(issue), n, "blocks", p.Actor, true)
		if err != nil {
			return changed, err
		}
		if added {
			changes.BlockedByAdded = append(changes.BlockedByAdded, peer.ShortID)
			changes.BlockedByAddedUIDs = append(changes.BlockedByAddedUIDs, peer.UID)
			changed = true
		}
	}
	// add_related: URL issue ↔ N (type=related, canonicalized).
	for _, n := range p.AddRelated {
		added, peer, err := addEdgeTx(ctx, tx, issue, p.ProjectIDFor(issue), n, "related", p.Actor, false)
		if err != nil {
			return changed, err
		}
		if added {
			changes.RelatedAdded = append(changes.RelatedAdded, peer.ShortID)
			changes.RelatedAddedUIDs = append(changes.RelatedAddedUIDs, peer.UID)
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
			changes.BlocksRemoved = append(changes.BlocksRemoved, peer.ShortID)
			changes.BlocksRemovedUIDs = append(changes.BlocksRemovedUIDs, peer.UID)
			changed = true
		}
	}
	for _, n := range p.RemoveBlockedBy {
		removed, peer, err := removeEdgeTx(ctx, tx, issue, n, "blocks", true)
		if err != nil {
			return changed, err
		}
		if removed {
			changes.BlockedByRemoved = append(changes.BlockedByRemoved, peer.ShortID)
			changes.BlockedByRemovedUIDs = append(changes.BlockedByRemovedUIDs, peer.UID)
			changed = true
		}
	}
	for _, n := range p.RemoveRelated {
		removed, peer, err := removeEdgeTx(ctx, tx, issue, n, "related", false)
		if err != nil {
			return changed, err
		}
		if removed {
			changes.RelatedRemoved = append(changes.RelatedRemoved, peer.ShortID)
			changes.RelatedRemovedUIDs = append(changes.RelatedRemovedUIDs, peer.UID)
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

// ProjectIDFor returns the issue's project ID — a tiny helper so the link-op
// loops above read cleanly.
func (EditIssueAtomicParams) ProjectIDFor(i Issue) int64 { return i.ProjectID }

// linkPeerRef captures the identity of a link's peer (UID + short_id) for
// payload emission. UIDs are canonical; short_ids are display snapshots.
type linkPeerRef struct {
	UID     string
	ShortID string
}

// addEdgeTx inserts a link of the given type within the existing TX. When
// reverseDirection is true, the URL issue becomes the link's target and the
// numbered issue becomes the source (used for blocked_by). Idempotent on
// duplicate. Self-link returns ErrSelfLink.
func addEdgeTx(ctx context.Context, tx *sql.Tx, urlIssue Issue, projectID, targetNum int64, linkType, actor string, reverseDirection bool) (bool, linkPeerRef, error) {
	target, err := lookupIssueByNumberTx(ctx, tx, projectID, targetNum)
	if errors.Is(err, ErrNotFound) {
		return false, linkPeerRef{}, &LinkTargetNotFoundError{Number: targetNum}
	}
	if err != nil {
		return false, linkPeerRef{}, err
	}
	if target.ID == urlIssue.ID {
		return false, linkPeerRef{}, ErrSelfLink
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
		return false, linkPeerRef{}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, linkPeerRef{}, err
	}
	if err := insertLinkRowTx(ctx, tx, projectID, from, to, linkType, actor); err != nil {
		// A concurrent edit may have inserted the same link between the
		// pre-insert lookup above and our INSERT. Treat that race as the
		// same idempotent no-op the lookup would have produced — the
		// resulting graph state is exactly what the caller asked for, just
		// committed by someone else first. The dedicated link endpoint
		// (used by the TUI) has the same behavior; mapping ErrLinkExists
		// to a 500 here would be a regression.
		if errors.Is(err, ErrLinkExists) {
			return false, linkPeerRef{}, nil
		}
		return false, linkPeerRef{}, err
	}
	return true, linkPeerRef{UID: target.UID, ShortID: target.ShortID}, nil
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
func removeEdgeTx(ctx context.Context, tx *sql.Tx, urlIssue Issue, targetNum int64, linkType string, reverseDirection bool) (bool, linkPeerRef, error) {
	target, err := lookupIssueByNumberTxIncludingDeleted(ctx, tx, urlIssue.ProjectID, targetNum)
	if errors.Is(err, ErrNotFound) {
		return false, linkPeerRef{}, nil
	}
	if err != nil {
		return false, linkPeerRef{}, err
	}
	from, to := urlIssue.ID, target.ID
	if reverseDirection {
		from, to = to, from
	}
	if linkType == "related" && from > to {
		from, to = to, from
	}
	link, err := lookupLinkByEndpointsTx(ctx, tx, from, to, linkType)
	if errors.Is(err, ErrNotFound) {
		return false, linkPeerRef{}, nil
	}
	if err != nil {
		return false, linkPeerRef{}, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, link.ID)
	if err != nil {
		return false, linkPeerRef{}, fmt.Errorf("delete link: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, linkPeerRef{}, fmt.Errorf("delete link rows affected: %w", err)
	}
	// rows == 0 means a concurrent edit deleted the link between our
	// lookup and our DELETE — treat as the same idempotent no-op the
	// missing-link branch above handles. Returning true here would let
	// the caller append a phantom entry to the change payload for a
	// removal that didn't actually happen this transaction.
	if rows == 0 {
		return false, linkPeerRef{}, nil
	}
	return true, linkPeerRef{UID: target.UID, ShortID: target.ShortID}, nil
}

// insertLinkRowTx inserts one row into the `links` table within an existing
// TX. Maps the standard schema errors (duplicate, parent-already-set,
// self-link, cross-project) onto the typed sentinels.
//
// Race-window disambiguation for parent: the partial-parent UNIQUE produces
// the same error text whether the conflicting row points at the same
// target (concurrent identical insert → idempotent no-op) or at a different
// parent (real "parent already set" rejection). This mirrors the existing
// CreateLinkAndEvent path: re-query under the same TX to tell them apart
// and surface ErrLinkExists for the same-target case so callers can
// short-circuit to a no-op rather than 409 the user.
func insertLinkRowTx(ctx context.Context, tx *sql.Tx, projectID, fromID, toID int64, linkType, author string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO links(project_id, from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
		 VALUES(?, ?, ?, (SELECT uid FROM issues WHERE id = ?), (SELECT uid FROM issues WHERE id = ?), ?, ?)`,
		projectID, fromID, toID, fromID, toID, linkType, author)
	if err != nil {
		classified := classifyLinkInsertError(err)
		if errors.Is(classified, ErrParentAlreadySet) && linkType == "parent" {
			var n int
			qErr := tx.QueryRowContext(ctx,
				`SELECT 1 FROM links WHERE from_issue_id = ? AND to_issue_id = ? AND type = ?`,
				fromID, toID, linkType).Scan(&n)
			if qErr == nil {
				return ErrLinkExists
			}
		}
		return classified
	}
	return nil
}

// lookupIssueByNumberTx fetches one issue by (project_id, number) within a
// TX. Soft-deleted rows are excluded — mutations that add link rows must
// not target hidden issues. For paths that need to identify the peer of an
// existing link (remove/replace), use lookupIssueByNumberTxIncludingDeleted
// so a soft-deleted other-endpoint doesn't make link cleanup impossible.
func lookupIssueByNumberTx(ctx context.Context, tx *sql.Tx, projectID, number int64) (Issue, error) {
	return lookupIssueByNumberTxOpts(ctx, tx, projectID, number, false)
}

// lookupIssueByNumberTxIncludingDeleted is the soft-delete-tolerant variant
// used by remove/replace link paths.
func lookupIssueByNumberTxIncludingDeleted(ctx context.Context, tx *sql.Tx, projectID, number int64) (Issue, error) {
	return lookupIssueByNumberTxOpts(ctx, tx, projectID, number, true)
}

func lookupIssueByNumberTxOpts(ctx context.Context, tx *sql.Tx, projectID, number int64, includeDeleted bool) (Issue, error) {
	// EditIssueAtomic still takes int64 link refs (api.LinkChanges remains
	// int64 until Task 10). Until the daemon migrates to short_id refs we
	// resolve the int64 ref against the issue's row id.
	const base = `SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status,
		       i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision,
		       i.recurrence_id, i.occurrence_key,
		       i.created_at, i.updated_at, i.closed_at, i.deleted_at
		FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE i.project_id = ? AND i.id = ?`
	q := base + ` AND i.deleted_at IS NULL`
	if includeDeleted {
		q = base
	}
	row := tx.QueryRowContext(ctx, q, projectID, number)
	return scanIssue(row)
}

// lookupIssueByIDTxIncludingDeleted fetches one issue by id within a TX,
// including soft-deleted rows. Used when reading the peer of an existing
// link, where the link row is still valid even if the peer issue has
// been soft-deleted.
func lookupIssueByIDTxIncludingDeleted(ctx context.Context, tx *sql.Tx, id int64) (Issue, error) {
	const q = `SELECT i.id, i.uid, i.project_id, p.uid, i.short_id, i.title, i.body, i.status,
		       i.closed_reason, i.owner, i.priority, i.author, i.metadata, i.revision,
		       i.recurrence_id, i.occurrence_key,
		       i.created_at, i.updated_at, i.closed_at, i.deleted_at
		FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE i.id = ?`
	row := tx.QueryRowContext(ctx, q, id)
	return scanIssue(row)
}

// lookupParentOfTx returns the parent link for child (or ErrNotFound) within
// a TX. Mirrors DB.ParentOf's query but uses tx.
func lookupParentOfTx(ctx context.Context, tx *sql.Tx, childIssueID int64) (Link, error) {
	row := tx.QueryRowContext(ctx,
		linkSelect+` WHERE from_issue_id = ? AND type = 'parent'`,
		childIssueID)
	return scanLink(row)
}

// lookupLinkByEndpointsTx finds a link row matching (from, to, type) within
// a TX. Mirrors DB.LinkByEndpoints but uses tx.
func lookupLinkByEndpointsTx(ctx context.Context, tx *sql.Tx, fromID, toID int64, linkType string) (Link, error) {
	row := tx.QueryRowContext(ctx,
		linkSelect+` WHERE from_issue_id = ? AND to_issue_id = ? AND type = ?`,
		fromID, toID, linkType)
	return scanLink(row)
}

// ErrParentCycle is returned when set_parent would create a cycle in the
// parent graph (the new parent is already a descendant of the issue under
// edit). Surfaced by the handler as 400 validation.
var ErrParentCycle = errors.New("parent cycle")

// assertNoParentCycleTx walks up newParentID's parent chain looking for
// editingID. If found, the requested set_parent edit would create a loop;
// returns ErrParentCycle. The walk is bounded by maxDepth so a corrupted
// graph (which the schema's UNIQUE-on-from + same-project triggers should
// already prevent) cannot wedge the transaction.
//
// Runs inside the same TX as the rest of the link delta so the check sees
// changed prior mutations the same edit has staged (e.g. a remove_parent on
// the new parent, which would already be visible after that branch ran).
func assertNoParentCycleTx(ctx context.Context, tx *sql.Tx, editingID, newParentID int64) error {
	const maxDepth = 1024
	current := newParentID
	for i := 0; i < maxDepth; i++ {
		if current == editingID {
			return ErrParentCycle
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
	return fmt.Errorf("parent chain exceeds depth limit %d (corrupted graph?)", maxDepth)
}

// singlePeerForLinksChangedTx returns the lone peer's (id, uid) when the
// aggregated changes reference exactly one distinct peer UID. Returns
// nil/nil when zero or multiple peers are involved. The lookup ignores
// soft-delete state: an aggregated event can reference a peer that was
// soft-deleted (e.g. an idempotent --remove-blocks against a now-hidden
// peer), and the envelope should still point to it.
func singlePeerForLinksChangedTx(ctx context.Context, tx *sql.Tx, c AtomicEditChanges) (*int64, *string, error) {
	seen := map[string]struct{}{}
	add := func(uid string) {
		if uid != "" {
			seen[uid] = struct{}{}
		}
	}
	if c.ParentSetUID != nil {
		add(*c.ParentSetUID)
	}
	if c.ParentRemovedUID != nil {
		add(*c.ParentRemovedUID)
	}
	for _, lists := range [][]string{
		c.BlocksAddedUIDs, c.BlocksRemovedUIDs,
		c.BlockedByAddedUIDs, c.BlockedByRemovedUIDs,
		c.RelatedAddedUIDs, c.RelatedRemovedUIDs,
	} {
		for _, u := range lists {
			add(u)
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
