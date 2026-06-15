package daemon

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
)

// resolveIssueRef parses an URL path component {ref} (short_id, qualified
// short_id, or 26-char ULID) and returns the matching issue.
//
// include controls soft-deleted visibility per spec §6: normal read/mutate
// paths pass IncludeDeletedNo; restore/idempotent-delete/purge/
// idempotency-collision paths pass IncludeDeletedYes.
//
// Cross-project guard: a ULID-based GET on
// /projects/{project_id}/issues/{ref} must still match project_id. A ULID
// that resolves to a different project is reported as issue_not_found so
// the URL path can't be used to fish across projects. The same guard
// applies to qualified short_id refs ("other#abc4"): if the qualifier
// names a different project than the URL's project_id, the lookup is
// rejected — otherwise a same-suffix issue in the URL project could be
// silently substituted for the one the qualifier intended.
func resolveIssueRef(ctx context.Context, store db.Storage, projectID int64, ref string, include db.IncludeDeleted) (db.Issue, error) {
	parsed, err := shortid.Parse(ref)
	if err != nil {
		return db.Issue{}, api.NewError(404, "issue_not_found", "issue not found", "", nil)
	}
	if parsed.ULID != "" {
		issue, err := store.IssueByUID(ctx, parsed.ULID, include)
		if errors.Is(err, db.ErrNotFound) {
			return db.Issue{}, api.NewError(404, "issue_not_found", "issue not found", "", nil)
		}
		if err != nil {
			return db.Issue{}, api.NewError(500, "internal", err.Error(), "", nil)
		}
		if issue.ProjectID != projectID {
			return db.Issue{}, api.NewError(404, "issue_not_found", "issue not found", "", nil)
		}
		return issue, nil
	}
	if parsed.Project != "" {
		project, err := store.ProjectByID(ctx, projectID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return db.Issue{}, api.NewError(404, "issue_not_found", "issue not found", "", nil)
			}
			return db.Issue{}, api.NewError(500, "internal", err.Error(), "", nil)
		}
		if parsed.Project != project.Name {
			return db.Issue{}, api.NewError(404, "issue_not_found", "issue not found", "", nil)
		}
	}
	issue, err := store.IssueByShortID(ctx, projectID, parsed.ShortID, include)
	if errors.Is(err, db.ErrNotFound) {
		return db.Issue{}, api.NewError(404, "issue_not_found", "issue not found", "", nil)
	}
	if err != nil {
		return db.Issue{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	return issue, nil
}

// activeIssueByRef gates resolveIssueRef on the parent project's archive
// state first (mirroring the surface-API contract that archived projects
// return project_not_found). Internal callers that need to operate on
// issues whose parent project is archived should call resolveIssueRef
// directly.
func activeIssueByRef(ctx context.Context, store db.Storage, projectID int64, ref string, include db.IncludeDeleted) (db.Issue, error) {
	if _, err := activeProjectByID(ctx, store, projectID); err != nil {
		return db.Issue{}, err
	}
	return resolveIssueRef(ctx, store, projectID, ref, include)
}

// qualifiedID renders the cross-project canonical form "project#short_id".
// Spec §3: qualified form parses by splitting on the last "#".
func qualifiedID(projectName, shortID string) string {
	return projectName + "#" + shortID
}

// resolveLinkTargetRef resolves a link-target ref. Unlike resolveIssueRef
// (the URL-subject resolver, which scopes every ref form to the URL project
// as an anti-fishing guard), link targets may live in any project: a bare
// short_id resolves in the subject issue's project, a qualified
// "project#short_id" in the named project, and a 26-char ULID globally. A
// miss is a plain issue_not_found — it means "not found anywhere", and
// single-token auth makes that no leak.
func resolveLinkTargetRef(ctx context.Context, store db.Storage, subjectProjectID int64, ref string, include db.IncludeDeleted) (db.Issue, error) {
	notFound := api.NewError(404, "issue_not_found", "issue not found", "", nil)
	parsed, err := shortid.Parse(ref)
	if err != nil {
		return db.Issue{}, notFound
	}
	if parsed.ULID != "" {
		issue, err := store.IssueByUID(ctx, parsed.ULID, include)
		if errors.Is(err, db.ErrNotFound) {
			return db.Issue{}, notFound
		}
		if err != nil {
			return db.Issue{}, api.NewError(500, "internal", err.Error(), "", nil)
		}
		return issue, nil
	}
	projectID := subjectProjectID
	if parsed.Project != "" {
		// Archived projects resolve here so removes can clean up existing
		// links; adds reject archived targets in requireLinkTargetAddable.
		project, err := store.ProjectByNameIncludingArchived(ctx, parsed.Project)
		if errors.Is(err, db.ErrNotFound) {
			return db.Issue{}, notFound
		}
		if err != nil {
			return db.Issue{}, api.NewError(500, "internal", err.Error(), "", nil)
		}
		projectID = project.ID
	}
	issue, err := store.IssueByShortID(ctx, projectID, parsed.ShortID, include)
	if errors.Is(err, db.ErrNotFound) {
		return db.Issue{}, notFound
	}
	if err != nil {
		return db.Issue{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	return issue, nil
}

// requireLinkTargetAddable gates ADD-side link targets: a peer whose project
// is archived cannot gain new links (409 link_target_archived, naming the
// project), while removes never call this — existing links to archived
// projects must always be removable. The subject issue's own project is
// already gated active by the handler (activeIssueByRef / activeProjectByID).
// The TARGET's project is checked regardless of which ref form resolved it,
// so a ULID that lands on an archived-project issue is rejected too.
func requireLinkTargetAddable(ctx context.Context, store db.Storage, subjectProjectID int64, target db.Issue) error {
	if target.ProjectID == subjectProjectID {
		return nil
	}
	project, err := store.ProjectByID(ctx, target.ProjectID)
	if err != nil {
		return api.NewError(500, "internal", err.Error(), "", nil)
	}
	if project.DeletedAt != nil {
		return api.NewError(409, "link_target_archived",
			fmt.Sprintf("link target %s is in archived project %q",
				qualifiedID(project.Name, target.ShortID), project.Name),
			"unarchive the project to add links", nil)
	}
	return nil
}

// validateInitialLinkType rejects an initial link whose type the DB layer
// would reject with db.ErrInitialLinkInvalidType, returning the byte-identical
// 400 the create handler maps for that error. Mirrors the DB's rule (queries.go):
// only parent|blocks|related are valid, and type=parent has no incoming form
// (a child-side link is filed from the child's POV via type=parent). Running
// this before target resolution keeps a malformed type a 400 even when the
// target is a claimed foreign issue.
func validateInitialLinkType(l api.CreateInitialLinkBody) error {
	invalid := api.NewError(400, "validation",
		"link.type must be parent|blocks|related", "", nil)
	switch l.Type {
	case "parent":
		if l.Incoming {
			return invalid
		}
		return nil
	case "blocks", "related":
		return nil
	default:
		return invalid
	}
}

// resolveInitialLinks turns CreateInitialLinkBody entries (string ToRef) into
// db.InitialLink entries (int64 ToNumber, which the db layer treats as an
// issue row id). Soft-deleted targets are excluded — initial-link creation
// must reject hidden peers per spec §6.
//
// This does only pure resolution and link-type validation so its result feeds
// the idempotency fingerprint. The archived-target gate
// (requireInitialLinkTargetsAddable) and the federated claim gate
// (requireFederatedLinkClaims) are state-dependent and must run only on a
// fresh create — AFTER the idempotency lookup — so a retry of an
// already-successful create still returns the stored reuse envelope even if a
// target's project was archived or its claim changed since the first request.
//
// The second return value contains the resolved target issues in input order
// for those two gates.
func resolveInitialLinks(ctx context.Context, store db.Storage, projectID int64, links []api.CreateInitialLinkBody) ([]db.InitialLink, []db.Issue, error) {
	out := make([]db.InitialLink, 0, len(links))
	targets := make([]db.Issue, 0, len(links))
	for _, l := range links {
		// Validate link type before resolving the target, so an invalid type
		// is a 400 even when the target is a claimed foreign issue (the
		// federated claim gate runs after this resolver). The DB applies the
		// same rule via db.ErrInitialLinkInvalidType; matching it here keeps a
		// malformed request a validation error rather than a claim_denied 409.
		if err := validateInitialLinkType(l); err != nil {
			return nil, nil, err
		}
		target, err := resolveLinkTargetRef(ctx, store, projectID, l.ToRef, db.IncludeDeletedNo)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, db.InitialLink{
			Type:     l.Type,
			ToNumber: target.ID,
			Incoming: l.Incoming,
		})
		targets = append(targets, target)
	}
	return out, targets, nil
}

// requireInitialLinkTargetsAddable gates each resolved initial-link target
// through requireLinkTargetAddable (rejecting peers in archived projects).
// Initial links are always adds. Run this only on a fresh create — after the
// idempotency lookup finds no reuse — so an idempotent retry is not refused by
// an archive that happened after the original create succeeded.
func requireInitialLinkTargetsAddable(ctx context.Context, store db.Storage, projectID int64, targets []db.Issue) error {
	for _, target := range targets {
		if err := requireLinkTargetAddable(ctx, store, projectID, target); err != nil {
			return err
		}
	}
	return nil
}

// fillLinksDeltaParams resolves each api.LinksDelta string ref into an
// issue id and stuffs the int64-keyed slices into params. Refs resolve
// through resolveLinkTargetRef, which maps misses to a plain issue_not_found
// 404, so error returns are wire-ready. Add paths (set_parent, add_*)
// additionally gate the target through requireLinkTargetAddable, which
// rejects peers in archived projects; remove paths never gate so existing
// links to archived peers can always be cleaned up.
func fillLinksDeltaParams(ctx context.Context, store db.Storage, projectID int64, d *api.LinksDelta, params *db.EditIssueAtomicParams) error {
	if d == nil {
		return nil
	}
	resolve := func(ref string, include db.IncludeDeleted) (db.Issue, error) {
		return resolveLinkTargetRef(ctx, store, projectID, ref, include)
	}
	// resolveAdd is the add-side variant: resolve then gate the target so a
	// peer in an archived project is rejected before any mutation runs.
	resolveAdd := func(ref string, include db.IncludeDeleted) (int64, error) {
		issue, err := resolve(ref, include)
		if err != nil {
			return 0, err
		}
		if err := requireLinkTargetAddable(ctx, store, projectID, issue); err != nil {
			return 0, err
		}
		return issue.ID, nil
	}
	resolveAddSlice := func(refs []string, include db.IncludeDeleted) ([]int64, error) {
		if len(refs) == 0 {
			return nil, nil
		}
		ids := make([]int64, 0, len(refs))
		for _, r := range refs {
			id, err := resolveAdd(r, include)
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, nil
	}
	// resolveSliceTolerant is the idempotent-remove variant: misses
	// (issue_not_found) are silently dropped instead of surfacing 404, and
	// no addable gate is applied. The desired end state — "no link from this
	// issue to N" — already holds when there is no N at all, so the remove
	// is a no-op.
	resolveSliceTolerant := func(refs []string, include db.IncludeDeleted) ([]int64, error) {
		if len(refs) == 0 {
			return nil, nil
		}
		ids := make([]int64, 0, len(refs))
		for _, r := range refs {
			issue, err := resolve(r, include)
			if err != nil {
				var ae *api.APIError
				if errors.As(err, &ae) && ae.Status == 404 {
					continue
				}
				return nil, err
			}
			ids = append(ids, issue.ID)
		}
		return ids, nil
	}
	if d.SetParent != nil {
		id, err := resolveAdd(*d.SetParent, db.IncludeDeletedNo)
		if err != nil {
			return err
		}
		params.SetParent = &id
	}
	if d.RemoveParent != nil {
		// Remove paths must tolerate a soft-deleted peer (the link row is
		// still live; the user can still ask to clean it up) and never gate.
		issue, err := resolve(*d.RemoveParent, db.IncludeDeletedYes)
		if err != nil {
			return err
		}
		params.RemoveParent = &issue.ID
	}
	var err error
	if params.AddBlocks, err = resolveAddSlice(d.AddBlocks, db.IncludeDeletedNo); err != nil {
		return err
	}
	if params.AddBlockedBy, err = resolveAddSlice(d.AddBlockedBy, db.IncludeDeletedNo); err != nil {
		return err
	}
	if params.AddRelated, err = resolveAddSlice(d.AddRelated, db.IncludeDeletedNo); err != nil {
		return err
	}
	if params.RemoveBlocks, err = resolveSliceTolerant(d.RemoveBlocks, db.IncludeDeletedYes); err != nil {
		return err
	}
	if params.RemoveBlockedBy, err = resolveSliceTolerant(d.RemoveBlockedBy, db.IncludeDeletedYes); err != nil {
		return err
	}
	if params.RemoveRelated, err = resolveSliceTolerant(d.RemoveRelated, db.IncludeDeletedYes); err != nil {
		return err
	}
	return nil
}

// buildLinkChanges projects db.AtomicEditChanges into the wire-facing
// api.LinkChanges. PeerIdentity carries the project name captured in-tx,
// so no storage lookup is required.
func buildLinkChanges(changes db.AtomicEditChanges) *api.LinkChanges {
	peer := func(p db.PeerIdentity) api.LinkPeer {
		return api.LinkPeer{
			UID:         p.UID,
			ShortID:     p.ShortID,
			Project:     p.Project,
			QualifiedID: qualifiedID(p.Project, p.ShortID),
		}
	}
	peers := func(ps []db.PeerIdentity) []api.LinkPeer {
		if len(ps) == 0 {
			return nil
		}
		out := make([]api.LinkPeer, 0, len(ps))
		for _, p := range ps {
			out = append(out, peer(p))
		}
		return out
	}
	out := &api.LinkChanges{}
	if changes.ParentSet != nil {
		p := peer(*changes.ParentSet)
		out.ParentSet = &p
	}
	if changes.ParentRemoved != nil {
		p := peer(*changes.ParentRemoved)
		out.ParentRemoved = &p
	}
	out.BlocksAdded = peers(changes.BlocksAdded)
	out.BlocksRemoved = peers(changes.BlocksRemoved)
	out.BlockedByAdded = peers(changes.BlockedByAdded)
	out.BlockedByRemoved = peers(changes.BlockedByRemoved)
	out.RelatedAdded = peers(changes.RelatedAdded)
	out.RelatedRemoved = peers(changes.RelatedRemoved)
	return out
}
