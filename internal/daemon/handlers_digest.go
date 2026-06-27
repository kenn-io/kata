package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func registerDigestHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "digestGlobal",
		Method:      "GET",
		Path:        "/api/v1/digest",
	}, func(ctx context.Context, in *api.DigestRequest) (*api.DigestResponse, error) {
		return doDigest(ctx, cfg, 0, in.Since, in.Until, in.Actors)
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "digestProject",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/digest",
	}, func(ctx context.Context, in *api.DigestProjectRequest) (*api.DigestResponse, error) {
		if in.ProjectID <= 0 {
			return nil, api.NewError(400, "validation",
				"project_id must be a positive integer", "", nil)
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		return doDigest(ctx, cfg, in.ProjectID, in.Since, in.Until, in.Actors)
	})
}

// doDigest is the shared implementation for both digest endpoints.
func doDigest(
	ctx context.Context,
	cfg ServerConfig,
	projectID int64,
	sinceStr, untilStr string,
	actors []string,
) (*api.DigestResponse, error) {
	since, err := time.Parse(time.RFC3339, sinceStr)
	if err != nil {
		return nil, api.NewError(400, "validation",
			"since must be an RFC3339 timestamp", "", nil)
	}
	var until time.Time
	if untilStr == "" {
		until = time.Now().UTC()
	} else {
		until, err = time.Parse(time.RFC3339, untilStr)
		if err != nil {
			return nil, api.NewError(400, "validation",
				"until must be an RFC3339 timestamp", "", nil)
		}
	}
	if !until.After(since) {
		return nil, api.NewError(400, "validation",
			"until must be strictly after since", "", nil)
	}

	rows, err := cfg.DB.EventsInWindow(ctx, db.EventsInWindowParams{
		Since:     since.UTC().Format("2006-01-02T15:04:05.000Z"),
		Until:     until.UTC().Format("2006-01-02T15:04:05.000Z"),
		ProjectID: projectID,
		Actors:    actors,
	})
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}

	out := &api.DigestResponse{}
	out.Body.Since = since.UTC()
	out.Body.Until = until.UTC()
	out.Body.ProjectID = projectID
	out.Body.EventCount = len(rows)
	out.Body.Totals, out.Body.Actors = aggregateDigest(rows)
	return out, nil
}

// issueKey identifies an issue across projects in the digest aggregator.
// The UID is globally unique on its own, but we still hold the project id
// because it scopes the rendering helpers (project_name comes off the
// event row regardless).
type issueKey struct {
	ProjectID int64
	UID       string
}

// issueAccum tracks the per-(actor,issue) action sequence. We collapse repeats
// (comments especially) to "verb:short_id" tokens so the human renderer
// doesn't drown in duplicate lines for chatty agents.
type issueAccum struct {
	ProjectName     string
	IssueShortID    string // joined from issues.short_id by EventsInWindow
	Created         bool
	CloseReason     string // empty if not closed in window
	Reopened        bool
	Edited          bool
	CommentCount    int
	AssignedTo      string // last value wins
	Unassigned      bool
	PrioritySet     *int64 // last value wins; nil = unchanged in window
	PriorityCleared bool
	LabelsAdded     []string
	LabelsRemoved   []string
	Linked          []string // formatted "type:short_id"
	Unlinked        []string
	UnblocksOthers  []string // short_ids this actor unblocked
	Deleted         bool
	Restored        bool
	Other           int
}

// aggregateDigest folds an ordered slice of events into the digest grand
// totals and per-actor sections.
func aggregateDigest(rows []db.Event) (api.DigestTotals, []api.DigestActorEntry) {
	type actorState struct {
		totals api.DigestTotals
		issues map[issueKey]*issueAccum
	}
	byActor := map[string]*actorState{}
	var grand api.DigestTotals

	for _, e := range rows {
		st, ok := byActor[e.Actor]
		if !ok {
			st = &actorState{issues: map[issueKey]*issueAccum{}}
			byActor[e.Actor] = st
		}
		// Some event types have no associated issue (none today, but be defensive).
		var key issueKey
		var acc *issueAccum
		if e.IssueUID != nil {
			key = issueKey{ProjectID: e.ProjectID, UID: *e.IssueUID}
			a, ok := st.issues[key]
			if !ok {
				a = &issueAccum{ProjectName: e.ProjectName}
				st.issues[key] = a
			}
			// Latest short_id observed for this issue wins so renders
			// reflect any post-merge / post-cutover shift visible at
			// digest time. UID keys keep the row stable regardless.
			if e.IssueShortID != nil && *e.IssueShortID != "" {
				a.IssueShortID = *e.IssueShortID
			}
			acc = a
		}
		applyEvent(e, &st.totals, &grand, acc)
	}

	// Cross-actor enrichment: for every issue.unlinked of type=blocks the
	// blocker side gets credit for an "unblocks #blocked" action. The
	// aggregator stores this on the actor who emitted the unlink event.
	// (No additional pass needed — already handled in applyEvent.)

	out := make([]api.DigestActorEntry, 0, len(byActor))
	for actor, st := range byActor {
		entry := api.DigestActorEntry{
			Actor:  actor,
			Totals: st.totals,
		}
		entry.Issues = renderIssueAccums(st.issues)
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Actor < out[j].Actor })
	return grand, out
}

// applyEvent classifies a single event and updates totals + per-issue state.
// totals and grand are both bumped (per-actor and overall).
func applyEvent(e db.Event, totals, grand *api.DigestTotals, acc *issueAccum) {
	switch e.Type {
	case "issue.created":
		bump(&totals.Created, &grand.Created)
		if acc != nil {
			acc.Created = true
		}
		// CreateIssue folds initial labels/owner/links into the same payload
		// rather than emitting separate issue.labeled/assigned/linked events,
		// so the digest must mine them out here or it undercounts common
		// create-time activity (e.g. `kata create --label bug --owner alice`).
		labels, owner, links := createdInitialState(e.Payload)
		for _, l := range labels {
			bump(&totals.Labeled, &grand.Labeled)
			if acc != nil {
				acc.LabelsAdded = append(acc.LabelsAdded, l)
			}
		}
		if owner != "" {
			bump(&totals.Assigned, &grand.Assigned)
			if acc != nil {
				acc.AssignedTo = owner
			}
		}
		for _, lk := range links {
			bump(&totals.Linked, &grand.Linked)
			if acc != nil && lk.Type != "" {
				// An inverse-direction blocks link (`--blocked-by N` on
				// create) is stored as type="blocks" with incoming=true;
				// the digest's per-edge label flips to "blocked_by" so the
				// summary reads in the issue's POV — without this, a
				// `--blocked-by` create would surface as `blocks:short_id`
				// and reverse the dependency direction in reports.
				label := lk.Type
				if lk.Type == "blocks" && lk.Incoming {
					label = "blocked_by"
				}
				acc.Linked = append(acc.Linked, fmt.Sprintf("%s:%s", label, lk.ToShortID))
			}
		}
	case "issue.closed":
		bump(&totals.Closed, &grand.Closed)
		if acc != nil {
			acc.CloseReason = closeReasonOf(e.Payload)
			if acc.CloseReason == "" {
				acc.CloseReason = "done"
			}
		}
	case "issue.reopened":
		bump(&totals.Reopened, &grand.Reopened)
		if acc != nil {
			acc.Reopened = true
		}
	case "issue.commented":
		bump(&totals.Commented, &grand.Commented)
		if acc != nil {
			acc.CommentCount++
		}
	case "issue.updated":
		bump(&totals.Edited, &grand.Edited)
		if acc != nil {
			acc.Edited = true
		}
	case "issue.comment_edited":
		bump(&totals.Edited, &grand.Edited)
		if acc != nil {
			acc.Edited = true
		}
	case "issue.assigned":
		bump(&totals.Assigned, &grand.Assigned)
		if acc != nil {
			acc.AssignedTo = ownerOf(e.Payload)
		}
	case "issue.unassigned":
		bump(&totals.Unassigned, &grand.Unassigned)
		if acc != nil {
			acc.Unassigned = true
		}
	case "issue.priority_set":
		bump(&totals.PrioritySet, &grand.PrioritySet)
		if acc != nil {
			if p, ok := priorityOf(e.Payload); ok {
				v := p
				acc.PrioritySet = &v
			}
		}
	case "issue.priority_cleared":
		bump(&totals.PriorityCleared, &grand.PriorityCleared)
		if acc != nil {
			acc.PriorityCleared = true
		}
	case "issue.labeled":
		bump(&totals.Labeled, &grand.Labeled)
		if acc != nil {
			if l := labelOf(e.Payload); l != "" {
				acc.LabelsAdded = append(acc.LabelsAdded, l)
			}
		}
	case "issue.unlabeled":
		bump(&totals.Unlabeled, &grand.Unlabeled)
		if acc != nil {
			if l := labelOf(e.Payload); l != "" {
				acc.LabelsRemoved = append(acc.LabelsRemoved, l)
			}
		}
	case "issue.linked":
		bump(&totals.Linked, &grand.Linked)
		if acc != nil {
			if t, to := linkSummary(e.Payload); t != "" {
				acc.Linked = append(acc.Linked, fmt.Sprintf("%s:%s", t, to))
			}
		}
	case "issue.unlinked":
		bump(&totals.Unlinked, &grand.Unlinked)
		t, to := linkSummary(e.Payload)
		if acc != nil && t != "" {
			acc.Unlinked = append(acc.Unlinked, fmt.Sprintf("%s:%s", t, to))
		}
		// "blocked-on-X resolved": when the blocker side explicitly removes
		// the blocks edge, credit them with unblocking the other issue. The
		// link payload uses from_short_id = the URL issue (the blocker,
		// whose edit removes the link), so to_short_id is the issue that
		// becomes unblocked.
		if t == "blocks" && acc != nil && to != "" {
			acc.UnblocksOthers = append(acc.UnblocksOthers, to)
			bump(&totals.Unblocked, &grand.Unblocked)
		}
	case "issue.links_changed":
		// Aggregated event from the PATCH /issues/{n} path (kata#1).
		// One event covers any combination of adds and removes; we
		// expand it back into per-edge digest accounting so reports
		// look the same regardless of which API path produced the
		// link mutation.
		applyLinksChangedDigest(e.Payload, totals, grand, acc)
	case "issue.soft_deleted":
		bump(&totals.Deleted, &grand.Deleted)
		if acc != nil {
			acc.Deleted = true
		}
	case "issue.restored":
		bump(&totals.Restored, &grand.Restored)
		if acc != nil {
			acc.Restored = true
		}
	default:
		bump(&totals.Other, &grand.Other)
		if acc != nil {
			acc.Other++
		}
	}
}

func bump(local, grand *int) { *local++; *grand++ }

// closeReasonOf extracts the close reason from an issue.closed payload.
// Returns "" on parse failure so the caller can default to "done".
func closeReasonOf(payload string) string {
	if payload == "" {
		return ""
	}
	var p struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return ""
	}
	return p.Reason
}

// priorityOf extracts the new priority from an issue.priority_set payload.
// Returns ok=false on parse failure or when the field is missing.
func priorityOf(payload string) (int64, bool) {
	if payload == "" {
		return 0, false
	}
	var p struct {
		Priority *int64 `json:"priority"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil || p.Priority == nil {
		return 0, false
	}
	return *p.Priority, true
}

func ownerOf(payload string) string {
	if payload == "" {
		return ""
	}
	var p struct {
		Owner string `json:"owner"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return ""
	}
	return p.Owner
}

func labelOf(payload string) string {
	if payload == "" {
		return ""
	}
	var p struct {
		Label string `json:"label"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return ""
	}
	return p.Label
}

// createdInitialState mines an issue.created payload for any initial labels,
// owner, and links that CreateIssue folds in instead of emitting separate
// issue.labeled/assigned/linked events. Returns zero values on parse failure
// so the caller treats malformed payloads as "no extras" rather than crashing.
func createdInitialState(payload string) ([]string, string, []createdLink) {
	if payload == "" {
		return nil, "", nil
	}
	var p struct {
		Labels []string      `json:"labels"`
		Owner  string        `json:"owner"`
		Links  []createdLink `json:"links"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return nil, "", nil
	}
	return p.Labels, p.Owner, p.Links
}

type createdLink struct {
	Type      string `json:"type"`
	ToShortID string `json:"to_short_id"`
	Incoming  bool   `json:"incoming,omitempty"`
}

// linkSummary extracts the (type, to_short_id) pair from an
// issue.linked / issue.unlinked payload. Returns empty strings on parse
// failure.
func linkSummary(payload string) (string, string) {
	if payload == "" {
		return "", ""
	}
	var p struct {
		Type      string `json:"type"`
		ToShortID string `json:"to_short_id"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return "", ""
	}
	return p.Type, p.ToShortID
}

// applyLinksChangedDigest expands an issue.links_changed payload into the
// same per-edge digest accounting that issue.linked / issue.unlinked use,
// so reports do not vary by API path. The aggregated event always carries
// the issue's POV (parent/blocks/blocked_by/related, each with adds and
// removes); a single edit may produce many digest entries.
//
// Per spec §9.2 the *_added / *_removed fields are short_id strings; the
// digest renders "type:short_id" tokens directly.
func applyLinksChangedDigest(payload string, totals, grand *api.DigestTotals, acc *issueAccum) {
	if payload == "" {
		return
	}
	var p struct {
		ParentSet        *string  `json:"parent_set,omitempty"`
		ParentRemoved    *string  `json:"parent_removed,omitempty"`
		BlocksAdded      []string `json:"blocks_added,omitempty"`
		BlocksRemoved    []string `json:"blocks_removed,omitempty"`
		BlockedByAdded   []string `json:"blocked_by_added,omitempty"`
		BlockedByRemoved []string `json:"blocked_by_removed,omitempty"`
		RelatedAdded     []string `json:"related_added,omitempty"`
		RelatedRemoved   []string `json:"related_removed,omitempty"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return
	}
	add := func(linkType, target string) {
		bump(&totals.Linked, &grand.Linked)
		if acc != nil {
			acc.Linked = append(acc.Linked, fmt.Sprintf("%s:%s", linkType, target))
		}
	}
	remove := func(linkType, target string, alsoUnblocks bool) {
		bump(&totals.Unlinked, &grand.Unlinked)
		if acc != nil {
			acc.Unlinked = append(acc.Unlinked, fmt.Sprintf("%s:%s", linkType, target))
		}
		if alsoUnblocks && acc != nil && target != "" {
			acc.UnblocksOthers = append(acc.UnblocksOthers, target)
			bump(&totals.Unblocked, &grand.Unblocked)
		}
	}

	if p.ParentSet != nil {
		add("parent", *p.ParentSet)
	}
	if p.ParentRemoved != nil {
		remove("parent", *p.ParentRemoved, false)
	}
	for _, n := range p.BlocksAdded {
		add("blocks", n)
	}
	for _, n := range p.BlocksRemoved {
		remove("blocks", n, true)
	}
	for _, n := range p.BlockedByAdded {
		add("blocked_by", n)
	}
	// blocked_by_removed deletes the same `blocks` edge as blocks_removed
	// would from the opposite POV — but the actor semantics differ.
	// `kata edit X --remove-blocked-by Y` means X is no longer blocked by
	// Y; X did not unblock anyone. We surface this as a plain unlink from
	// X's POV (no UnblocksOthers credit, no Unblocked-total bump) so the
	// digest doesn't misattribute the action to X. The mirror operation
	// (`kata edit Y --remove-blocks X`) still credits Y with unblocking.
	for _, n := range p.BlockedByRemoved {
		remove("blocked_by", n, false)
	}
	for _, n := range p.RelatedAdded {
		add("related", n)
	}
	for _, n := range p.RelatedRemoved {
		remove("related", n, false)
	}
}

// renderIssueAccums turns the per-issue accumulators into the wire shape:
// one DigestIssueActions per issue with a stable, ordered list of action
// tokens. Issues are sorted by (project_id, uid).
func renderIssueAccums(m map[issueKey]*issueAccum) []api.DigestIssueActions {
	keys := make([]issueKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ProjectID != keys[j].ProjectID {
			return keys[i].ProjectID < keys[j].ProjectID
		}
		return keys[i].UID < keys[j].UID
	})
	out := make([]api.DigestIssueActions, 0, len(keys))
	for _, k := range keys {
		acc := m[k]
		if acc == nil {
			continue
		}
		out = append(out, api.DigestIssueActions{
			ProjectID:    k.ProjectID,
			ProjectName:  acc.ProjectName,
			IssueUID:     k.UID,
			IssueShortID: acc.IssueShortID,
			Actions:      renderActions(acc),
		})
	}
	return out
}

// renderActions emits action tokens in canonical order — created first
// (creation context), then comment count, edits, ownership, labels, links,
// state changes (closed/reopened), and finally lifecycle (deleted/restored).
// The fixed order keeps the human renderer deterministic and avoids surfacing
// "closed" before "created" when both happen in the same window.
func renderActions(acc *issueAccum) []string {
	var actions []string
	if acc.Created {
		actions = append(actions, "created")
	}
	if acc.CommentCount == 1 {
		actions = append(actions, "commented")
	} else if acc.CommentCount > 1 {
		actions = append(actions, fmt.Sprintf("commented:%d", acc.CommentCount))
	}
	if acc.Edited {
		actions = append(actions, "edited")
	}
	if acc.AssignedTo != "" {
		actions = append(actions, "assigned:"+acc.AssignedTo)
	}
	if acc.Unassigned {
		actions = append(actions, "unassigned")
	}
	if acc.PrioritySet != nil {
		actions = append(actions, fmt.Sprintf("priority:%d", *acc.PrioritySet))
	}
	if acc.PriorityCleared {
		actions = append(actions, "priority_cleared")
	}
	for _, l := range acc.LabelsAdded {
		actions = append(actions, "labeled:"+l)
	}
	for _, l := range acc.LabelsRemoved {
		actions = append(actions, "unlabeled:"+l)
	}
	actions = append(actions, acc.Linked...)
	for _, u := range acc.Unlinked {
		actions = append(actions, "un"+u)
	}
	for _, n := range acc.UnblocksOthers {
		actions = append(actions, "unblocks:"+n)
	}
	if acc.Reopened {
		actions = append(actions, "reopened")
	}
	if acc.CloseReason != "" {
		actions = append(actions, "closed:"+acc.CloseReason)
	}
	if acc.Deleted {
		actions = append(actions, "deleted")
	}
	if acc.Restored {
		actions = append(actions, "restored")
	}
	if acc.Other > 0 {
		actions = append(actions, fmt.Sprintf("other:%d", acc.Other))
	}
	// Empty action list shouldn't really happen, but be defensive: an issue
	// with only "issue.foo"-style unknown events would otherwise render as
	// an empty bullet. Drop the trailing empty.
	if len(actions) == 0 {
		actions = []string{"-"}
	}
	return dedupe(actions)
}

// dedupe drops adjacent duplicates while preserving order. We get duplicates
// when, e.g., the same label is added and removed twice — surfacing the dupes
// is noise; collapsing them keeps the report scannable.
func dedupe(in []string) []string {
	if len(in) <= 1 {
		return in
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		key := strings.ToLower(s)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out
}
