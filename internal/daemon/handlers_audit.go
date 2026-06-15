package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
)

// registerAuditHandlers installs /api/v1/audit/closes, a read-only
// projection of issue.closed events that surfaces per-close audit rows
// with derived flags. The endpoint is project-scoped via ?project_id;
// callers identify the project the same way `kata list` does. The
// handler reuses EventsInWindow for the time / actor filter, then
// applies in-process filters for reason, parent, and no-evidence.
// Close.throttled events in the same window decorate the matching
// close rows with the "throttled" flag so reviewers can see which
// closes the agent retried past a refusal.
func registerAuditHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "auditCloses",
		Method:      "GET",
		Path:        "/api/v1/audit/closes",
	}, func(ctx context.Context, in *api.AuditClosesRequest) (*api.AuditClosesResponse, error) {
		return doAuditCloses(ctx, cfg, in)
	})
}

func doAuditCloses(
	ctx context.Context, cfg ServerConfig, in *api.AuditClosesRequest,
) (*api.AuditClosesResponse, error) {
	if in.ProjectID <= 0 {
		return nil, api.NewError(400, "validation",
			"project_id must be a positive integer", "", nil)
	}
	if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
		return nil, err
	}
	since, until, err := parseAuditWindow(in.Since, in.Until)
	if err != nil {
		return nil, err
	}
	// --actor is intentionally NOT pushed down to EventsInWindow. The
	// throttle-marker pass needs to observe every close of the issue
	// to end a cycle: if agent-b closes the issue between agent-a's
	// throttle event and agent-a's later close, agent-b's close ends
	// the cycle and agent-a's later close starts fresh. Filtering at
	// SQL would hide agent-b's close and incorrectly carry the marker
	// forward. The actor filter is applied at row-emit time below.
	params := db.EventsInWindowParams{
		ProjectID: in.ProjectID,
		Since:     since,
		Until:     until,
	}
	events, err := cfg.DB.EventsInWindow(ctx, params)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	rows, err := buildAuditRows(ctx, cfg, in, events)
	if err != nil {
		return nil, err
	}
	out := &api.AuditClosesResponse{}
	out.Body.Rows = rows
	return out, nil
}

// parseAuditWindow parses the request's --since / --until pair. Empty
// since defaults to the zero time; empty until defaults to now. The
// daemon's events table stores created_at at millisecond precision via
// a sqlite strftime format, so we format here in the same way.
const auditTimeFmt = "2006-01-02T15:04:05.000Z"

func parseAuditWindow(sinceIn, untilIn string) (since, until string, err error) {
	since = time.Time{}.UTC().Format(auditTimeFmt)
	until = time.Now().UTC().Format(auditTimeFmt)
	if sinceIn != "" {
		t, perr := time.Parse(time.RFC3339, sinceIn)
		if perr != nil {
			return "", "", api.NewError(400, "validation",
				"since must be an RFC3339 timestamp", "", nil)
		}
		since = t.UTC().Format(auditTimeFmt)
	}
	if untilIn != "" {
		t, perr := time.Parse(time.RFC3339, untilIn)
		if perr != nil {
			return "", "", api.NewError(400, "validation",
				"until must be an RFC3339 timestamp", "", nil)
		}
		until = t.UTC().Format(auditTimeFmt)
	}
	return since, until, nil
}

// closeEventPayload is the wire shape the daemon writes into the
// payload column for issue.closed events. Defined locally because the
// db package does not export it.
//
// ParentUID is the parent's immutable UID at close time. ParentShortID
// is the close-time display value, retained as a fallback when the
// parent has since been purged and the UID no longer resolves through
// the live issues table. Resolving UID → current short_id at audit
// time keeps the row stable across project-merge collision reshuffles
// (which can rewrite source-side short_ids) without losing the audit
// signal. Nil pointers signal "legacy event that predates these
// fields" — the audit projection falls back to a live links lookup
// only for that case; non-nil empty signals "no parent at close".
type closeEventPayload struct {
	Reason        string         `json:"reason"`
	Message       string         `json:"message,omitempty"`
	Evidence      []api.Evidence `json:"evidence,omitempty"`
	ParentUID     *string        `json:"parent_uid,omitempty"`
	ParentShortID *string        `json:"parent_short_id,omitempty"`
}

// buildAuditRows projects close events into AuditCloseRow shape and
// applies the reason / parent / no-evidence filters. Parent context
// comes from the close event's frozen parent_uid (resolved to the
// CURRENT short_id at read time so a project-merge collision rewrite
// is reflected) with parent_short_id as a fallback when the UID has
// since been purged. Legacy events that predate both fields fall
// back to a live links lookup — the best we can do for that subset.
//
// The throttled flag is decorated via a single chronological pass: a
// close.throttled event arms a pending marker for that (issue, actor)
// pair. Any subsequent close of the issue — by any actor — ENDS the
// pending cycle for that issue, but the flag is applied only when the
// closing actor was the one whose retry tripped the guard. Subsequent
// reopen → close cycles start fresh.
func buildAuditRows(
	ctx context.Context, cfg ServerConfig,
	in *api.AuditClosesRequest, events []db.Event,
) ([]api.AuditCloseRow, error) {
	parentFilter, err := resolveAuditParentFilter(ctx, cfg, in.ProjectID, in.Parent)
	if err != nil {
		return nil, err
	}
	parsed := make([]closeEventPayload, len(events))
	for i, ev := range events {
		if ev.Type != "issue.closed" {
			continue
		}
		_ = json.Unmarshal([]byte(ev.Payload), &parsed[i])
	}
	parentQualifiers, err := resolveParentQualifiers(ctx, cfg, parsed)
	if err != nil {
		return nil, err
	}
	legacyParents, err := loadLegacyParentsForCloseEvents(ctx, cfg, events, parsed)
	if err != nil {
		return nil, err
	}
	rows := make([]api.AuditCloseRow, 0, len(events))
	// pendingThrottle maps issue short_id -> set of actors with a
	// pending throttle marker. Any close of the issue clears the set;
	// the closing actor is flagged only if it was in the set.
	pendingThrottle := map[string]map[string]struct{}{}
	for i, ev := range events {
		// close.throttled markers arm a pending flag for the next
		// matching issue.closed; consume happens below. Skip the rest
		// of the loop body for non-close events so the throttle path
		// doesn't accidentally produce a row.
		if ev.Type == "close.throttled" && ev.IssueShortID != nil {
			actors := pendingThrottle[*ev.IssueShortID]
			if actors == nil {
				actors = map[string]struct{}{}
				pendingThrottle[*ev.IssueShortID] = actors
			}
			actors[ev.Actor] = struct{}{}
			continue
		}
		if ev.Type != "issue.closed" {
			continue
		}
		// End the throttle cycle for this issue BEFORE applying output
		// filters (--reason, --parent). Any close, by any actor, ends
		// the cycle — an intervening close means the pending throttle
		// is no longer the "next" close in the same cycle. The flag is
		// applied only when the closing actor was the throttled one.
		p := parsed[i]
		var throttledNow bool
		if ev.IssueShortID != nil {
			if actors, ok := pendingThrottle[*ev.IssueShortID]; ok {
				if _, has := actors[ev.Actor]; has {
					throttledNow = true
				}
				delete(pendingThrottle, *ev.IssueShortID)
			}
		}
		if in.Actor != "" && ev.Actor != in.Actor {
			continue
		}
		if in.Reason != "" && p.Reason != in.Reason {
			continue
		}
		row := api.AuditCloseRow{
			Time:    ev.CreatedAt.UTC().Format(time.RFC3339),
			Actor:   ev.Actor,
			Reason:  p.Reason,
			Message: p.Message,
		}
		if ev.IssueShortID != nil {
			row.Issue = *ev.IssueShortID
		}
		switch {
		case p.ParentUID != nil && *p.ParentUID != "":
			if q, ok := parentQualifiers[*p.ParentUID]; ok {
				// Same-project parent renders bare; a foreign parent
				// renders qualified ("project#short_id") so the ref is
				// unambiguous and round-trips as a --parent filter.
				if q.ProjectID == in.ProjectID {
					row.Parent = q.ShortID
				} else {
					row.Parent = qualifiedID(q.ProjectName, q.ShortID)
				}
			} else if p.ParentShortID != nil {
				// UID no longer resolves (parent purged). Fall back to
				// the stored short_id as best-effort display; it may be
				// stale after a merge, but it is the only label we kept
				// for an issue that no longer exists.
				row.Parent = *p.ParentShortID
			}
		case p.ParentUID != nil:
			// Modern event with no parent at close time. Leave empty.
		case p.ParentShortID != nil:
			// Transition-only: a payload with parent_short_id but no
			// parent_uid would mean a brief intermediate format; trust
			// the stored short_id.
			row.Parent = *p.ParentShortID
		case ev.IssueID != nil:
			if pn, ok := legacyParents[*ev.IssueID]; ok {
				row.Parent = pn
			}
		}
		if parentFilter.has && !parentFilter.matches(row.Parent, p.ParentUID) {
			continue
		}
		for _, e := range p.Evidence {
			row.EvidenceTypes = append(row.EvidenceTypes, string(e.Type))
		}
		// Spec §5: legacy / pre-v1 closes have no evidence and surface
		// with flags=no-evidence. wontfix is the one reason where a
		// no-evidence close is admissible by design.
		if len(p.Evidence) == 0 && p.Reason != "wontfix" {
			row.Flags = append(row.Flags, "no-evidence")
		}
		if throttledNow {
			row.Flags = append(row.Flags, "throttled")
		}
		if in.NoEvidence && !auditFlagsContain(row.Flags, "no-evidence") {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// auditParentFilter holds the multiple identities we'll compare each
// row against. resolvedShortID is the parent's CURRENT short_id from
// a live (IncludeDeletedYes) lookup; resolvedUID is its UID from that
// same lookup, matched against the close payload's frozen parent_uid —
// the only identity that works when the parent lives in another
// project (parent links span projects). parsedShortID and parsedUID
// come from running the raw ref through shortid.Parse so a purged
// parent referenced as `project#short` or by full UID still matches
// stored snapshots in close payloads. raw preserves the literal
// fallback for any odd form Parse rejected.
type auditParentFilter struct {
	resolvedShortID string
	resolvedUID     string
	parsedShortID   string
	parsedUID       string
	raw             string
	has             bool
}

// resolveAuditParentFilter turns the --parent query value into an
// auditParentFilter, accepting any ref form the link-target resolver
// accepts: bare short_id (audited project), qualified
// `project#short_id` (any project, archived included — parent links
// span projects), or full UID (global). Soft-deleted parents resolve
// through IncludeDeletedYes — audit is a historical view and a close
// of a child whose parent was later soft-deleted is still a real
// audit row. Purged parents (no row remains) fall through; the
// matcher uses parsed/raw values to hit stored parent_uid and
// parent_short_id snapshots in payloads.
func resolveAuditParentFilter(
	ctx context.Context, cfg ServerConfig, projectID int64, parentRef string,
) (auditParentFilter, error) {
	if parentRef == "" {
		return auditParentFilter{}, nil
	}
	f := auditParentFilter{raw: parentRef, has: true}
	parsed, perr := shortid.Parse(parentRef)
	if perr == nil {
		f.parsedShortID = parsed.ShortID
		f.parsedUID = parsed.ULID
	}
	issue, rerr := resolveLinkTargetRef(ctx, cfg.DB, projectID, parentRef, db.IncludeDeletedYes)
	if rerr == nil {
		f.resolvedUID = issue.UID
		if issue.ProjectID == projectID {
			f.resolvedShortID = issue.ShortID
		} else {
			// Foreign parent: match by UID only. row.Parent is a display
			// short_id from the audited project's perspective, so the
			// foreign ref's bare suffix must not match same-suffix issues
			// in the audited project.
			f.parsedShortID = ""
		}
		return f, nil
	}
	var apiErr *api.APIError
	if !errors.As(rerr, &apiErr) || apiErr.Status != http.StatusNotFound {
		return auditParentFilter{}, rerr
	}
	// Resolver 404 means the ref resolved nowhere: a purged parent in
	// THIS project (we want the parsed/raw fallback to hit stored
	// snapshots), or a qualified ref whose project or short_id suffix
	// does not exist (we must NOT let parsedShortID match same-suffix
	// issues in the scoped project). Distinguish by re-checking the
	// parsed qualifier against the scoped project's name. Mismatched
	// qualifiers clear the parsed short_id so only the raw form remains
	// as a fallback, and the raw form ("other#abc4") never matches a
	// bare row.Parent.
	if perr == nil && parsed.Project != "" {
		project, projErr := cfg.DB.ProjectByID(ctx, projectID)
		if projErr != nil {
			return auditParentFilter{}, api.NewError(500, "internal", projErr.Error(), "", nil)
		}
		if parsed.Project != project.Name {
			// Clear every parsed identifier, not just parsedShortID, so a
			// mismatched qualifier is a "match nothing" filter regardless
			// of parser shape. Today shortid.Parse never sets both Project
			// and ULID, but keeping the clear local makes the invariant
			// obvious here.
			f.parsedShortID = ""
			f.parsedUID = ""
		}
	}
	return f, nil
}

// matches reports whether a close event's row.Parent (current display
// short_id) and payloadParentUID (frozen close-time UID, may be nil
// for legacy events) line up with the filter under any of the
// accepted ref forms.
//
// When the filter resolved a concrete parent (UID known) and the row
// froze a parent UID at close time, UID equality is authoritative:
// falling through to short_id comparison would let a same-suffix
// parent in another project claim the row (foreign parents display
// only their bare close-time short_id). The short_id fallbacks remain
// for legacy rows with no frozen UID and for purged parents, where the
// snapshot is the only signal left.
func (f auditParentFilter) matches(rowParent string, payloadParentUID *string) bool {
	if f.resolvedUID != "" && payloadParentUID != nil && *payloadParentUID != "" {
		return *payloadParentUID == f.resolvedUID
	}
	if f.resolvedShortID != "" && rowParent == f.resolvedShortID {
		return true
	}
	if f.parsedShortID != "" && rowParent == f.parsedShortID {
		return true
	}
	if f.parsedUID != "" && payloadParentUID != nil && *payloadParentUID == f.parsedUID {
		return true
	}
	if f.raw != "" && rowParent == f.raw {
		return true
	}
	return false
}

// loadLegacyParentsForCloseEvents gathers the issue ids of close events
// whose payload carries neither parent_uid nor parent_short_id (legacy
// events from before close-time parent freezing) and resolves each
// child -> parent via the live links table. New events skip the lookup
// entirely because their payload already carries the close-time parent.
//
// The live lookup remains best-effort for legacy data — if the issue
// was reparented since closing, the audit row reflects the current
// link, not the close-time link. There is no record of the original
// parent for legacy events.
func loadLegacyParentsForCloseEvents(
	ctx context.Context, cfg ServerConfig,
	events []db.Event, parsed []closeEventPayload,
) (map[int64]string, error) {
	ids := make([]int64, 0, len(events))
	seen := map[int64]struct{}{}
	for i, ev := range events {
		if ev.Type != "issue.closed" || ev.IssueID == nil {
			continue
		}
		if parsed[i].ParentUID != nil || parsed[i].ParentShortID != nil {
			continue
		}
		if _, ok := seen[*ev.IssueID]; ok {
			continue
		}
		seen[*ev.IssueID] = struct{}{}
		ids = append(ids, *ev.IssueID)
	}
	if len(ids) == 0 {
		return map[int64]string{}, nil
	}
	parents, err := cfg.DB.ParentShortIDsByIssues(ctx, ids)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	return parents, nil
}

// resolveParentQualifiers gathers every distinct, non-empty parent_uid
// across the parsed close payloads and asks the db to resolve them to
// their current project + short_id. Resolution is GLOBAL (not scoped to
// the audited project) so a parent that lives in another project still
// resolves — the caller renders it qualified ("project#short_id"). The
// result map omits UIDs that no longer resolve (parent purged); the audit
// projection falls back to the close-time parent_short_id for those.
func resolveParentQualifiers(
	ctx context.Context, cfg ServerConfig, parsed []closeEventPayload,
) (map[string]db.IssueQualifier, error) {
	seen := map[string]struct{}{}
	uids := make([]string, 0, len(parsed))
	for _, p := range parsed {
		if p.ParentUID == nil || *p.ParentUID == "" {
			continue
		}
		if _, ok := seen[*p.ParentUID]; ok {
			continue
		}
		seen[*p.ParentUID] = struct{}{}
		uids = append(uids, *p.ParentUID)
	}
	if len(uids) == 0 {
		return map[string]db.IssueQualifier{}, nil
	}
	out, err := cfg.DB.IssueQualifiersByUIDs(ctx, uids)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	return out, nil
}

func auditFlagsContain(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
