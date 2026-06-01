package daemon

import (
	"context"
	"errors"

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

// resolveInitialLinks turns CreateInitialLinkBody entries (string ToRef) into
// db.InitialLink entries (int64 ToNumber, which the db layer treats as an
// issue row id). Soft-deleted targets are excluded — initial-link creation
// must reject hidden peers per spec §6.
func resolveInitialLinks(ctx context.Context, store db.Storage, projectID int64, links []api.CreateInitialLinkBody) ([]db.InitialLink, error) {
	out := make([]db.InitialLink, 0, len(links))
	for _, l := range links {
		target, err := resolveIssueRef(ctx, store, projectID, l.ToRef, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		out = append(out, db.InitialLink{
			Type:     l.Type,
			ToNumber: target.ID,
			Incoming: l.Incoming,
		})
	}
	return out, nil
}

// fillLinksDeltaParams resolves each api.LinksDelta string ref into an
// issue id and stuffs the int64-keyed slices into params. Each ref is
// resolved through resolveIssueRef, which already maps to issue_not_found
// 404 for misses, so error returns are wire-ready.
func fillLinksDeltaParams(ctx context.Context, store db.Storage, projectID int64, d *api.LinksDelta, params *db.EditIssueAtomicParams) error {
	if d == nil {
		return nil
	}
	resolve := func(ref string, include db.IncludeDeleted) (int64, error) {
		issue, err := resolveIssueRef(ctx, store, projectID, ref, include)
		if err != nil {
			return 0, err
		}
		return issue.ID, nil
	}
	resolveSlice := func(refs []string, include db.IncludeDeleted) ([]int64, error) {
		if len(refs) == 0 {
			return nil, nil
		}
		ids := make([]int64, 0, len(refs))
		for _, r := range refs {
			id, err := resolve(r, include)
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, nil
	}
	// resolveSliceTolerant is the idempotent-remove variant: misses
	// (issue_not_found) are silently dropped instead of surfacing 404.
	// The desired end state — "no link from this issue to N" — already
	// holds when there is no N at all, so the remove is a no-op.
	resolveSliceTolerant := func(refs []string, include db.IncludeDeleted) ([]int64, error) {
		if len(refs) == 0 {
			return nil, nil
		}
		ids := make([]int64, 0, len(refs))
		for _, r := range refs {
			id, err := resolve(r, include)
			if err != nil {
				var ae *api.APIError
				if errors.As(err, &ae) && ae.Status == 404 {
					continue
				}
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, nil
	}
	if d.SetParent != nil {
		id, err := resolve(*d.SetParent, db.IncludeDeletedNo)
		if err != nil {
			return err
		}
		params.SetParent = &id
	}
	if d.RemoveParent != nil {
		// Remove paths must tolerate a soft-deleted peer (the link row is
		// still live; the user can still ask to clean it up).
		id, err := resolve(*d.RemoveParent, db.IncludeDeletedYes)
		if err != nil {
			return err
		}
		params.RemoveParent = &id
	}
	var err error
	if params.AddBlocks, err = resolveSlice(d.AddBlocks, db.IncludeDeletedNo); err != nil {
		return err
	}
	if params.AddBlockedBy, err = resolveSlice(d.AddBlockedBy, db.IncludeDeletedNo); err != nil {
		return err
	}
	if params.AddRelated, err = resolveSlice(d.AddRelated, db.IncludeDeletedNo); err != nil {
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
// api.LinkChanges. The db layer reports parallel slices of (short_id, uid);
// peers project 1:1 onto LinkPeer.
func buildLinkChanges(_ context.Context, _ db.Storage, changes db.AtomicEditChanges) (*api.LinkChanges, error) {
	peer := func(short string, uid string) api.LinkPeer {
		return api.LinkPeer{UID: uid, ShortID: short}
	}
	peers := func(shorts, uids []string) []api.LinkPeer {
		if len(shorts) == 0 {
			return nil
		}
		out := make([]api.LinkPeer, 0, len(shorts))
		for i, s := range shorts {
			var u string
			if i < len(uids) {
				u = uids[i]
			}
			out = append(out, peer(s, u))
		}
		return out
	}
	out := &api.LinkChanges{}
	if changes.ParentSet != nil && changes.ParentSetUID != nil {
		p := peer(*changes.ParentSet, *changes.ParentSetUID)
		out.ParentSet = &p
	}
	if changes.ParentRemoved != nil && changes.ParentRemovedUID != nil {
		p := peer(*changes.ParentRemoved, *changes.ParentRemovedUID)
		out.ParentRemoved = &p
	}
	out.BlocksAdded = peers(changes.BlocksAdded, changes.BlocksAddedUIDs)
	out.BlocksRemoved = peers(changes.BlocksRemoved, changes.BlocksRemovedUIDs)
	out.BlockedByAdded = peers(changes.BlockedByAdded, changes.BlockedByAddedUIDs)
	out.BlockedByRemoved = peers(changes.BlockedByRemoved, changes.BlockedByRemovedUIDs)
	out.RelatedAdded = peers(changes.RelatedAdded, changes.RelatedAddedUIDs)
	out.RelatedRemoved = peers(changes.RelatedRemoved, changes.RelatedRemovedUIDs)
	return out, nil
}
