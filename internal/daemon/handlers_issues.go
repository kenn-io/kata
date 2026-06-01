package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/similarity"
	"go.kenn.io/kata/internal/uid"
)

const minIssueUIDPrefixLen = 8

// registerIssuesHandlers installs the four issue routes (create/list/show/edit)
// on humaAPI. CreateIssue writes both the issue row and the matching
// issue.created event in one tx (see db.CreateIssue) so the response always
// carries an event for the CLI to render.
func registerIssuesHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "createIssue",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues",
	}, func(ctx context.Context, in *api.CreateIssueRequest) (*api.MutationResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}

		links, err := resolveInitialLinks(ctx, cfg.DB, in.ProjectID, in.Body.Links)
		if err != nil {
			return nil, err
		}

		// Validate priority before the idempotency lookup so an out-of-range
		// value is rejected with a 400 instead of being silently absorbed by a
		// reuse path that ignores the bad input. Priority also rides the
		// fingerprint, so idempotency_mismatch keys with different priorities
		// surface the prior issue rather than reusing it.
		if err := validatePriorityRange(in.Body.Priority); err != nil {
			return nil, err
		}

		// Idempotency runs before look-alike so it wins over force_new (§3.7).
		idempotencyFingerprint, reuse, err := tryIdempotencyMatch(ctx, cfg, in, links)
		if err != nil {
			return nil, err
		}
		if reuse != nil {
			return reuse, nil
		}
		if !in.Body.ForceNew {
			if err := runLookalikeCheck(ctx, cfg, in); err != nil {
				return nil, err
			}
		}
		issue, evt, err := cfg.DB.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID:              in.ProjectID,
			Title:                  in.Body.Title,
			Body:                   in.Body.Body,
			Author:                 actor,
			Owner:                  in.Body.Owner,
			Priority:               in.Body.Priority,
			Labels:                 in.Body.Labels,
			Links:                  links,
			IdempotencyKey:         in.IdempotencyKey,
			IdempotencyFingerprint: idempotencyFingerprint,
		})
		switch {
		case errors.Is(err, db.ErrInitialLinkInvalidType):
			return nil, api.NewError(400, "validation",
				"link.type must be parent|blocks|related", "", nil)
		case errors.Is(err, db.ErrInitialLinkTargetNotFound):
			return nil, api.NewError(404, "issue_not_found",
				"initial link target not found in this project", "", nil)
		case errors.Is(err, db.ErrSelfLink):
			return nil, api.NewError(400, "validation",
				"cannot link an issue to itself", "", nil)
		case errors.Is(err, db.ErrLabelInvalid):
			return nil, api.NewError(400, "validation",
				"label must match charset [a-z0-9._:-] and length 1..64", "", nil)
		case errors.Is(err, db.ErrParentAlreadySet):
			return nil, api.NewError(409, "parent_already_set",
				"duplicate parent in initial links", "pass at most one parent link", nil)
		case errors.Is(err, db.ErrFederatedReadOnly):
			return nil, federationReadOnlyError(err)
		case errors.Is(err, db.ErrNotFound):
			return nil, api.NewError(404, "project_not_found", "project not found", "", nil)
		case err != nil:
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: &evt, ProjectID: in.ProjectID})
		cfg.Hooks.Enqueue(evt)
		out := &api.MutationResponse{}
		out.Body.Issue = issue
		out.Body.Event = &evt
		out.Body.Changed = true
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "listIssues",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/issues",
	}, func(ctx context.Context, in *api.ListIssuesRequest) (*api.ListIssuesResponse, error) {
		// Validate mutual exclusion of --unowned and --owner
		if in.Unowned && in.Owner != "" {
			return nil, api.NewError(400, "validation",
				"--unowned and --owner are mutually exclusive", "", nil)
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		priority, err := parsePriorityQuery(in.Priority, "priority")
		if err != nil {
			return nil, err
		}
		maxPriority, err := parsePriorityQuery(in.MaxPriority, "max_priority")
		if err != nil {
			return nil, err
		}
		issues, err := cfg.DB.ListIssues(ctx, db.ListIssuesParams{
			ProjectID:     in.ProjectID,
			Status:        in.Status,
			Priority:      priority,
			MaxPriority:   maxPriority,
			Limit:         in.Limit,
			Unowned:       in.Unowned,
			Owner:         in.Owner,
			Labels:        in.Labels,
			ExcludeLabels: in.ExcludeLabels,
		})
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		issueOuts, err := hydrateIssueOuts(ctx, cfg.DB, in.ProjectID, issues)
		out := &api.ListIssuesResponse{}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out.Body.Issues = issueOuts
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "listAllIssues",
		Method:      "GET",
		Path:        "/api/v1/issues",
	}, func(ctx context.Context, in *api.ListAllIssuesRequest) (*api.ListIssuesResponse, error) {
		if in.DeprecatedView != "" || in.DeprecatedArea != "" ||
			in.DeprecatedOffset != "" || in.DeprecatedClientTZ != "" {
			return nil, api.NewError(400, "removed_param",
				"view, area, offset query params and the X-Kata-Client-TZ header were removed",
				"assemble named views (today/upcoming/inbox/someday/anytime/logbook) client-side from /api/v1/issues and /api/v1/projects",
				nil)
		}
		if in.ProjectID < 0 {
			return nil, api.NewError(400, "validation",
				"project_id must be a positive integer", "", nil)
		}
		if in.ProjectID > 0 {
			if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
				return nil, err
			}
		}
		priority, err := parsePriorityQuery(in.Priority, "priority")
		if err != nil {
			return nil, err
		}
		maxPriority, err := parsePriorityQuery(in.MaxPriority, "max_priority")
		if err != nil {
			return nil, err
		}
		issues, err := cfg.DB.ListAllIssues(ctx, db.ListAllIssuesParams{
			ProjectID:   in.ProjectID,
			Status:      in.Status,
			Priority:    priority,
			MaxPriority: maxPriority,
			Limit:       in.Limit,
		})
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		issueOuts, err := hydrateIssueOutsCrossProject(ctx, cfg.DB, issues)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.ListIssuesResponse{}
		out.Body.Issues = issueOuts
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "showIssue",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}",
	}, func(ctx context.Context, in *api.ShowIssueRequest) (*api.ShowIssueResponse, error) {
		include := db.IncludeDeletedNo
		if in.IncludeDeleted {
			include = db.IncludeDeletedYes
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, include)
		if err != nil {
			return nil, err
		}
		return buildShowIssueResponse(ctx, cfg, issue, in.IncludeDeleted)
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "showIssueByUID",
		Method:      "GET",
		Path:        "/api/v1/issues/{uid}",
	}, func(ctx context.Context, in *api.ShowIssueByUIDRequest) (*api.ShowIssueResponse, error) {
		include := db.IncludeDeletedNo
		if in.IncludeDeleted {
			include = db.IncludeDeletedYes
		}
		issue, err := resolveIssueByUIDOrPrefix(ctx, cfg.DB, in.UID, include)
		if err != nil {
			return nil, err
		}
		// Hide issues whose parent project is archived, mirroring every
		// other project-scoped handler. The UID lookup itself returns the
		// row regardless of project archive state.
		if _, perr := activeProjectByID(ctx, cfg.DB, issue.ProjectID); perr != nil {
			return nil, perr
		}
		return buildShowIssueResponse(ctx, cfg, issue, in.IncludeDeleted)
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "editIssue",
		Method:      "PATCH",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}",
	}, editIssueHandler(cfg))
}

// editIssueHandler dispatches a PATCH /issues/{ref} call. It applies any
// title/body/owner change, the priority change, and any LinksDelta mutations
// in a single daemon transaction. Reports applied link mutations in the
// response's `changes` block. Either every requested mutation lands or none
// do.
//
// Callers can pass only a links_delta (no title/body/owner) and the request
// is valid as long as the delta contains at least one mutation.
func editIssueHandler(cfg ServerConfig) func(context.Context, *api.EditIssueRequest) (*api.EditIssueResponse, error) {
	return func(ctx context.Context, in *api.EditIssueRequest) (*api.EditIssueResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		if err := requireFederatedIssueClaim(ctx, cfg, in.ProjectID, issue, actor); err != nil {
			return nil, err
		}

		hasFieldChange := in.Body.Title != nil || in.Body.Body != nil || in.Body.Owner != nil
		hasPriorityChange := in.Body.SetPriority != nil || in.Body.ClearPriority
		hasLinkChange := in.Body.LinksDelta != nil && linksDeltaNonEmpty(in.Body.LinksDelta)
		if !hasFieldChange && !hasPriorityChange && !hasLinkChange {
			return nil, api.NewError(400, "validation", "no fields to update",
				"pass at least one of title, body, owner, set_priority, clear_priority, or links_delta", nil)
		}
		if in.Body.SetPriority != nil && in.Body.ClearPriority {
			return nil, api.NewError(400, "validation",
				"cannot set_priority and clear_priority in the same call",
				"choose one", nil)
		}
		if err := validatePriorityRange(in.Body.SetPriority); err != nil {
			return nil, err
		}
		if hasLinkChange {
			if err := validateLinksDelta(in.Body.LinksDelta); err != nil {
				return nil, err
			}
		}

		params := db.EditIssueAtomicParams{
			IssueID:       issue.ID,
			Actor:         actor,
			Title:         in.Body.Title,
			Body:          in.Body.Body,
			Owner:         in.Body.Owner,
			SetPriority:   in.Body.SetPriority,
			ClearPriority: in.Body.ClearPriority,
		}
		if hasLinkChange {
			if err := fillLinksDeltaParams(ctx, cfg.DB, in.ProjectID, in.Body.LinksDelta, &params); err != nil {
				return nil, err
			}
			// Re-check for add/remove conflicts on resolved issue IDs, not
			// just raw ref strings. Catches the case where add and remove
			// list different ref forms ("abc4" vs the issue's full ULID)
			// that name the same issue — validateLinksDelta's string-eq
			// can't see this.
			if err := validateResolvedLinksDelta(&params); err != nil {
				return nil, err
			}
			if err := requireFederatedLinksDeltaClaims(ctx, cfg, in.ProjectID, actor, issue, &params); err != nil {
				return nil, err
			}
		}

		result, err := cfg.DB.EditIssueAtomic(ctx, params)
		if err != nil {
			return nil, mapAtomicEditError(err, issue.ShortID, in.Body.LinksDelta)
		}
		// Broadcast all events post-commit. Order matches DB.EditIssueAtomic's
		// emission order: issue.updated → priority → links_changed.
		for i := range result.Events {
			ev := result.Events[i]
			cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: &ev, ProjectID: in.ProjectID})
			cfg.Hooks.Enqueue(ev)
		}

		out := &api.EditIssueResponse{}
		out.Body.Issue = result.Issue
		out.Body.Changed = result.AnyChange
		// `events` carries every event in emission order so a client can
		// observe each transition (issue.updated, issue.priority_*,
		// issue.links_changed) — important for mixed PATCHes where the
		// priority transition would otherwise be hidden by an event
		// emitted later. `event` is retained as a compatibility alias
		// pointing at the LAST event for callers that only expected one.
		if len(result.Events) > 0 {
			out.Body.Events = make([]db.Event, len(result.Events))
			copy(out.Body.Events, result.Events)
			last := result.Events[len(result.Events)-1]
			out.Body.Event = &last
		}
		// `changes` is only present on relationship-bearing PATCHes — its
		// presence is the wire signal "this response describes link
		// mutations." Omit it entirely on field-only / priority-only
		// edits so older clients keying off its presence keep working.
		// The gate is "did the request actually ask for a link op", not
		// "is the links_delta field non-nil" — a `links_delta: {}`
		// envelope carries no operations and should be treated like the
		// field-only PATCH it functionally is.
		if linksDeltaRequestsAnyOp(in.Body.LinksDelta) {
			changes, err := buildLinkChanges(ctx, cfg.DB, result.Changes)
			if err != nil {
				return nil, api.NewError(500, "internal", err.Error(), "", nil)
			}
			out.Body.Changes = changes
		}
		return out, nil
	}
}

// linksDeltaRequestsAnyOp reports whether the delta carries at least one
// requested link operation. Used to decide whether the response should
// include the `changes` block: a non-nil but empty `links_delta` is
// treated like a field-only PATCH because no link op was actually asked
// for. Older clients key off the presence of `changes` to detect
// relationship mutations, so signal-fidelity matters.
func linksDeltaRequestsAnyOp(d *api.LinksDelta) bool {
	if d == nil {
		return false
	}
	return d.SetParent != nil || d.RemoveParent != nil ||
		len(d.AddBlocks) > 0 || len(d.AddBlockedBy) > 0 || len(d.AddRelated) > 0 ||
		len(d.RemoveBlocks) > 0 || len(d.RemoveBlockedBy) > 0 || len(d.RemoveRelated) > 0
}

// mapAtomicEditError translates DB-layer errors from EditIssueAtomic into
// the right API error envelope. Touches only error categories the atomic
// path can produce. issueShortID is the URL issue's short_id, used in
// human-readable error messages.
func mapAtomicEditError(err error, issueShortID string, delta *api.LinksDelta) error {
	var lt *db.LinkTargetNotFoundError
	switch {
	case errors.As(err, &lt):
		return api.NewError(404, "issue_not_found",
			"link target not found", "", nil)
	case errors.Is(err, db.ErrNotFound):
		return api.NewError(404, "issue_not_found",
			"target issue not found", "", nil)
	case errors.Is(err, db.ErrParentMismatch):
		assertion := ""
		if delta != nil && delta.RemoveParent != nil {
			assertion = *delta.RemoveParent
		}
		return api.NewError(409, "parent_mismatch",
			fmt.Sprintf("issue #%s's current parent does not match asserted #%s", issueShortID, assertion),
			"read the current parent before asserting a removal", nil)
	case errors.Is(err, db.ErrSelfLink):
		return api.NewError(400, "validation", "cannot link an issue to itself", "", nil)
	case errors.Is(err, db.ErrCrossProjectLink):
		return api.NewError(400, "validation", "cross-project links are not allowed", "", nil)
	case errors.Is(err, db.ErrParentCycle):
		return api.NewError(400, "validation",
			fmt.Sprintf("set_parent on #%s would create a parent cycle", issueShortID),
			"the requested parent is a descendant of this issue", nil)
	case errors.Is(err, db.ErrParentAlreadySet):
		// Should not surface from the atomic path (set_parent replaces),
		// but map cleanly if it ever does.
		return api.NewError(409, "parent_already_set", err.Error(), "", nil)
	case errors.Is(err, db.ErrFederatedReadOnly):
		return federationReadOnlyError(err)
	default:
		return api.NewError(500, "internal", err.Error(), "", nil)
	}
}

// validateLinksDelta rejects deltas that are internally contradictory before
// any mutation runs. Catches:
//   - set_parent + remove_parent in the same call
//   - the same (type, target) appearing in both an add list and the matching
//     remove list (e.g. add_blocks: [abc4] and remove_blocks: [abc4])
//
// Self-link detection lives in the per-link helpers (where we have the URL
// issue's ref to compare against).
func validateLinksDelta(d *api.LinksDelta) error {
	if d == nil {
		return nil
	}
	if d.SetParent != nil && d.RemoveParent != nil {
		return api.NewError(400, "validation",
			"links_delta cannot set_parent and remove_parent in the same call",
			"choose one", nil)
	}
	if conflict := firstConflict(d.AddBlocks, d.RemoveBlocks); conflict != "" {
		return api.NewError(400, "validation",
			fmt.Sprintf("links_delta conflict: blocks #%s appears in both add_blocks and remove_blocks", conflict),
			"", nil)
	}
	if conflict := firstConflict(d.AddBlockedBy, d.RemoveBlockedBy); conflict != "" {
		return api.NewError(400, "validation",
			fmt.Sprintf("links_delta conflict: blocked_by #%s appears in both add_blocked_by and remove_blocked_by", conflict),
			"", nil)
	}
	if conflict := firstConflict(d.AddRelated, d.RemoveRelated); conflict != "" {
		return api.NewError(400, "validation",
			fmt.Sprintf("links_delta conflict: related #%s appears in both add_related and remove_related", conflict),
			"", nil)
	}
	return nil
}

// validateResolvedLinksDelta is the canonical-ID conflict check that runs
// after fillLinksDeltaParams. validateLinksDelta catches obvious string
// duplicates before any DB lookup; this pass catches the harder case
// where add and remove use different ref forms ("abc4" vs the full ULID)
// that resolve to the same issue.
func validateResolvedLinksDelta(p *db.EditIssueAtomicParams) error {
	if id, ok := firstIDConflict(p.AddBlocks, p.RemoveBlocks); ok {
		return api.NewError(400, "validation",
			fmt.Sprintf("links_delta conflict: blocks issue id %d appears in both add_blocks and remove_blocks", id),
			"choose one", nil)
	}
	if id, ok := firstIDConflict(p.AddBlockedBy, p.RemoveBlockedBy); ok {
		return api.NewError(400, "validation",
			fmt.Sprintf("links_delta conflict: blocked_by issue id %d appears in both add_blocked_by and remove_blocked_by", id),
			"choose one", nil)
	}
	if id, ok := firstIDConflict(p.AddRelated, p.RemoveRelated); ok {
		return api.NewError(400, "validation",
			fmt.Sprintf("links_delta conflict: related issue id %d appears in both add_related and remove_related", id),
			"choose one", nil)
	}
	return nil
}

func requireFederatedLinksDeltaClaims(
	ctx context.Context,
	cfg ServerConfig,
	projectID int64,
	actor string,
	source db.Issue,
	p *db.EditIssueAtomicParams,
) error {
	ids := make(map[int64]struct{})
	addID := func(id int64) {
		if id != source.ID {
			ids[id] = struct{}{}
		}
	}
	addIDs := func(in []int64) {
		for _, id := range in {
			addID(id)
		}
	}
	if p.SetParent != nil {
		addID(*p.SetParent)
		existing, err := cfg.DB.ParentOf(ctx, source.ID)
		switch {
		case err == nil:
			if existing.ToIssueID != *p.SetParent {
				addID(existing.ToIssueID)
			}
		case errors.Is(err, db.ErrNotFound):
		default:
			return api.NewError(500, "internal", err.Error(), "", nil)
		}
	}
	if p.RemoveParent != nil {
		addID(*p.RemoveParent)
	}
	addIDs(p.AddBlocks)
	addIDs(p.AddBlockedBy)
	addIDs(p.AddRelated)
	addIDs(p.RemoveBlocks)
	addIDs(p.RemoveBlockedBy)
	addIDs(p.RemoveRelated)
	if len(ids) == 0 {
		return nil
	}

	issues := make([]db.Issue, 0, len(ids))
	for id := range ids {
		issue, err := cfg.DB.IssueByID(ctx, id)
		if err != nil {
			return api.NewError(500, "internal", err.Error(), "", nil)
		}
		issues = append(issues, issue)
	}
	return requireFederatedLinkClaims(ctx, cfg, projectID, actor, issues...)
}

// firstIDConflict reports the first int64 present in both slices.
func firstIDConflict(adds, removes []int64) (int64, bool) {
	if len(adds) == 0 || len(removes) == 0 {
		return 0, false
	}
	seen := make(map[int64]struct{}, len(adds))
	for _, n := range adds {
		seen[n] = struct{}{}
	}
	for _, n := range removes {
		if _, ok := seen[n]; ok {
			return n, true
		}
	}
	return 0, false
}

// firstConflict returns the first ref present in both slices, or "" when
// there is no overlap. Used by validateLinksDelta.
func firstConflict(adds, removes []string) string {
	if len(adds) == 0 || len(removes) == 0 {
		return ""
	}
	seen := make(map[string]struct{}, len(adds))
	for _, n := range adds {
		seen[n] = struct{}{}
	}
	for _, n := range removes {
		if _, ok := seen[n]; ok {
			return n
		}
	}
	return ""
}

// linksDeltaNonEmpty reports whether the delta contains at least one
// add or remove instruction. Callers use this to gate the empty-edit
// validation error.
func linksDeltaNonEmpty(d *api.LinksDelta) bool {
	if d == nil {
		return false
	}
	return d.SetParent != nil || d.RemoveParent != nil ||
		len(d.AddBlocks) > 0 || len(d.AddBlockedBy) > 0 || len(d.AddRelated) > 0 ||
		len(d.RemoveBlocks) > 0 || len(d.RemoveBlockedBy) > 0 || len(d.RemoveRelated) > 0
}

func resolveIssueByUIDOrPrefix(ctx context.Context, store db.Storage, ref string, include db.IncludeDeleted) (db.Issue, error) {
	// ULIDs are spec-defined as case-insensitive. Uppercase the ref
	// before validation/lookup so a user typing the lowercase form
	// they got from a copy-paste pipeline isn't told their input is
	// invalid. The normalized form also feeds the error messages, so
	// "no match for ABC12345" reads the same regardless of case.
	normalized := strings.ToUpper(ref)
	if uid.Valid(normalized) {
		issue, err := store.IssueByUID(ctx, normalized, include)
		if errors.Is(err, db.ErrNotFound) {
			return db.Issue{}, api.NewError(404, "issue_not_found",
				fmt.Sprintf("no issue matches uid %s", normalized), "", nil)
		}
		if err != nil {
			return db.Issue{}, api.NewError(500, "internal", err.Error(), "", nil)
		}
		return issue, nil
	}
	if len(normalized) < minIssueUIDPrefixLen {
		return db.Issue{}, api.NewError(400, "prefix_too_short",
			fmt.Sprintf("uid prefix %q must be at least %d characters", ref, minIssueUIDPrefixLen),
			"", nil)
	}
	if !uid.ValidPrefix(normalized) {
		return db.Issue{}, api.NewError(400, "validation",
			fmt.Sprintf("%q is not a valid ULID prefix (Crockford base32: 0-9, A-Z excluding I/L/O/U; first char 0-7)", ref),
			"", nil)
	}
	matches, err := store.IssueUIDPrefixMatch(ctx, normalized, 20, include)
	if err != nil {
		return db.Issue{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	switch len(matches) {
	case 0:
		return db.Issue{}, api.NewError(404, "issue_not_found",
			fmt.Sprintf("no issue matches uid prefix %s", normalized), "", nil)
	case 1:
		return matches[0], nil
	default:
		candidates := make([]string, 0, len(matches))
		for _, issue := range matches {
			candidates = append(candidates,
				fmt.Sprintf("%s (#%s project %d)", issue.UID, issue.ShortID, issue.ProjectID))
		}
		return db.Issue{}, api.NewError(409, "prefix_ambiguous",
			"uid prefix is ambiguous: "+strings.Join(candidates, ", "), "",
			map[string]any{"candidates": candidates})
	}
}

func buildShowIssueResponse(ctx context.Context, cfg ServerConfig, issue db.Issue, includeDeleted bool) (*api.ShowIssueResponse, error) {
	if issue.DeletedAt != nil && !includeDeleted {
		return nil, api.NewError(404, "issue_not_found",
			"issue not found",
			"pass include_deleted=true to view soft-deleted issues",
			nil)
	}
	comments, err := listComments(ctx, cfg.DB, issue.ID)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	links, err := loadLinkOuts(ctx, cfg.DB, issue.ID)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	labels, err := cfg.DB.LabelsByIssue(ctx, issue.ID)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	parent, err := loadParentRef(ctx, cfg.DB, issue)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	children, err := cfg.DB.ChildrenOfIssue(ctx, issue.ProjectID, issue.ID)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	childOuts, err := hydrateIssueOuts(ctx, cfg.DB, issue.ProjectID, children)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	out := &api.ShowIssueResponse{}
	out.Body.Issue = issue
	out.Body.Comments = comments
	out.Body.Links = links
	out.Body.Labels = labels
	out.Body.Parent = parent
	out.Body.Children = childOuts
	claimRelevant, err := showIssueClaimRelevant(ctx, cfg.DB, issue.ProjectID)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if !claimRelevant {
		return out, nil
	}
	if issue.DeletedAt != nil {
		return out, nil
	}
	if cfg.Auth.Token == "" && cfg.InsecureReadonly {
		return out, nil
	}
	refreshedHubNow, err := refreshShowClaimStatus(ctx, cfg, issue)
	if err != nil {
		return nil, err
	}
	if err := hydrateClaimViolationsForIssue(ctx, cfg.DB, issue, out); err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if err := hydrateClaimOutForIssue(ctx, cfg, issue, out); err != nil {
		return nil, err
	}
	if refreshedHubNow != nil && (out.Body.Lease != nil || len(out.Body.PendingLeases) > 0) {
		out.Body.ClaimHubNow = refreshedHubNow
		out.Body.LeaseHubNow = refreshedHubNow
	}
	return out, nil
}

func hydrateClaimViolationsForIssue(ctx context.Context, store db.Storage, issue db.Issue, out *api.ShowIssueResponse) error {
	violations, count, err := store.UnresolvedClaimViolationsForIssue(ctx, issue.ProjectID, issue.UID, 3)
	if err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	out.Body.ClaimViolations = claimViolationOuts(violations)
	out.Body.LeaseViolations = out.Body.ClaimViolations
	out.Body.ClaimViolationCount = &count
	out.Body.LeaseViolationCount = &count
	return nil
}

func claimViolationOuts(violations []db.ClaimViolationSummary) []api.ClaimViolationOut {
	out := make([]api.ClaimViolationOut, 0, len(violations))
	for _, v := range violations {
		out = append(out, api.ClaimViolationOut{
			EventID:                    v.EventID,
			EventUID:                   v.EventUID,
			IssueUID:                   v.IssueUID,
			ShortID:                    v.IssueShortID,
			OffendingEventUID:          v.OffendingEventUID,
			OffendingEventType:         v.OffendingEventType,
			OffendingOriginInstanceUID: v.OffendingOriginInstanceUID,
			Actor:                      v.Actor,
			Reason:                     v.Reason,
			At:                         v.At,
		})
	}
	return out
}

func issueRefFromDB(iss db.Issue, projectName string) api.IssueRef {
	return api.IssueRef{
		UID:         iss.UID,
		ShortID:     iss.ShortID,
		QualifiedID: qualifiedID(projectName, iss.ShortID),
		Title:       iss.Title,
		Status:      iss.Status,
	}
}

func loadParentRef(ctx context.Context, store db.Storage, issue db.Issue) (*api.IssueRef, error) {
	link, err := store.ParentOf(ctx, issue.ID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	parent, err := store.IssueByID(ctx, link.ToIssueID)
	if err != nil {
		return nil, err
	}
	project, err := store.ProjectByID(ctx, parent.ProjectID)
	if err != nil {
		return nil, err
	}
	ref := issueRefFromDB(parent, project.Name)
	return &ref, nil
}

// hydrateIssueOutsCrossProject hydrates labels/parent/child-counts for issues
// that may span multiple projects. Per-project hydration helpers
// (LabelsByIssues, ParentNumbersByIssues, ChildCountsByParents) all scope by
// project_id, so we group by ProjectID and run them per group, then assemble
// the IssueOut slice in the input order. Realistic project counts are tiny
// (≤10) so the per-group cost is bounded.
func hydrateIssueOutsCrossProject(ctx context.Context, store db.Storage, issues []db.Issue) ([]api.IssueOut, error) {
	if len(issues) == 0 {
		return []api.IssueOut{}, nil
	}
	byProject := map[int64][]db.Issue{}
	for _, iss := range issues {
		byProject[iss.ProjectID] = append(byProject[iss.ProjectID], iss)
	}
	rowsByID := make(map[int64]api.IssueOut, len(issues))
	for projectID, group := range byProject {
		hydrated, err := hydrateIssueOuts(ctx, store, projectID, group)
		if err != nil {
			return nil, err
		}
		for _, row := range hydrated {
			rowsByID[row.ID] = row
		}
	}
	out := make([]api.IssueOut, len(issues))
	for i, iss := range issues {
		out[i] = rowsByID[iss.ID]
	}
	return out, nil
}

func hydrateIssueOuts(ctx context.Context, store db.Storage, projectID int64, issues []db.Issue) ([]api.IssueOut, error) {
	project, err := store.ProjectByID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(issues))
	for i, iss := range issues {
		ids[i] = iss.ID
	}
	labelsByID, err := store.LabelsByIssues(ctx, projectID, ids)
	if err != nil {
		return nil, err
	}
	parentNumbers, err := store.ParentNumbersByIssues(ctx, projectID, ids)
	if err != nil {
		return nil, err
	}
	childCounts, err := store.ChildCountsByParents(ctx, projectID, ids)
	if err != nil {
		return nil, err
	}
	blocks, err := store.BlockNumbersByIssues(ctx, projectID, ids)
	if err != nil {
		return nil, err
	}
	blockedBy, err := store.BlockedByNumbersByIssues(ctx, projectID, ids)
	if err != nil {
		return nil, err
	}
	related, err := store.RelatedNumbersByIssues(ctx, projectID, ids)
	if err != nil {
		return nil, err
	}
	// Gather peer ids referenced by any relationship slice so we can resolve
	// each to LinkPeer{UID, ShortID} in one pass.
	peerCache := map[int64]api.LinkPeer{}
	collectPeer := func(id int64) error {
		if _, ok := peerCache[id]; ok {
			return nil
		}
		peer, err := store.IssueByID(ctx, id)
		if err != nil {
			return err
		}
		peerCache[id] = api.LinkPeer{UID: peer.UID, ShortID: peer.ShortID}
		return nil
	}
	for _, ids := range blocks {
		for _, id := range ids {
			if err := collectPeer(id); err != nil {
				return nil, err
			}
		}
	}
	for _, ids := range blockedBy {
		for _, id := range ids {
			if err := collectPeer(id); err != nil {
				return nil, err
			}
		}
	}
	for _, ids := range related {
		for _, id := range ids {
			if err := collectPeer(id); err != nil {
				return nil, err
			}
		}
	}
	for _, id := range parentNumbers {
		if err := collectPeer(id); err != nil {
			return nil, err
		}
	}
	out := make([]api.IssueOut, len(issues))
	for i, iss := range issues {
		row := api.IssueOut{
			Issue:       iss,
			QualifiedID: qualifiedID(project.Name, iss.ShortID),
			Labels:      labelsByID[iss.ID],
			Blocks:      peerSlice(peerCache, blocks[iss.ID]),
			BlockedBy:   peerSlice(peerCache, blockedBy[iss.ID]),
			Related:     peerSlice(peerCache, related[iss.ID]),
		}
		if parentID, ok := parentNumbers[iss.ID]; ok {
			if peer, ok := peerCache[parentID]; ok {
				sid := peer.ShortID
				row.ParentShortID = &sid
			}
		}
		if counts := childCounts[iss.ID]; counts.Total > 0 {
			row.ChildCounts = &counts
		}
		out[i] = row
	}
	return out, nil
}

// peerSlice projects a slice of peer issue ids onto LinkPeer entries using
// the cache, in the same order. Missing ids (the cache miss case) yield a
// zero-value LinkPeer rather than a panic so a transient lookup gap doesn't
// crash the list handler.
func peerSlice(cache map[int64]api.LinkPeer, ids []int64) []api.LinkPeer {
	if len(ids) == 0 {
		return nil
	}
	out := make([]api.LinkPeer, 0, len(ids))
	for _, id := range ids {
		out = append(out, cache[id])
	}
	return out
}

// loadLinkOuts fetches every link involving issueID, resolving both endpoint
// short_ids so the wire response carries LinkPeer (UID + short_id) for each
// side. One IssueByID call per endpoint is fine for show; pagination is a
// Plan 4 concern.
func loadLinkOuts(ctx context.Context, store db.Storage, issueID int64) ([]api.LinkOut, error) {
	rows, err := store.LinksByIssue(ctx, issueID)
	if err != nil {
		return nil, err
	}
	out := make([]api.LinkOut, 0, len(rows))
	for _, l := range rows {
		from, err := store.IssueByID(ctx, l.FromIssueID)
		if err != nil {
			return nil, err
		}
		to, err := store.IssueByID(ctx, l.ToIssueID)
		if err != nil {
			return nil, err
		}
		out = append(out, api.LinkOut{
			ID:        l.ID,
			ProjectID: l.ProjectID,
			From:      api.LinkPeer{UID: from.UID, ShortID: from.ShortID},
			To:        api.LinkPeer{UID: to.UID, ShortID: to.ShortID},
			Type:      l.Type,
			Author:    l.Author,
			CreatedAt: l.CreatedAt,
		})
	}
	return out, nil
}

// listComments fetches every comment attached to issueID in chronological
// order. Plan 1 ships no pagination; the show handler embeds the full slice.
func listComments(ctx context.Context, store db.Storage, issueID int64) ([]db.Comment, error) {
	return store.CommentsByIssue(ctx, issueID)
}

const (
	// idempotencyWindow is the 7-day lookback per spec §3.6.
	idempotencyWindow = 7 * 24 * time.Hour
	// similarityThreshold is the soft-block trigger per spec §3.7.
	similarityThreshold = 0.7
)

// tryIdempotencyMatch runs the §3.6 idempotency lookup. Returns the fingerprint
// (so the caller can fold it into the issue.created event payload) and, when a
// prior issue exists for the key, a complete reuse-envelope MutationResponse
// (the caller should return it directly). Returns the relevant 409 wire error
// for mismatch / soft-deleted cases. When IdempotencyKey is empty, returns
// ("", nil, nil) so the caller falls through to the look-alike check.
//
// Known limitation: the lookup → CreateIssue is not atomic. Two concurrent
// requests with the same Idempotency-Key can both miss the lookup and both
// insert a fresh issue. Closing the race requires either a daemon-level
// per-key mutex with bounded GC, or restructuring CreateIssue around
// BEGIN IMMEDIATE with an in-TX re-lookup. Deferred from Plan 3 — small
// in single-user CLI usage. Tracked under roborev Job 16791-1.
func tryIdempotencyMatch(ctx context.Context, cfg ServerConfig, in *api.CreateIssueRequest,
	links []db.InitialLink) (string, *api.MutationResponse, error) {
	if in.IdempotencyKey == "" {
		return "", nil, nil
	}
	// Compute both fingerprint forms: the new (deduped) form is what we
	// write for fresh creates and what most retries should match. The legacy
	// (non-deduped) form is what idempotency events produced before kata#1's
	// dedupe-in-Fingerprint change carry. Lookup accepts either so a retry
	// inside the existing idempotency window after upgrade doesn't trip
	// idempotency_mismatch on a logically-equivalent request.
	//
	// Known asymmetry: if a pre-kata#1 request stored a fingerprint over
	// duplicate-bearing links (e.g. `[A, A]`) and the post-upgrade retry
	// sends the same intent in deduped form (`[A]`), neither computed
	// fingerprint matches the stored hash because the stored hash captured
	// the duplicate cardinality and we cannot reconstruct it from the
	// retry alone. Surfaces as 409 idempotency_mismatch; the user resolves
	// it by sending a fresh key. The window self-heals after 7 days, so
	// this only affects retries crossing the upgrade boundary within the
	// window. Storing the count alongside the hash on new writes does not
	// help pre-upgrade entries, so we accept the gap rather than complicate
	// the storage shape.
	fp := db.Fingerprint(in.Body.Title, in.Body.Body, in.Body.Owner, in.Body.Labels, links, in.Body.Priority)
	fpLegacy := db.FingerprintLegacy(in.Body.Title, in.Body.Body, in.Body.Owner, in.Body.Labels, links, in.Body.Priority)
	since := time.Now().Add(-idempotencyWindow)
	match, err := cfg.DB.LookupIdempotency(ctx, in.ProjectID, in.IdempotencyKey, since)
	if err != nil {
		return "", nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if match == nil {
		return fp, nil, nil
	}
	if match.Fingerprint != fp && match.Fingerprint != fpLegacy {
		// Resolve the prior issue so the mismatch envelope carries UID +
		// short_id + qualified_id rather than the dropped numeric ref.
		prior, err := cfg.DB.IssueByID(ctx, match.IssueID)
		if err != nil {
			return "", nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		priorProject, err := cfg.DB.ProjectByID(ctx, prior.ProjectID)
		if err != nil {
			return "", nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		return "", nil, api.NewError(409, "idempotency_mismatch",
			"idempotency key matched a prior issue with a different fingerprint",
			"either use a fresh key, or send the exact same fields as the original",
			map[string]any{
				"uid":          prior.UID,
				"short_id":     prior.ShortID,
				"qualified_id": qualifiedID(priorProject.Name, prior.ShortID),
			})
	}
	existing, err := cfg.DB.IssueByID(ctx, match.IssueID)
	if err != nil {
		return "", nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if existing.DeletedAt != nil {
		existingProject, err := cfg.DB.ProjectByID(ctx, existing.ProjectID)
		if err != nil {
			return "", nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		return "", nil, api.NewError(409, "idempotency_deleted",
			"idempotency key matched a soft-deleted issue",
			"run `kata restore "+existing.ShortID+"` or use a fresh key",
			map[string]any{
				"uid":          existing.UID,
				"short_id":     existing.ShortID,
				"qualified_id": qualifiedID(existingProject.Name, existing.ShortID),
			})
	}
	// Copy the Event off the *IdempotencyMatch struct so OriginalEvent has a
	// stable address that doesn't alias the lookup result.
	origCopy := match.Event
	out := &api.MutationResponse{}
	out.Body.Issue = existing
	out.Body.Event = nil
	out.Body.OriginalEvent = &origCopy
	out.Body.Changed = false
	out.Body.Reused = true
	return fp, out, nil
}

// runLookalikeCheck runs the §3.7 soft-block: SearchFTSAny over title+body
// (OR-of-tokens for high recall), scores each candidate via similarity.Score,
// and returns a 409 duplicate_candidates error if any candidate is at or
// above the 0.7 threshold. nil means proceed. The OR variant is required
// because near-duplicates that differ by even one token would be filtered
// out by SearchFTS's implicit-AND before similarity scoring runs.
func runLookalikeCheck(ctx context.Context, cfg ServerConfig, in *api.CreateIssueRequest) error {
	q := strings.TrimSpace(in.Body.Title + " " + in.Body.Body)
	candidates, err := cfg.DB.SearchFTSAny(ctx, in.ProjectID, q, 20, false)
	if err != nil {
		return api.NewError(500, "internal", err.Error(), "", nil)
	}
	matched := []map[string]any{}
	for _, c := range candidates {
		score := similarity.Score(in.Body.Title, in.Body.Body, c.Issue.Title, c.Issue.Body)
		if score >= similarityThreshold {
			matched = append(matched, map[string]any{
				"uid":      c.Issue.UID,
				"short_id": c.Issue.ShortID,
				"title":    c.Issue.Title,
				"score":    score,
			})
		}
	}
	if len(matched) == 0 {
		return nil
	}
	return api.NewError(409, "duplicate_candidates",
		formatDuplicateMessage(matched),
		"comment on an existing issue, or pass force_new=true to create anyway",
		map[string]any{"candidates": matched})
}

// formatDuplicateMessage produces a singular/plural-aware human message for
// the duplicate_candidates 409 response.
func formatDuplicateMessage(matched []map[string]any) string {
	n := len(matched)
	if n == 1 {
		return "1 open issue matches this title"
	}
	return strconv.Itoa(n) + " open issues match this title"
}
