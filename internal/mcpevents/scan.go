package mcpevents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// Scan sizing: events read per page, and pages per pass before the worker
// lets deliveries run.
const (
	scanPage      = 500
	maxScanPages  = 20
	projectPrefix = "project."
)

// scanSubscription is one active subscription as the scan matches it.
type scanSubscription struct {
	row  db.MCPEventSubscription
	args Arguments
	// selected holds an explicit project selection; nil follows every
	// project the principal can see.
	selected map[string]bool
}

func (s scanSubscription) selects(projectUID string) bool {
	return s.selected == nil || s.selected[projectUID]
}

// scanAuthority caches one principal's authority for a single scan pass.
type scanAuthority struct {
	auth    Authorized
	err     error
	visible map[string]bool
	// ids maps each visible project UID to its local id.
	ids map[string]int64
}

// selected returns the UIDs of the projects the subscription selects that
// its owner can see now.
func (a *scanAuthority) selected(sub scanSubscription) []string {
	uids := make([]string, 0, len(a.visible))
	for uid := range a.visible {
		if sub.selects(uid) {
			uids = append(uids, uid)
		}
	}
	return uids
}

// scan reads the archive after the daemon-wide scan position, marks the
// active subscriptions each event concerns dirty, and advances the position.
// It reports whether the position moved. A database without a position, as
// after a restore, starts at head and marks every active subscription dirty.
func (s *Service) scan(ctx context.Context) (bool, error) {
	// The scan serves every subscriber and applies each owner's authority
	// itself, so it reads the whole archive even when ctx is a subscribe
	// request limited to some projects, and the issues it reads are not that
	// request's. Cancellation and transaction fences still come from ctx.
	ctx = db.WithoutIssueRegistries(db.WithAuthorizedProjects(ctx, nil))
	position, ok, err := s.store.MCPEventsScanPosition(ctx)
	if err != nil {
		return false, err
	}
	head, err := s.storeHead(ctx)
	if err != nil {
		return false, err
	}
	if !ok {
		return s.store.InitializeMCPEventsScanPosition(ctx, head)
	}
	if head <= position {
		return false, nil
	}
	subs, unreadable, err := s.activeSubscriptions(ctx)
	if err != nil {
		return false, err
	}
	// The delivery pass ends a row whose arguments cannot be read.
	if err := s.store.MarkMCPEventSubscriptionsDirty(ctx, unreadable); err != nil {
		return false, err
	}
	if len(subs) == 0 {
		return s.store.AdvanceMCPEventsScanPosition(ctx, position, head)
	}
	auths := map[string]*scanAuthority{}
	reset, err := s.markResets(ctx, subs, position, auths)
	if err != nil {
		return false, err
	}
	// A purge can delete the archive event that would otherwise prompt the
	// fingerprint check, so any reset prompts it too.
	if err := s.refreshCatalogs(ctx, subs, position, head, auths, reset); err != nil {
		return false, err
	}
	ended := map[string]bool{}
	moved := false
	for range maxScanPages {
		events, err := s.store.EventsAfter(ctx, db.EventsAfterParams{AfterID: position, ThroughID: head, Limit: scanPage})
		if err != nil {
			return moved, err
		}
		dirty := map[string]bool{}
		next := position
		for _, event := range events {
			for _, sub := range subs {
				if dirty[sub.row.ID] || ended[sub.row.ID] {
					continue
				}
				concerns, err := s.concerns(ctx, sub, event, auths)
				switch {
				case ctx.Err() != nil:
					return moved, ctx.Err()
				case errors.Is(err, ErrPrincipalRevoked):
					// End it now rather than at the next authority pass, so
					// its terminated notice goes out with this drain.
					s.endLive(ctx, sub.row.ID, sub.row.Actor, "principal_revoked")
					ended[sub.row.ID] = true
				case err != nil, concerns:
					// An owner whose authority cannot be checked now is
					// marked conservatively, so one outage never holds back
					// the shared position. Delivery re-acquires the owner
					// before every send, so its ping waits for authority.
					dirty[sub.row.ID] = true
				}
			}
			next = event.ID
		}
		// A short page reached head; hidden system-project rows and purge
		// positions above the last event row are settled too.
		if len(events) < scanPage {
			next = head
		}
		ids := make([]string, 0, len(dirty))
		for id := range dirty {
			ids = append(ids, id)
		}
		if err := s.store.MarkMCPEventSubscriptionsDirty(ctx, ids); err != nil {
			return moved, err
		}
		if next == position {
			return moved, nil
		}
		advanced, err := s.store.AdvanceMCPEventsScanPosition(ctx, position, next)
		if err != nil || !advanced {
			// Another worker moved the position; it scans from there.
			return moved, err
		}
		moved, position = true, next
		if position >= head {
			return moved, nil
		}
	}
	return moved, nil
}

// markResets marks the subscriptions whose visible projects had a purge or a
// project stream reset after position. A purge deletes events the scan can no
// longer read, and a reset invalidates a project's history, so those
// subscribers reload. Each reset is matched to its project: an owner that
// cannot see the project learns nothing of it. The remaining events are still
// scanned as usual.
func (s *Service) markResets(ctx context.Context, subs []scanSubscription, position int64, auths map[string]*scanAuthority) (bool, error) {
	if reset, err := s.store.PurgeResetCheck(ctx, position, 0); err != nil || reset == 0 {
		return false, err
	}
	resetByProject := map[int64]bool{}
	var ids []string
	for _, sub := range subs {
		auth, err := s.scanAuthority(ctx, sub.row, auths)
		switch {
		case ctx.Err() != nil:
			return true, ctx.Err()
		case errors.Is(err, ErrPrincipalRevoked):
			// The authority recheck ends this subscription.
			continue
		case err != nil:
			// Marked conservatively, as in the event scan.
			ids = append(ids, sub.row.ID)
			continue
		}
		for _, uid := range auth.selected(sub) {
			id := auth.ids[uid]
			reset, checked := resetByProject[id]
			if !checked {
				after, err := s.store.PurgeResetCheck(ctx, position, id)
				if err != nil {
					return true, err
				}
				reset = after != 0
				resetByProject[id] = reset
			}
			if reset {
				ids = append(ids, sub.row.ID)
				break
			}
		}
	}
	return true, s.store.MarkMCPEventSubscriptionsDirty(ctx, ids)
}

// catalogEvents change which projects exist, so they can change any owner's
// visible projects. Access audit events do the same from the hidden system
// project.
var catalogEvents = db.MCPEventsCatalogEventTypes

// refreshCatalogs recomputes every subscription's visible-project fingerprint
// when (after, through] holds an event that can change it, and marks the
// subscriptions whose fingerprint changed. A gain or a loss both change it;
// an owner whose projects are unchanged learns nothing about projects it
// cannot see. Visibility is read live, after the range was fixed, so a change
// committed in between is folded in early; marking every change keeps that
// from hiding it. It runs before the position moves past those events, and
// recording the same fingerprint again changes nothing.
func (s *Service) refreshCatalogs(ctx context.Context, subs []scanSubscription, after, through int64, auths map[string]*scanAuthority, force bool) error {
	if !force {
		changed, err := s.catalogChanged(ctx, after, through)
		if err != nil || !changed {
			return err
		}
	}
	versions := map[string]string{}
	var unavailable []string
	for _, sub := range subs {
		auth, err := s.scanAuthority(ctx, sub.row, auths)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, ErrPrincipalRevoked):
			// The authority recheck ends this subscription.
		case err != nil:
			// Marked conservatively, as in the event scan.
			unavailable = append(unavailable, sub.row.ID)
		default:
			versions[sub.row.ID] = catalogVersion(auth.selected(sub))
		}
	}
	// A newer change can outdate these fingerprints. The store then keeps the
	// stored ones, and the later scan that reads the change computes them again.
	if _, err := s.store.UpdateMCPEventCatalogs(ctx, versions, through); err != nil {
		return err
	}
	// Their projects may have changed unseen, so the next refresh marks them
	// again even if visibility comes back to the stored fingerprint.
	if err := s.store.ForgetMCPEventCatalogs(ctx, unavailable); err != nil {
		return err
	}
	return s.store.MarkMCPEventSubscriptionsDirty(ctx, unavailable)
}

// catalogChanged reports whether (after, through] holds an access or catalog
// event, which can change any owner's visible projects.
func (s *Service) catalogChanged(ctx context.Context, after, through int64) (bool, error) {
	access, err := s.store.MCPEventsAccessChangesAfter(ctx, after, through, 1)
	if err != nil {
		return false, err
	}
	catalog, err := s.store.EventsAfter(ctx, db.EventsAfterParams{AfterID: after, ThroughID: through, Types: catalogEvents, Limit: 1})
	return len(access)+len(catalog) > 0, err
}

// settleCatalog runs once a subscription is active. Its fingerprint was taken
// with the archive at resolvedAfter, and the catch-up scan may have consumed
// later changes while the row was not active. If any change that can alter
// visibility followed, the fingerprint is forgotten, so the next refresh marks
// the row instead of matching a stale one. Changes after this check are
// scanned with the row active.
func (s *Service) settleCatalog(ctx context.Context, id string, resolvedAfter int64) error {
	ctx = db.WithoutIssueRegistries(db.WithAuthorizedProjects(ctx, nil))
	head, err := s.storeHead(ctx)
	if err != nil {
		return err
	}
	changed, err := s.catalogChanged(ctx, resolvedAfter, head)
	if err != nil {
		return err
	}
	if !changed {
		reset, err := s.store.PurgeResetCheck(ctx, resolvedAfter, 0)
		if err != nil {
			return err
		}
		changed = reset != 0
	}
	if !changed {
		return nil
	}
	return s.store.ForgetMCPEventCatalogs(ctx, []string{id})
}

// catalogVersion fingerprints a set of projects by UID.
func catalogVersion(projects []string) string {
	sorted := slices.Sorted(slices.Values(projects))
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:])
}

// rematch matches pulled events again after a federation pull has applied
// them. The pull commits remote events before it applies them, so the scan may
// have matched them against projections the pull had not updated, or skipped
// one whose issue did not exist yet; the applying transaction records the
// range of applied events (db.RecordMCPEventsRematch) for every daemon. Pulled
// events include this daemon's own, replayed after a federation reset.
//
// Only events up to the published scan position are matched again: a scan on
// another worker may hold later ones it read before the pull applied them, and
// it publishes them only after this pass. The record is acknowledged through
// that position, so the rest waits for a later pass.
func (s *Service) rematch(ctx context.Context) error {
	ctx = db.WithoutIssueRegistries(db.WithAuthorizedProjects(ctx, nil))
	from, through, token, ok, err := s.store.MCPEventsRematch(ctx)
	if err != nil || !ok {
		return err
	}
	position, ok, err := s.store.MCPEventsScanPosition(ctx)
	if err != nil || !ok || from > position {
		return err
	}
	through = min(through, position)
	subs, _, err := s.activeSubscriptions(ctx)
	if err != nil {
		return err
	}
	auths := map[string]*scanAuthority{}
	for after := from - 1; after < through; {
		events, err := s.store.EventsAfter(ctx, db.EventsAfterParams{AfterID: after, ThroughID: through, Limit: scanPage})
		if err != nil {
			return err
		}
		var ids []string
		for _, event := range events {
			for _, sub := range subs {
				concerns, err := s.concerns(ctx, sub, event, auths)
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// As in the scan, an owner whose authority cannot be
				// checked now is marked conservatively.
				if concerns || (err != nil && !errors.Is(err, ErrPrincipalRevoked)) {
					ids = append(ids, sub.row.ID)
				}
			}
		}
		if err := s.store.MarkMCPEventSubscriptionsDirty(ctx, ids); err != nil {
			return err
		}
		if len(events) < scanPage {
			break
		}
		after = events[len(events)-1].ID
	}
	_, err = s.store.AcknowledgeMCPEventsRematch(ctx, token, through)
	return err
}

// catchUpPasses bounds the scan a subscribe call runs before activating.
const catchUpPasses = 4

// catchUp scans the archive up to its head before a subscription is
// activated, so the new row matches only changes made after the subscribe
// call. Anything older is the client's first inbox read. It is best effort:
// a failure or a backlog beyond the bound costs at most a redundant ping.
func (s *Service) catchUp(ctx context.Context) {
	for range catchUpPasses {
		moved, err := s.scan(ctx)
		if err != nil || !moved {
			return
		}
	}
}

// activeSubscriptions lists live rows with their parsed arguments. Rows whose
// arguments cannot be read are returned separately by id.
func (s *Service) activeSubscriptions(ctx context.Context) ([]scanSubscription, []string, error) {
	now := s.now().UTC()
	var subs []scanSubscription
	var unreadable []string
	for row, err := range s.store.ExportMCPEventSubscriptions(ctx, db.ExportFilter{}) {
		if err != nil {
			return nil, nil, err
		}
		export := row.Subscription()
		if !live(export, now) {
			continue
		}
		var args Arguments
		if json.Unmarshal(export.Arguments, &args) != nil {
			unreadable = append(unreadable, export.ID)
			continue
		}
		sub := scanSubscription{row: export, args: args}
		if !args.Dynamic() {
			sub.selected = make(map[string]bool, len(args.ProjectUIDs))
			for _, uid := range args.ProjectUIDs {
				sub.selected[uid] = true
			}
		}
		subs = append(subs, sub)
	}
	return subs, unreadable, nil
}

// concerns decides whether one event can change the subscription's inbox. A
// ping names only the recipient, so a conservative yes costs one extra inbox
// read; a wrong no leaves an inbox stale. ErrPrincipalRevoked means the owner
// lost its authority; any other error means authority is unavailable now.
func (s *Service) concerns(ctx context.Context, sub scanSubscription, event db.Event, auths map[string]*scanAuthority) (bool, error) {
	o := Occurrence{EventID: event.ID, EventUID: event.UID, Type: event.Type, ProjectUID: event.ProjectUID, Payload: []byte(event.Payload)}
	if strings.HasPrefix(event.Type, "issue.") {
		// Some federated issue events name their issue only in the payload.
		o.IssueUID = db.FederationEventIssueUID(event.IssueUID, event.Payload)
	} else if event.IssueUID != nil {
		o.IssueUID = *event.IssueUID
	}
	from := movedFrom(o)
	visibleProject, departure := o.ProjectUID, false
	if !sub.selects(o.ProjectUID) {
		if from == "" || !sub.selects(from) {
			return false, nil
		}
		// A subscriber of the departure project learns of the move without
		// the destination or the issue.
		visibleProject, departure = from, true
	}
	project := strings.HasPrefix(o.Type, projectPrefix)
	reason := inboxEventReason(o, sub.args)
	if !project && reason == "" && !scopeChange(o) {
		return false, nil
	}
	auth, err := s.scanAuthority(ctx, sub.row, auths)
	if err != nil {
		return false, err
	}
	if project {
		if o.Type != "project.merged" && slices.Contains(catalogEvents, o.Type) {
			// refreshCatalogs decides these from each owner's visible projects.
			return false, nil
		}
		// A merge moves the source's requests into this project, and a
		// rename or other project change can change how its requests show.
		// Either concerns the owners who can see the project.
		return auth.visible[o.ProjectUID], nil
	}
	if auth.auth.Scoped() && scopeChange(o) {
		// The change can move requests in or out of the token's subtree, but
		// only inside a project the token can see. A move counts from either
		// end, since it takes descendants out of one project and into another.
		if o.Type == "issue.moved" {
			return (sub.selects(o.ProjectUID) && auth.visible[o.ProjectUID]) || (from != "" && sub.selects(from) && auth.visible[from]), nil
		}
		if !departure {
			return auth.visible[o.ProjectUID], nil
		}
	}
	if reason == "" {
		return false, nil
	}
	check := Occurrence{ProjectUID: visibleProject, IssueUID: o.IssueUID}
	if departure {
		check.IssueUID = ""
	}
	switch err := auth.auth.AuthorizeOccurrence(ctx, check); {
	case errors.Is(err, ErrOccurrenceHidden) && !departure && from != "" && sub.selects(from):
		// The issue moved somewhere the owner cannot see. The departure
		// project still lost it.
		return visible(ctx, auth.auth, Occurrence{ProjectUID: from})
	case errors.Is(err, ErrOccurrenceHidden):
		return false, nil
	case err != nil:
		return false, err
	}
	if !departure && o.IssueUID != "" && issueLifecycle(o) {
		return auth.auth.LifecycleMayChangeInbox(ctx, o.IssueUID, sub.args.For)
	}
	return true, nil
}

// visible reports whether the owner can see the occurrence now.
func visible(ctx context.Context, auth Authorized, o Occurrence) (bool, error) {
	switch err := auth.AuthorizeOccurrence(ctx, o); {
	case errors.Is(err, ErrOccurrenceHidden):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// authorityKey identifies the authority a row is checked under: its owner and
// the actor stored with it. A host can rename the actor behind one owner, and
// rows made before and after keep their own actor, so they are decided apart.
// Ownership stays the principal alone.
func authorityKey(row db.MCPEventSubscription) string {
	return row.PrincipalKey + "\x00" + row.Actor
}

// scanAuthority reacquires a principal once per scan pass.
func (s *Service) scanAuthority(ctx context.Context, row db.MCPEventSubscription, auths map[string]*scanAuthority) (*scanAuthority, error) {
	if cached, ok := auths[authorityKey(row)]; ok {
		return cached, cached.err
	}
	entry := &scanAuthority{}
	entry.auth, entry.err = s.authority.Reacquire(ctx, principalOf(row))
	if entry.err == nil {
		projects, err := entry.auth.VisibleProjects(ctx, nil)
		if err != nil {
			entry.err = err
		} else {
			entry.visible = make(map[string]bool, len(projects))
			entry.ids = make(map[string]int64, len(projects))
			for _, p := range projects {
				entry.visible[p.UID] = true
				entry.ids[p.UID] = p.ID
			}
		}
	}
	// An outage is cached for the rest of the pass too, so an owner whose
	// authority is down is not asked again for every event.
	auths[authorityKey(row)] = entry
	return entry, entry.err
}

// scopeChange reports an event that can move issues in or out of an issue
// subtree: a lifecycle change, a link change, a move, or a federated
// snapshot, which can delete, restore or reparent an issue in one step. A
// scoped credential's inbox can change on any of them even when the event's
// own issue holds no request.
func scopeChange(o Occurrence) bool {
	switch o.Type {
	case "issue.linked", "issue.unlinked", "issue.links_changed", "issue.snapshot", "issue.moved":
		return true
	}
	return issueLifecycle(o)
}

// movedFrom returns the departure project of an issue move, or "".
func movedFrom(o Occurrence) string {
	if o.Type != "issue.moved" {
		return ""
	}
	var move struct {
		From string `json:"from_project_uid"`
	}
	if json.Unmarshal(o.Payload, &move) != nil {
		return ""
	}
	return move.From
}
