package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func revalidateIssueScopedPrincipal(ctx context.Context, store db.Storage) error {
	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.Scope == nil {
		return nil
	}
	if principal.ExpiresAt == nil || !time.Now().UTC().Before(*principal.ExpiresAt) || principal.TokenID == 0 {
		return api.NewError(401, "unauthorized", "authentication required", "", nil)
	}
	current, err := store.APITokenByID(ctx, principal.TokenID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return internalAPIError(err)
	}
	admitted := db.APIToken{ID: principal.TokenID, Actor: principal.Actor, Scope: principal.Scope, ExpiresAt: principal.ExpiresAt}
	if err != nil || !db.ActiveAPITokenGrantMatches(current, admitted, time.Now()) {
		return api.NewError(401, "unauthorized", "authentication required", "", nil)
	}
	return revalidateIssueScopedDomain(ctx, store, principal.Scope)
}

func revalidateIssueScopedDomain(
	ctx context.Context, store db.Storage, scope *db.APITokenScope,
) error {
	unauthorized := func() error {
		return api.NewError(http.StatusUnauthorized, "unauthorized", "authentication required", "", nil)
	}
	project, err := store.ProjectByUID(ctx, scope.ProjectUID)
	if errors.Is(err, db.ErrNotFound) || (err == nil && project.DeletedAt != nil) {
		return unauthorized()
	}
	if err != nil {
		return internalAPIError(err)
	}
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	if err == nil && binding.Role == db.FederationRoleSpoke {
		return unauthorized()
	}
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return internalAPIError(err)
	}
	root, err := store.IssueByUID(ctx, scope.RootIssueUID, db.IncludeDeletedNo)
	if errors.Is(err, db.ErrNotFound) || (err == nil && root.ProjectID != project.ID) {
		return unauthorized()
	}
	if err != nil {
		return internalAPIError(err)
	}
	return nil
}

func withScopedPrincipalRevalidation(store db.Storage, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if issueScopeFromContext(r.Context()) == nil {
			next.ServeHTTP(w, r)
			return
		}
		if err := revalidateIssueScopedPrincipal(r.Context(), store); err != nil {
			if apiErr, ok := errors.AsType[*api.APIError](err); ok {
				api.WriteEnvelope(w, apiErr.Status, apiErr.Code, apiErr.Message)
			} else {
				api.WriteEnvelope(w, http.StatusInternalServerError, "internal", "internal error")
			}
			return
		}
		principal, ok := PrincipalFromContext(r.Context())
		if !ok || principal.Scope == nil || principal.ExpiresAt == nil {
			api.WriteEnvelope(w, http.StatusServiceUnavailable, "authorization_unavailable",
				"scoped authorization is unavailable")
			return
		}
		admitted := db.APIToken{
			ID: principal.TokenID, Actor: principal.Actor,
			Scope: principal.Scope, ExpiresAt: principal.ExpiresAt,
		}
		nativeFence := store.IssueScopedTokenTransactionFence(admitted)
		fenced := db.WithAdditionalTransactionFence(db.WithIssueScopeTargets(r.Context()), func(
			fenceCtx context.Context, transaction db.Transaction,
		) error {
			if err := nativeFence(fenceCtx, transaction); err != nil {
				if errors.Is(err, db.ErrNotFound) {
					return api.NewError(http.StatusUnauthorized, "unauthorized",
						"authentication required", "", nil)
				}
				return err
			}
			return nil
		})
		r = r.WithContext(fenced)
		if r.URL.Path == pathEventsStreamPath {
			next.ServeHTTP(w, r)
			return
		}
		response := newBufferedScopedResponse(w)
		next.ServeHTTP(response, r)
		if err := validateScopedResponse(r.Context(), store, *principal.Scope); err != nil {
			if apiErr, ok := errors.AsType[*api.APIError](err); ok {
				api.WriteEnvelope(w, apiErr.Status, apiErr.Code, apiErr.Message)
			} else {
				api.WriteEnvelope(w, http.StatusInternalServerError, "internal", "internal error")
			}
			return
		}
		response.writeTo(w)
	})
}

var issueScopedAllActions = []string{
	"issue.read", "issue.create_child", "issue.edit", "issue.comment", "issue.labels",
	"issue.metadata", "issue.assign", "issue.claim", "issue.close", "issue.reopen",
	"issue.link", "issue.lease", "activity.read",
}

func issueScopedAllowedActions(writable bool) []string {
	if writable {
		return append([]string(nil), issueScopedAllActions...)
	}
	return []string{"issue.read", "activity.read"}
}

func issueScopeFromContext(ctx context.Context) *db.APITokenScope {
	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.Kind != PrincipalDBToken || principal.Scope == nil {
		return nil
	}
	return principal.Scope
}

func authorizeIssueScopedProject(ctx context.Context, project db.Project) error {
	scope := issueScopeFromContext(ctx)
	if scope == nil {
		return nil
	}
	if project.DeletedAt != nil || project.UID != scope.ProjectUID {
		return api.NewError(404, "project_not_found", "project not found", "", nil)
	}
	return nil
}

// authorizeIssueScopedIssue checks active parent containment and records the
// target for revalidation by the native write transaction.
func authorizeIssueScopedIssue(ctx context.Context, store db.Storage, issue db.Issue) error {
	scope := issueScopeFromContext(ctx)
	if scope == nil {
		return nil
	}
	if err := revalidateIssueScopedPrincipal(ctx, store); err != nil {
		return err
	}
	allowed, err := store.IssueInScope(ctx, *scope, issue.ID)
	if err != nil {
		return internalAPIError(err)
	}
	if !allowed {
		return api.NewError(404, "issue_not_found", "issue not found", "", nil)
	}
	db.RecordIssueScopeTarget(ctx, issue.ID)
	return nil
}

// issueScopedMembership returns active collection candidates from one recursive
// query. nil IDs means unrestricted; an empty non-nil slice denies all rows.
func issueScopedMembership(ctx context.Context, store db.Storage) (db.Project, []int64, error) {
	scope := issueScopeFromContext(ctx)
	if scope == nil {
		return db.Project{}, nil, nil
	}
	project, err := store.ProjectByUID(ctx, scope.ProjectUID)
	if errors.Is(err, db.ErrNotFound) {
		return db.Project{}, []int64{}, nil
	}
	if err != nil {
		return db.Project{}, nil, internalAPIError(err)
	}
	members, err := store.IssueScopedMembers(ctx, *scope)
	if err != nil {
		return db.Project{}, nil, internalAPIError(err)
	}
	ids := make([]int64, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.ID)
	}
	return project, ids, nil
}

func issueScopedAllowedIDSet(ctx context.Context, store db.Storage) (map[int64]struct{}, bool, error) {
	if issueScopeFromContext(ctx) == nil {
		return nil, false, nil
	}
	_, ids, err := issueScopedMembership(ctx, store)
	if err != nil {
		return nil, true, err
	}
	allowed := make(map[int64]struct{}, len(ids))
	for _, issueID := range ids {
		allowed[issueID] = struct{}{}
	}
	return allowed, true, nil
}

func filterIssueScopedIssues(issues []db.Issue, allowed map[int64]struct{}, scoped bool) []db.Issue {
	if !scoped {
		return issues
	}
	out := make([]db.Issue, 0, len(issues))
	for _, issue := range issues {
		if _, ok := allowed[issue.ID]; ok {
			out = append(out, issue)
		}
	}
	return out
}

func filterIssueScopedReportEvents(
	ctx context.Context, store db.Storage, events []db.Event,
) ([]db.Event, map[string]struct{}, error) {
	scope := issueScopeFromContext(ctx)
	if scope == nil {
		return events, nil, nil
	}
	if err := revalidateIssueScopedPrincipal(ctx, store); err != nil {
		return nil, nil, err
	}
	allowed, allowedUIDs, err := issueScopedEventMembership(ctx, store, *scope)
	if err != nil {
		return nil, nil, err
	}
	out := make([]db.Event, 0, len(events))
	projectUID := scope.ProjectUID
	for _, event := range events {
		projected, ok := projectIssueScopedEvent(event, allowed, projectUID)
		if !ok {
			continue
		}
		// Reports keep the typed safe projection and re-add only the
		// authorized link/parent context the digest and audit aggregators
		// read, so counts stay truthful without naming hidden peers.
		projected.Payload = scopedReportPayload(event, projected.Payload, allowedUIDs)
		out = append(out, projected)
	}
	return out, allowedUIDs, nil
}

// scopedReportPayload starts from the typed safe projection already applied
// by projectIssueScopedEvent and re-adds the report-only context the
// aggregators mine out of payloads, filtered to authorized (in-scope) peers:
// issue.created initial links, authorized issue.links_changed edges, and the
// in-scope parent snapshot on issue.closed rows.
func scopedReportPayload(event db.Event, safePayload string, allowedUIDs map[string]struct{}) string {
	switch event.Type {
	case "issue.created":
		return scopedCreatedReportPayload(event.Payload, safePayload, allowedUIDs)
	case "issue.links_changed":
		return scopedLinksChangedReportPayload(event.Payload, allowedUIDs)
	case "issue.closed":
		return scopedClosedReportPayload(event.Payload, safePayload, allowedUIDs)
	default:
		return safePayload
	}
}

// createdLinkPayload mirrors the digest-relevant subset of the stored
// issue.created links array (see createdLinkOut in the storage layer).
type createdLinkPayload struct {
	Type      string `json:"type"`
	ToShortID string `json:"to_short_id,omitempty"`
	Incoming  bool   `json:"incoming,omitempty"`
}

func scopedCreatedReportPayload(rawPayload, safePayload string, allowedUIDs map[string]struct{}) string {
	if rawPayload == "" || safePayload == "" {
		return safePayload
	}
	var raw struct {
		Links []struct {
			Type       string `json:"type"`
			ToShortID  string `json:"to_short_id"`
			ToIssueUID string `json:"to_issue_uid"`
			Incoming   bool   `json:"incoming"`
		} `json:"links"`
	}
	if json.Unmarshal([]byte(rawPayload), &raw) != nil {
		return safePayload
	}
	links := make([]createdLinkPayload, 0, len(raw.Links))
	for _, link := range raw.Links {
		if _, ok := allowedUIDs[link.ToIssueUID]; !ok {
			continue
		}
		links = append(links, createdLinkPayload{
			Type: link.Type, ToShortID: link.ToShortID, Incoming: link.Incoming,
		})
	}
	if len(links) == 0 {
		return safePayload
	}
	var projected map[string]jsontext.Value
	if json.Unmarshal([]byte(safePayload), &projected) != nil {
		return safePayload
	}
	if projected == nil {
		// JSON null unmarshals into a nil map; there is no safe object to
		// enrich, so fail closed to the unmodified safe payload.
		return safePayload
	}
	encoded, err := json.Marshal(links)
	if err != nil {
		return safePayload
	}
	projected["links"] = encoded
	merged, err := json.Marshal(projected)
	if err != nil {
		return safePayload
	}
	return string(merged)
}

// linksChangedReportPayload is the digest's view of an issue.links_changed
// event: short_id-form edges only, restricted to in-scope peers. The UID-form
// parallel arrays are dropped; per-edge authorization uses them purely as
// matching keys against the granted subtree.
type linksChangedReportPayload struct {
	ParentSet        *string  `json:"parent_set,omitempty"`
	ParentRemoved    *string  `json:"parent_removed,omitempty"`
	BlocksAdded      []string `json:"blocks_added,omitempty"`
	BlocksRemoved    []string `json:"blocks_removed,omitempty"`
	BlockedByAdded   []string `json:"blocked_by_added,omitempty"`
	BlockedByRemoved []string `json:"blocked_by_removed,omitempty"`
	RelatedAdded     []string `json:"related_added,omitempty"`
	RelatedRemoved   []string `json:"related_removed,omitempty"`
	UpdatedAt        string   `json:"updated_at,omitempty"`
}

func scopedLinksChangedReportPayload(rawPayload string, allowedUIDs map[string]struct{}) string {
	if rawPayload == "" {
		return ""
	}
	var raw struct {
		ParentSet            *string  `json:"parent_set"`
		ParentSetUID         *string  `json:"parent_set_uid"`
		ParentRemoved        *string  `json:"parent_removed"`
		ParentRemovedUID     *string  `json:"parent_removed_uid"`
		BlocksAdded          []string `json:"blocks_added"`
		BlocksAddedUIDs      []string `json:"blocks_added_uids"`
		BlocksRemoved        []string `json:"blocks_removed"`
		BlocksRemovedUIDs    []string `json:"blocks_removed_uids"`
		BlockedByAdded       []string `json:"blocked_by_added"`
		BlockedByAddedUIDs   []string `json:"blocked_by_added_uids"`
		BlockedByRemoved     []string `json:"blocked_by_removed"`
		BlockedByRemovedUIDs []string `json:"blocked_by_removed_uids"`
		RelatedAdded         []string `json:"related_added"`
		RelatedAddedUIDs     []string `json:"related_added_uids"`
		RelatedRemoved       []string `json:"related_removed"`
		RelatedRemovedUIDs   []string `json:"related_removed_uids"`
		UpdatedAt            string   `json:"updated_at"`
	}
	if json.Unmarshal([]byte(rawPayload), &raw) != nil {
		return ""
	}
	filterShortIDs := func(shortIDs, uids []string) []string {
		if len(shortIDs) != len(uids) {
			return nil
		}
		kept := make([]string, 0, len(shortIDs))
		for index, shortID := range shortIDs {
			if _, ok := allowedUIDs[uids[index]]; !ok {
				continue
			}
			kept = append(kept, shortID)
		}
		return kept
	}
	out := linksChangedReportPayload{UpdatedAt: raw.UpdatedAt}
	keptEdges := 0
	collectEdge := func(dst *[]string, shortIDs, uids []string) {
		filtered := filterShortIDs(shortIDs, uids)
		keptEdges += len(filtered)
		if len(filtered) > 0 {
			*dst = filtered
		}
	}
	if raw.ParentSet != nil && raw.ParentSetUID != nil {
		if _, ok := allowedUIDs[*raw.ParentSetUID]; ok {
			out.ParentSet = raw.ParentSet
			keptEdges++
		}
	}
	if raw.ParentRemoved != nil && raw.ParentRemovedUID != nil {
		if _, ok := allowedUIDs[*raw.ParentRemovedUID]; ok {
			out.ParentRemoved = raw.ParentRemoved
			keptEdges++
		}
	}
	collectEdge(&out.BlocksAdded, raw.BlocksAdded, raw.BlocksAddedUIDs)
	collectEdge(&out.BlocksRemoved, raw.BlocksRemoved, raw.BlocksRemovedUIDs)
	collectEdge(&out.BlockedByAdded, raw.BlockedByAdded, raw.BlockedByAddedUIDs)
	collectEdge(&out.BlockedByRemoved, raw.BlockedByRemoved, raw.BlockedByRemovedUIDs)
	collectEdge(&out.RelatedAdded, raw.RelatedAdded, raw.RelatedAddedUIDs)
	collectEdge(&out.RelatedRemoved, raw.RelatedRemoved, raw.RelatedRemovedUIDs)
	if keptEdges == 0 {
		return ""
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func scopedClosedReportPayload(rawPayload, safePayload string, allowedUIDs map[string]struct{}) string {
	if rawPayload == "" || safePayload == "" {
		return safePayload
	}
	var raw struct {
		ParentUID     *string `json:"parent_uid"`
		ParentShortID *string `json:"parent_short_id"`
	}
	if json.Unmarshal([]byte(rawPayload), &raw) != nil {
		return safePayload
	}
	if raw.ParentUID == nil || raw.ParentShortID == nil {
		return safePayload
	}
	if _, ok := allowedUIDs[*raw.ParentUID]; !ok {
		return safePayload
	}
	var projected map[string]jsontext.Value
	if json.Unmarshal([]byte(safePayload), &projected) != nil {
		return safePayload
	}
	if projected == nil {
		// JSON null unmarshals into a nil map; there is no safe object to
		// enrich, so fail closed to the unmodified safe payload.
		return safePayload
	}
	parentUID, err := json.Marshal(*raw.ParentUID)
	if err != nil {
		return safePayload
	}
	parentShortID, err := json.Marshal(*raw.ParentShortID)
	if err != nil {
		return safePayload
	}
	projected["parent_uid"] = parentUID
	projected["parent_short_id"] = parentShortID
	merged, err := json.Marshal(projected)
	if err != nil {
		return safePayload
	}
	return string(merged)
}
