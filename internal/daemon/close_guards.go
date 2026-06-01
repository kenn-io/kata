package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
)

// openChildrenSampleLimit caps the number of children listed inline in the
// parent-close refusal message. The full count is surfaced as a "(N more)"
// suffix so the user knows to consult `kata show` for the rest.
const openChildrenSampleLimit = 10

// siblingThrottleWindow is the look-back period over which sibling closes by
// the same actor under the same parent are counted. siblingThrottleLimit is
// the threshold: at the Nth close (N == limit) the actor has already closed
// (limit) prior siblings, and the next close is refused. Spec §3.9 fixes both
// values at "3 closes in 60 seconds" for v1; neither is configurable.
const (
	siblingThrottleWindow = 60 * time.Second
	siblingThrottleLimit  = 3
)

// repeatedMessageWindow is the look-back period for the repeated-message
// guard (spec §3.10). Closes of sibling issues with an identical normalized
// message by the same actor within this window are refused.
const repeatedMessageWindow = 30 * time.Minute

// CheckParentCloseCompleteness refuses a close on an issue with open
// children. Implements spec §3.8: the close handler maps a non-nil return
// to a 409 with code `parent_has_open_children`. Returns nil when the
// issue has zero open children (closed children do not block parent close).
//
// issueID is the parent's rowid (driving the OpenChildrenOf query);
// issueShortID is the user-facing ref quoted in the "kata show ... --json"
// hint so the suggested command is something the user can actually run.
func CheckParentCloseCompleteness(
	ctx context.Context, d db.Storage, projectID, issueID int64, issueShortID string,
) error {
	children, total, err := d.OpenChildrenOf(ctx, projectID, issueID, openChildrenSampleLimit)
	if err != nil {
		return err
	}
	if total == 0 {
		return nil
	}
	lines := make([]string, 0, len(children))
	for _, c := range children {
		lines = append(lines, fmt.Sprintf("  %s  %s", c.ShortID, c.Title))
	}
	suffix := ""
	if total > openChildrenSampleLimit {
		suffix = fmt.Sprintf("\n  ... (%d more, see `kata show %s --json`)",
			total-openChildrenSampleLimit, issueShortID)
	}
	return fmt.Errorf(
		"refusing — issue has %d open children:\n%s%s\nClose children first, "+
			"or scope this issue differently",
		total, strings.Join(lines, "\n"), suffix)
}

// CheckSiblingCloseThrottle implements spec §3.9. When the close is allowed,
// it returns refusal=nil and the other fields are zero. When refused, it
// returns the parent issue number, the recent sibling-close cohort (issue
// numbers ordered newest-first), and a descriptive error; the handler maps
// the refusal to a 429 sibling_throttle and feeds parentNumber+cohort into
// the close.throttled audit event.
//
// Issues with no parent link are not throttled — the rule depends on a shared
// parent. Database lookup errors soft-fail (refusal=nil) so a broken read path
// cannot block legitimate closes; the structural guards above this one
// (parent completeness, evidence) already surface real correctness problems.
//
// Concurrency note (v1 trade-off): this guard runs as a read before the
// close transaction. Two concurrent close requests under the same parent
// can both pass the read at the limit boundary (each sees N-1 siblings)
// and then serialize through CloseIssue's write — momentarily exceeding
// the configured limit by one. SQLite's write lock serializes the
// underlying issue.closed writes, and any subsequent close in the same
// window still observes the now-elevated count and is refused. Refused
// attempts that do hit the guard emit close.throttled audit events, so
// the signal is preserved for operators reviewing the throttle's
// effectiveness. v2 may fold the throttle check into the close
// transaction itself to close the race; for now the audit trail is the
// record of refused attempts and the limit's eventual self-correction is
// the practical bound.
func CheckSiblingCloseThrottle(
	ctx context.Context, d db.Storage,
	projectID, issueID int64, actor string, now time.Time,
) (parentShortID string, cohort []string, refusal error) {
	parentLink, err := d.ParentOf(ctx, issueID)
	if err != nil {
		// ErrNotFound = no parent set; any other error is treated as a soft
		// failure rather than blocking the close.
		return "", nil, nil
	}
	since := now.Add(-siblingThrottleWindow)
	siblings, err := d.RecentSiblingCloses(ctx, projectID, parentLink.ToIssueID, issueID, actor, since)
	if err != nil {
		return "", nil, nil
	}
	if len(siblings) < siblingThrottleLimit {
		return "", nil, nil
	}
	parentIssue, err := d.IssueByID(ctx, parentLink.ToIssueID)
	if err != nil {
		return "", nil, nil
	}
	lines := make([]string, 0, len(siblings))
	refs := make([]string, 0, len(siblings))
	for _, ev := range siblings {
		if ev.IssueShortID == nil {
			continue
		}
		refs = append(refs, *ev.IssueShortID)
		lines = append(lines, fmt.Sprintf("  %s closed %s ago",
			*ev.IssueShortID, humanizeDuration(now.Sub(ev.CreatedAt))))
	}
	refusal = fmt.Errorf(
		"sibling-close throttle: you closed %d children of %s in the last %s:\n%s\n"+
			"Slow down and review the scope of each remaining child before closing. "+
			"Wait for the throttle window to clear, or ask a human reviewer to inspect and close",
		len(siblings), parentIssue.ShortID, humanizeDuration(siblingThrottleWindow),
		strings.Join(lines, "\n"))
	return parentIssue.ShortID, refs, refusal
}

// CheckRepeatedMessageGuard implements spec §3.10. When the close is allowed,
// all returns are zero. When refused, it returns the prior matching close's
// issue number, the parent's issue number, and a descriptive error; the
// handler maps the refusal to a 429 duplicate_message and feeds priorNumber
// and parentNumber into the close.throttled audit event without re-resolving
// the parent.
//
// The guard only applies to reason=done and reason=audit-no-change closes
// on parented issues: wontfix / duplicate / superseded plausibly reuse
// boilerplate, and unparented issues lack the shared-parent context that
// turns a reused message into a strong abuse signal. Database lookup
// errors soft-fail (refusal=nil) so a broken read path cannot block
// legitimate closes; the structural guards above this one already surface
// real correctness problems.
//
// Concurrency note (v1 trade-off): same race window as
// CheckSiblingCloseThrottle — this is a read before the close
// transaction, so two concurrent closes with identical messages can both
// pass the read. SQLite serializes the closing writes, and the second
// distinct close-with-this-message in the window is recorded; any later
// close that re-runs the read sees the prior message and is refused.
// close.throttled audit events fire for guarded attempts, so the signal
// is preserved. v2 may move the check into the close transaction; for v1
// the audit trail and the window's later self-correction bound the
// exposure.
func CheckRepeatedMessageGuard(
	ctx context.Context, d db.Storage,
	projectID, issueID int64,
	actor, reason, message string, now time.Time,
) (priorShortID, parentShortID string, refusal error) {
	if reason != "done" && reason != "audit-no-change" {
		return "", "", nil
	}
	parentLink, err := d.ParentOf(ctx, issueID)
	if err != nil {
		// ErrNotFound = no parent set; any other error is treated as a soft
		// failure rather than blocking the close.
		return "", "", nil
	}
	norm := NormalizeMessage(message)
	// An empty normalized message carries no signature to match against.
	// Skip the guard so the TUI bypass path (which stores message="") and
	// any legitimate caller that forgot to supply a message do not get
	// false 429s for "identical" empty prose across siblings.
	if norm == "" {
		return "", "", nil
	}
	since := now.Add(-repeatedMessageWindow)
	prior, err := d.RecentSameMessageClose(ctx, projectID, parentLink.ToIssueID, issueID, actor, norm, since)
	if err != nil || prior == nil {
		return "", "", nil
	}
	if prior.IssueShortID != nil {
		priorShortID = *prior.IssueShortID
	}
	if parentIssue, perr := d.IssueByID(ctx, parentLink.ToIssueID); perr == nil {
		parentShortID = parentIssue.ShortID
	}
	refusal = fmt.Errorf(
		"refusing — identical close message to your close of %s at %s. "+
			"Both issues share a parent, and the message has not changed. "+
			"Each closure should describe its specific issue. If the same "+
			"prose truly applies, close as `--duplicate-of` or "+
			"`--superseded-by` instead",
		priorShortID, prior.CreatedAt.Format("15:04:05"))
	return priorShortID, parentShortID, refusal
}

// humanizeDuration renders d as "N sec" under a minute and "N min" otherwise.
// Used by the throttle error to describe how long ago each sibling closed.
func humanizeDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d sec", int(d.Seconds()))
	}
	return fmt.Sprintf("%d min", int(d.Minutes()))
}

// emitThrottledEvent records a close.throttled audit event when a throttle
// guard refuses a close. It persists the event, broadcasts it on the SSE
// stream, and queues it for webhook hooks so audit/replay tools surface the
// refusal alongside other lifecycle events. The payload's Reason field is
// "sibling-burst" (§3.9) or "duplicate-message" (§3.10); Cohort is set on
// the burst path and Prior on the duplicate-message path.
func emitThrottledEvent(
	ctx context.Context, cfg ServerConfig, issue db.Issue,
	actor string, payload db.CloseThrottledPayload,
) error {
	evt, err := cfg.DB.InsertCloseThrottledEvent(ctx, issue.ID, actor, payload)
	if err != nil {
		return fmt.Errorf("emit close.throttled: %w", err)
	}
	cfg.Broadcaster.Broadcast(StreamMsg{
		Kind: "event", Event: &evt, ProjectID: issue.ProjectID,
	})
	cfg.Hooks.Enqueue(evt)
	return nil
}
