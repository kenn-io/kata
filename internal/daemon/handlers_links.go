package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// registerLinksHandlers installs POST/DELETE /links. CreateLinkAndEvent and
// DeleteLinkAndEvent wrap the link mutation, the matching issue.linked /
// issue.unlinked event, and the issues.updated_at touch in one TX so there's
// no window where the row mutation lands without its event.
//
// For type=parent --replace, the handler runs a pre-flight cycle check (walk
// the prospective parent's ancestor chain) BEFORE deleting the old parent, so
// a cycle-rejected replace leaves the existing parent untouched and emits no
// issue.unlinked event. On the happy path it emits an issue.unlinked event for
// the old parent (in its own TX) before inserting the new parent link with its
// issue.linked event. The response shape carries only the linked event; the
// unlinked event still lands in the events table for SSE/poll clients. The
// in-tx cycle guard in CreateLinkAndEvent stays as the race backstop.
func registerLinksHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "createLink",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/links",
	}, createLinkHandler(cfg))

	huma.Register(humaAPI, huma.Operation{
		OperationID: "deleteLink",
		Method:      "DELETE",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/links/{link_id}",
	}, deleteLinkHandler(cfg))
}

// requireFederatedLinkClaims gates a set of link endpoints, evaluating each
// issue against its OWN project's federation binding. Links span projects
// (storage v16), so a peer in a federated project must be checked against
// that project's claim state, not the URL issue's project. Soft-deleted
// peers are skipped (their link rows are still removable, but the issue can't
// hold a live claim). Duplicates are evaluated once.
func requireFederatedLinkClaims(ctx context.Context, cfg ServerConfig, actor string, issues ...db.Issue) error {
	seen := make(map[int64]struct{}, len(issues))
	for _, issue := range issues {
		if _, ok := seen[issue.ID]; ok {
			continue
		}
		if issue.DeletedAt != nil {
			continue
		}
		seen[issue.ID] = struct{}{}
		if err := requireFederatedIssueClaim(ctx, cfg, issue.ProjectID, issue, actor); err != nil {
			return err
		}
	}
	return nil
}

func createLinkHandler(cfg ServerConfig) func(context.Context, *api.CreateLinkRequest) (*api.CreateLinkResponse, error) {
	return func(ctx context.Context, in *api.CreateLinkRequest) (*api.CreateLinkResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		from, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		to, err := resolveLinkTargetRef(ctx, cfg.DB, in.ProjectID, in.Body.ToRef, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		if err := requireLinkTargetAddable(ctx, cfg.DB, in.ProjectID, to); err != nil {
			return nil, err
		}
		if err := requireFederatedIssueClaim(ctx, cfg, from.ProjectID, from, actor); err != nil {
			return nil, err
		}

		// Reject self-link before mutating state. The DB will catch this anyway,
		// but in the --replace path we delete the existing parent before we'd
		// see that error from CreateLinkAndEvent — leaving us with an
		// unlinked-but-unreplaced parent and a fired issue.unlinked event.
		// Links may span projects (storage v16): `to` is resolved globally via
		// resolveLinkTargetRef and gated against its own project's archive
		// state by requireLinkTargetAddable above.
		if from.ID == to.ID {
			return nil, api.NewError(400, "validation", "cannot link an issue to itself", "", nil)
		}

		// Storage endpoints: canonical (from < to) for related; otherwise as-is.
		// canonicalFrom/canonicalTo match the Link row's actual columns
		// and feed the LinkOut wire projection (so the response shows the
		// canonical link, e.g. (3, 5) regardless of which side the user posted
		// from). Event attribution always uses the URL issue (from).
		storageFromID, storageToID := from.ID, to.ID
		names := &projectNames{store: cfg.DB}
		canonicalFromPeer, err := linkPeerFor(ctx, names, from)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		canonicalToPeer, err := linkPeerFor(ctx, names, to)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		if in.Body.Type == "related" && storageFromID > storageToID {
			storageFromID, storageToID = storageToID, storageFromID
			canonicalFromPeer, canonicalToPeer = canonicalToPeer, canonicalFromPeer
		}
		if existing, lookupErr := cfg.DB.LinkByEndpoints(ctx, storageFromID, storageToID, in.Body.Type); lookupErr == nil {
			return mutationLinkResponse(from, existing, canonicalFromPeer, canonicalToPeer, nil, false), nil
		} else if !errors.Is(lookupErr, db.ErrNotFound) {
			return nil, api.NewError(500, "internal", lookupErr.Error(), "", nil)
		}
		if err := requireFederatedIssueClaim(ctx, cfg, to.ProjectID, to, actor); err != nil {
			return nil, err
		}

		// Parent --replace path: delete the existing parent link in its own TX
		// (emitting issue.unlinked) before inserting the new parent link. Parent
		// links are never canonicalized, so storageFromID == from.ID here.
		if in.Body.Type == "parent" && in.Body.Replace {
			// Pre-flight cycle check before any mutation: walk the prospective
			// parent's ancestor chain and reject if `from` is already an
			// ancestor. Without it the delete-then-insert sequence would unlink
			// the old parent (and emit issue.unlinked) before the in-tx guard
			// fires, leaving `from` parentless. The in-tx guard stays as the
			// race backstop; this closes the practical gap.
			cycle, cerr := parentReplaceWouldCycle(ctx, cfg.DB, from.ID, to.ID)
			if cerr != nil {
				return nil, api.NewError(500, "internal", cerr.Error(), "", nil)
			}
			if cycle {
				// Byte-identical to the insert path's db.ErrParentCycle mapping.
				return nil, api.NewError(400, "validation",
					fmt.Sprintf("set_parent on #%s would create a parent cycle", from.ShortID),
					"the requested parent is a descendant of this issue", nil)
			}
			if existing, perr := cfg.DB.ParentOf(ctx, from.ID); perr == nil {
				if existing.ToIssueID == to.ID {
					// Replacing with the same parent is a no-op.
					return mutationLinkResponse(from, existing, canonicalFromPeer, canonicalToPeer, nil, false), nil
				}
				// Resolve the OLD parent so the issue.unlinked event
				// payload records the parent we're actually removing.
				oldParentIssue, err := cfg.DB.IssueByID(ctx, existing.ToIssueID)
				if err != nil {
					return nil, api.NewError(500, "internal", err.Error(), "", nil)
				}
				if err := requireFederatedLinkClaims(ctx, cfg, actor, oldParentIssue); err != nil {
					return nil, err
				}
				unlinkEv := db.LinkEventParams{
					EventType:    "issue.unlinked",
					EventIssueID: from.ID,
					FromShortID:  from.ShortID,
					FromUID:      from.UID,
					ToShortID:    oldParentIssue.ShortID,
					ToUID:        oldParentIssue.UID,
					Actor:        actor,
				}
				unlinkEvt, err := cfg.DB.DeleteLinkAndEvent(ctx, existing, unlinkEv)
				if err != nil {
					if apiErr := federationReadOnlyError(err); apiErr != nil {
						return nil, apiErr
					}
					return nil, api.NewError(500, "internal", err.Error(), "", nil)
				}
				cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: &unlinkEvt, ProjectID: in.ProjectID})
				cfg.Hooks.Enqueue(unlinkEvt)
			} else if !errors.Is(perr, db.ErrNotFound) {
				return nil, api.NewError(500, "internal", perr.Error(), "", nil)
			}
		}

		// Default path: insert link + emit issue.linked + touch updated_at, all
		// in one TX. Distinct error types map to specific responses.
		linkEv := db.LinkEventParams{
			EventType:    "issue.linked",
			EventIssueID: from.ID,
			FromShortID:  from.ShortID,
			FromUID:      from.UID,
			ToShortID:    to.ShortID,
			ToUID:        to.UID,
			Actor:        actor,
		}
		link, evt, err := cfg.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{
			FromIssueID: storageFromID,
			ToIssueID:   storageToID,
			Type:        in.Body.Type,
			Author:      actor,
		}, linkEv)
		switch {
		case errors.Is(err, db.ErrLinkExists):
			// Duplicate (from, to, type) → no-op. Re-fetch and return existing.
			existing, lookupErr := cfg.DB.LinkByEndpoints(ctx, storageFromID, storageToID, in.Body.Type)
			if lookupErr != nil {
				return nil, api.NewError(500, "internal", lookupErr.Error(), "", nil)
			}
			return mutationLinkResponse(from, existing, canonicalFromPeer, canonicalToPeer, nil, false), nil
		case errors.Is(err, db.ErrParentAlreadySet):
			return nil, api.NewError(409, "parent_already_set",
				"this issue already has a parent", "pass replace=true to swap", nil)
		case errors.Is(err, db.ErrSelfLink):
			return nil, api.NewError(400, "validation", "cannot link an issue to itself", "", nil)
		case errors.Is(err, db.ErrParentCycle):
			// Byte-identical to the edit path's mapping (handlers_issues.go).
			// from is the child whose parent is being set, matching the edit
			// path's issueShortID.
			return nil, api.NewError(400, "validation",
				fmt.Sprintf("set_parent on #%s would create a parent cycle", from.ShortID),
				"the requested parent is a descendant of this issue", nil)
		case errors.Is(err, db.ErrFederatedReadOnly):
			return nil, federationReadOnlyError(err)
		case err != nil:
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}

		updatedIssue, err := cfg.DB.IssueByID(ctx, from.ID)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: &evt, ProjectID: in.ProjectID})
		cfg.Hooks.Enqueue(evt)
		return mutationLinkResponse(updatedIssue, link, canonicalFromPeer, canonicalToPeer, &evt, true), nil
	}
}

// parentReplaceWouldCycle reports whether making prospectiveParentID the parent
// of childID would close a parent cycle, by walking the prospective parent's
// ancestor chain upward via ParentOf. The cycle exists when childID is already
// an ancestor of (or equal to) the prospective parent. A db.ErrNotFound from
// ParentOf is the chain end (the issue has no parent), not an error. Used by
// the parent --replace pre-flight so a rejected replace never deletes the old
// parent first.
//
// The walk shares db.MaxParentDepth with the storage layer's in-transaction
// cycle guard: any chain that guard would refuse must already fail here,
// before the old parent is unlinked.
func parentReplaceWouldCycle(ctx context.Context, store db.Storage, childID, prospectiveParentID int64) (bool, error) {
	cursor := prospectiveParentID
	for hops := 0; hops < db.MaxParentDepth; hops++ {
		if cursor == childID {
			return true, nil
		}
		link, err := store.ParentOf(ctx, cursor)
		if errors.Is(err, db.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		cursor = link.ToIssueID
	}
	return false, fmt.Errorf("parent chain exceeded %d hops walking from issue %d", db.MaxParentDepth, prospectiveParentID)
}

func deleteLinkHandler(cfg ServerConfig) func(context.Context, *api.DeleteLinkRequest) (*api.MutationResponse, error) {
	return func(ctx context.Context, in *api.DeleteLinkRequest) (*api.MutationResponse, error) {
		actor, err := attributedActor(ctx, in.Actor)
		if err != nil {
			return nil, err
		}
		from, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}

		link, err := cfg.DB.LinkByID(ctx, in.LinkID)
		if errors.Is(err, db.ErrNotFound) {
			// Idempotent: no row → no-op envelope.
			out := &api.MutationResponse{}
			out.Body.Issue = from
			out.Body.Event = nil
			out.Body.Changed = false
			return out, nil
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		// The URL says we're operating on issue {ref}'s links. Reject if
		// the link's two endpoints don't include this issue — defends against
		// URL manipulation that would otherwise emit an event attributed to
		// the wrong issue. The resolved `from` already belongs to in.ProjectID,
		// so requiring it as an endpoint also scopes the delete to this project;
		// links themselves are project-independent edges (storage v16) and may
		// legitimately reach a peer in another project.
		if link.FromIssueID != from.ID && link.ToIssueID != from.ID {
			return nil, api.NewError(404, "link_not_found", "link not attached to this issue", "", nil)
		}

		// Resolve the link's storage endpoints so the payload carries each
		// peer's short_id + UID. For parent/blocks links the URL issue is
		// always the link's stored from side; for canonicalized related
		// links the URL issue may be either endpoint. The unlink payload
		// is always oriented from the URL issue's POV — from_* carries
		// the URL issue, to_* carries the peer — so consumers can render
		// "the URL issue unlinked from <peer>" regardless of which side
		// the stored row holds.
		linkFrom, err := cfg.DB.IssueByID(ctx, link.FromIssueID)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		linkTo, err := cfg.DB.IssueByID(ctx, link.ToIssueID)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		if link.FromIssueID != from.ID {
			linkFrom, linkTo = linkTo, linkFrom
		}
		if err := requireFederatedLinkClaims(ctx, cfg, actor, linkFrom, linkTo); err != nil {
			return nil, err
		}
		ev := db.LinkEventParams{
			EventType:    "issue.unlinked",
			EventIssueID: from.ID,
			FromShortID:  linkFrom.ShortID,
			FromUID:      linkFrom.UID,
			ToShortID:    linkTo.ShortID,
			ToUID:        linkTo.UID,
			Actor:        actor,
		}
		evt, err := cfg.DB.DeleteLinkAndEvent(ctx, link, ev)
		if errors.Is(err, db.ErrNotFound) {
			// Lost the race against another DELETE; surface as no-op.
			out := &api.MutationResponse{}
			out.Body.Issue = from
			out.Body.Event = nil
			out.Body.Changed = false
			return out, nil
		}
		if errors.Is(err, db.ErrFederatedReadOnly) {
			return nil, federationReadOnlyError(err)
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		updatedIssue, err := cfg.DB.IssueByID(ctx, from.ID)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: &evt, ProjectID: in.ProjectID})
		cfg.Hooks.Enqueue(evt)
		out := &api.MutationResponse{}
		out.Body.Issue = updatedIssue
		out.Body.Event = &evt
		out.Body.Changed = true
		return out, nil
	}
}

// mutationLinkResponse assembles a CreateLinkResponse from the source issue,
// the link row, the canonical endpoint peers, an optional event, and the
// changed flag. Used for both fresh inserts (event != nil, changed=true) and
// no-op envelopes (event == nil, changed=false).
func mutationLinkResponse(issue db.Issue, link db.Link, from, to api.LinkPeer, evt *db.Event, changed bool) *api.CreateLinkResponse {
	out := &api.CreateLinkResponse{}
	out.Body.Issue = issue
	out.Body.Link = api.LinkOut{
		ID:        link.ID,
		From:      from,
		To:        to,
		Type:      link.Type,
		Author:    link.Author,
		CreatedAt: link.CreatedAt,
	}
	out.Body.Event = evt
	out.Body.Changed = changed
	return out
}
