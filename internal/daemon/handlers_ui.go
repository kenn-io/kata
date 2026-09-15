package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

const (
	uiSnapshotLimitDefault   = 0
	uiSnapshotLimitMax       = 1000
	uiReferencesLimitDefault = 100
	uiReferencesLimitMax     = 200
)

type normalizedUISnapshotIntent struct {
	View              string   `json:"view"`
	ProjectUID        string   `json:"project_uid,omitempty"`
	Statuses          []string `json:"statuses,omitempty"`
	Owners            []string `json:"owners,omitempty"`
	Labels            []string `json:"labels,omitempty"`
	Relationships     []string `json:"relationships,omitempty"`
	Text              string   `json:"text,omitempty"`
	SelectedIssueUID  string   `json:"selected_issue_uid,omitempty"`
	IncludeGraph      bool     `json:"include_graph"`
	IncludeHistory    bool     `json:"include_history"`
	LocalDate         string   `json:"local_date,omitempty"`
	TimeZone          string   `json:"time_zone,omitempty"`
	ReadyAt           string   `json:"ready_at,omitempty"`
	DefaultTimezone   string   `json:"default_timezone,omitempty"`
	Limit             int      `json:"limit"`
	ScopeProjectUID   string   `json:"scope_project_uid,omitempty"`
	ScopeRootIssueUID string   `json:"scope_root_issue_uid,omitempty"`
}

func (in normalizedUISnapshotIntent) storeQuery() db.UISnapshotQuery {
	query := db.UISnapshotQuery{
		View: in.View, ProjectUID: in.ProjectUID,
		Statuses:      append([]string(nil), in.Statuses...),
		Owners:        append([]string(nil), in.Owners...),
		Labels:        append([]string(nil), in.Labels...),
		Relationships: append([]string(nil), in.Relationships...),
		Text:          in.Text, SelectedIssueUID: in.SelectedIssueUID,
		IncludeGraph: in.IncludeGraph, IncludeHistory: in.IncludeHistory,
		LocalDate: in.LocalDate, TimeZone: in.TimeZone, ReadyAt: in.ReadyAt,
		DefaultTimezone: in.DefaultTimezone, Limit: in.Limit,
	}
	if len(in.Statuses) == 1 {
		query.Status = in.Statuses[0]
	}
	if len(in.Owners) == 1 {
		query.Owner = in.Owners[0]
	}
	if len(in.Labels) == 1 {
		query.Label = in.Labels[0]
	}
	return query
}

type normalizedUIReferencesIntent struct {
	Query      string   `json:"query,omitempty"`
	ProjectUID string   `json:"project_uid,omitempty"`
	IssueUIDs  []string `json:"issue_uids,omitempty"`
	Limit      int      `json:"limit"`
}

type uiPolicy struct {
	Capabilities api.UICapabilities `json:"capabilities"`
	Origin       string             `json:"origin"`
	OriginStable bool               `json:"origin_stable"`
}

type uiETagBasis struct {
	ContractVersion string   `json:"contract_version"`
	Intent          any      `json:"intent"`
	Cursor          int64    `json:"cursor"`
	Policy          uiPolicy `json:"policy"`
}

func registerUIHandlers(humaAPI huma.API, cfg ServerConfig) {
	authorityCache := newUISnapshotAuthorityCache()
	enrichmentCache := newUISnapshotEnrichmentCache()
	huma.Register(humaAPI, huma.Operation{
		OperationID: "resolveUIIssueReference",
		Method:      http.MethodGet,
		Path:        "/api/v1/ui/issue-reference",
		Summary:     "Resolve an active issue to its browser route identity",
	}, func(ctx context.Context, in *api.UIIssueReferenceRequest) (*api.UIIssueReferenceResponse, error) {
		ctx, err := authorizeHostProjectScope(ctx, []int64{in.ProjectID}, nil, false)
		if err != nil {
			return nil, err
		}
		project, err := activeProjectByID(ctx, cfg.DB, in.ProjectID)
		if err != nil {
			return nil, err
		}
		issue, err := resolveIssueRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		if err := authorizeIssueScopedIssue(ctx, cfg.DB, issue); err != nil {
			return nil, err
		}
		out := &api.UIIssueReferenceResponse{}
		out.Body.Issue.UID = issue.UID
		out.Body.Issue.ProjectUID = project.UID
		return out, nil
	})

	snapshotOperation := huma.Operation{
		OperationID: "readUISnapshot",
		Method:      http.MethodGet,
		Path:        "/api/v1/ui/snapshot",
		Summary:     "Read a coherent browser snapshot",
		Description: "Returns 304 without projection reads when If-None-Match matches the current durable cursor, normalized intent, and effective capabilities.",
		Responses: map[string]*huma.Response{
			"304": {Description: "Snapshot unchanged"},
		},
	}
	huma.Register(humaAPI, snapshotOperation,
		func(ctx context.Context, in *api.UISnapshotRequest) (*api.UISnapshotResponse, error) {
			if cfg.UIStore == nil {
				return nil, api.NewError(http.StatusServiceUnavailable, "ui_unavailable", "UI read store is unavailable", "", nil)
			}
			intent, err := normalizeUISnapshotIntent(in)
			if err != nil {
				return nil, err
			}
			intent.ReadyAt = effectiveUIReadyAt(intent.Statuses, cfg.UIClock)
			intent.DefaultTimezone = cfg.DefaultTimezone
			if err := normalizeIssueScopeForUISnapshot(ctx, &intent); err != nil {
				return nil, err
			}
			policy := effectiveUIPolicy(ctx, cfg)
			var observedCursor *int64
			if in.IfNoneMatch != "" {
				cursor, err := cfg.UIStore.UIEventCursor(ctx)
				if err != nil {
					return nil, internalAPIError(err)
				}
				validator, err := makeUIETag(intent, cursor, policy)
				if err != nil {
					return nil, internalAPIError(err)
				}
				if matchesStrongETag(in.IfNoneMatch, validator) {
					return &api.UISnapshotResponse{Status: http.StatusNotModified, ETag: validator}, nil
				}
				observedCursor = &cursor
			}
			_, scopedIssueIDs, err := applyIssueScopeToUISnapshot(ctx, cfg.DB, &intent)
			if err != nil {
				return nil, err
			}

			authorityKey, err := uiSnapshotAuthorityKey(intent)
			if err != nil {
				return nil, internalAPIError(err)
			}
			cachedAuthority := db.UISnapshotData{}
			authorityCached := false
			// Scoped snapshots intentionally bypass shared response caches. The
			// query candidate set is the authority boundary; keeping its result
			// request-local prevents a future cache-key cleanup from widening or
			// narrowing one subtree with another principal's materialized data.
			if scopedIssueIDs == nil {
				cachedAuthority, authorityCached = authorityCache.get(authorityKey)
			}
			if scopedIssueIDs == nil && !authorityCached && intent.ProjectUID != "" && intent.Limit == 0 {
				globalIntent := intent
				globalIntent.ProjectUID = ""
				globalKey, keyErr := uiSnapshotAuthorityKey(globalIntent)
				if keyErr != nil {
					return nil, internalAPIError(keyErr)
				}
				if globalAuthority, ok := authorityCache.get(globalKey); ok {
					cachedAuthority = projectUISnapshotAuthority(globalAuthority, intent.ProjectUID)
					authorityCached = true
					authorityCache.put(authorityKey, cachedAuthority)
				}
			}
			enrichmentKey, err := uiSnapshotKey(intent)
			if err != nil {
				return nil, internalAPIError(err)
			}
			cachedEnrichment := db.UISnapshotData{}
			enrichmentCached := false
			if scopedIssueIDs == nil {
				cachedEnrichment, enrichmentCached = enrichmentCache.get(enrichmentKey)
			}
			cachedResponse := db.UISnapshotData{}
			responseCached := false
			if authorityCached && !uiSnapshotHasEnrichment(intent) {
				cachedResponse = cachedAuthority
				responseCached = true
			} else if authorityCached && enrichmentCached &&
				cachedAuthority.Cursor == cachedEnrichment.Cursor {
				cachedResponse = cachedEnrichment
				mergeUISnapshotAuthority(&cachedResponse, cachedAuthority)
				responseCached = true
			}
			if responseCached {
				if observedCursor == nil {
					cursor, err := cfg.UIStore.UIEventCursor(ctx)
					if err != nil {
						return nil, internalAPIError(err)
					}
					observedCursor = &cursor
				}
				if *observedCursor == cachedAuthority.Cursor {
					validator, err := makeUIETag(intent, cachedResponse.Cursor, policy)
					if err != nil {
						return nil, internalAPIError(err)
					}
					return snapshotResponse(cachedResponse, intent, policy, validator), nil
				}
			}
			query := intent.storeQuery()
			query.AllowedIssueIDs = scopedIssueIDs
			query.IssueScope = issueScopeFromContext(ctx)
			if authorityCached {
				cursor := cachedAuthority.Cursor
				query.ReuseAuthorityCursor = &cursor
			}
			data, err := cfg.UIStore.ReadUISnapshot(ctx, query)
			if err != nil {
				return nil, internalAPIError(err)
			}
			if data.AuthorityReused {
				mergeUISnapshotAuthority(&data, cachedAuthority)
			}
			if scopedIssueIDs != nil {
				filterScopedUISnapshot(&data, scopedIssueIDs, intent.ScopeProjectUID)
			}
			if scopedIssueIDs == nil && !data.AuthorityReused {
				authorityCache.put(authorityKey, data)
			}
			if scopedIssueIDs == nil && uiSnapshotHasEnrichment(intent) {
				enrichmentCache.put(enrichmentKey, data)
			}
			validator, err := makeUIETag(intent, data.Cursor, policy)
			if err != nil {
				return nil, internalAPIError(err)
			}
			return snapshotResponse(data, intent, policy, validator), nil
		})

	referencesOperation := huma.Operation{
		OperationID: "readUIReferences",
		Method:      http.MethodGet,
		Path:        "/api/v1/ui/references",
		Summary:     "Read bounded browser reference choices",
		Responses: map[string]*huma.Response{
			"304": {Description: "References unchanged"},
		},
	}
	huma.Register(humaAPI, referencesOperation,
		func(ctx context.Context, in *api.UIReferencesRequest) (*api.UIReferencesResponse, error) {
			if cfg.UIStore == nil {
				return nil, api.NewError(http.StatusServiceUnavailable, "ui_unavailable", "UI read store is unavailable", "", nil)
			}
			intent, err := normalizeUIReferencesIntent(in)
			if err != nil {
				return nil, err
			}
			query := db.UIReferencesQuery{
				Query: intent.Query, ProjectUID: intent.ProjectUID,
				IssueUIDs: append([]string(nil), intent.IssueUIDs...), Limit: intent.Limit,
			}
			_, scopedIssueIDs, err := applyIssueScopeToUIReferences(ctx, cfg.DB, &intent, &query)
			if err != nil {
				return nil, err
			}
			query.AllowedIssueIDs = scopedIssueIDs
			query.IssueScope = issueScopeFromContext(ctx)
			if len(intent.IssueUIDs) > 0 {
				capture, err := cfg.UIStore.ReadUIReferenceHydration(ctx, query)
				if err != nil {
					return nil, internalAPIError(err)
				}
				if !slices.Equal(capture.ResolvedUIDs, intent.IssueUIDs) {
					return nil, api.NewError(
						http.StatusNotFound, "not_found", "resource not found", "", nil,
					)
				}
				if scopedIssueIDs != nil {
					filterScopedUIReferences(&capture.References, intent.ProjectUID)
				}
				ctx, err = authorizeHostProjectScope(ctx, capture.ProjectIDs, nil, false)
				if err != nil {
					return nil, err
				}
				policy := effectiveUIPolicy(ctx, cfg)
				validator, err := makeUIETag(intent, capture.References.Cursor, policy)
				if err != nil {
					return nil, internalAPIError(err)
				}
				if matchesStrongETag(in.IfNoneMatch, validator) {
					return &api.UIReferencesResponse{Status: http.StatusNotModified, ETag: validator}, nil
				}
				sortUIReferences(&capture.References)
				return referencesResponse(capture.References, policy, validator), nil
			}
			ctx, err = authorizeHostProjectScope(ctx, nil, nil, true)
			if err != nil {
				return nil, err
			}
			policy := effectiveUIPolicy(ctx, cfg)
			if in.IfNoneMatch != "" {
				cursor, err := cfg.UIStore.UIEventCursor(ctx)
				if err != nil {
					return nil, internalAPIError(err)
				}
				validator, err := makeUIETag(intent, cursor, policy)
				if err != nil {
					return nil, internalAPIError(err)
				}
				if matchesStrongETag(in.IfNoneMatch, validator) {
					return &api.UIReferencesResponse{Status: http.StatusNotModified, ETag: validator}, nil
				}
			}
			data, err := cfg.UIStore.ReadUIReferences(ctx, query)
			if err != nil {
				return nil, internalAPIError(err)
			}
			if scopedIssueIDs != nil {
				filterScopedUIReferences(&data, intent.ProjectUID)
			}
			sortUIReferences(&data)
			validator, err := makeUIETag(intent, data.Cursor, policy)
			if err != nil {
				return nil, internalAPIError(err)
			}
			return referencesResponse(data, policy, validator), nil
		})
}

func applyIssueScopeToUISnapshot(
	ctx context.Context, store db.Storage, intent *normalizedUISnapshotIntent,
) (db.Project, []int64, error) {
	if err := normalizeIssueScopeForUISnapshot(ctx, intent); err != nil {
		return db.Project{}, nil, err
	}
	scope := issueScopeFromContext(ctx)
	if scope == nil {
		return db.Project{}, nil, nil
	}
	project, issueIDs, err := issueScopedMembership(ctx, store)
	if err != nil {
		return db.Project{}, nil, err
	}
	if intent.SelectedIssueUID != "" {
		issue, err := store.IssueByUID(ctx, intent.SelectedIssueUID, db.IncludeDeletedNo)
		if err != nil {
			return db.Project{}, nil, api.NewError(http.StatusNotFound, "issue_not_found", "issue not found", "", nil)
		}
		if err := authorizeIssueScopedIssue(ctx, store, issue); err != nil {
			return db.Project{}, nil, err
		}
	}
	return project, issueIDs, nil
}

// normalizeIssueScopeForUISnapshot adds the immutable grant identity to the
// request intent without reading projections. This lets conditional scoped
// requests derive their exact validator and return 304 after middleware
// revalidation plus the durable cursor read, before traversing subtree
// membership or resolving a selected issue.
func normalizeIssueScopeForUISnapshot(ctx context.Context, intent *normalizedUISnapshotIntent) error {
	scope := issueScopeFromContext(ctx)
	if scope == nil {
		return nil
	}
	if intent.ProjectUID != "" && intent.ProjectUID != scope.ProjectUID {
		return api.NewError(http.StatusNotFound, "project_not_found", "project not found", "", nil)
	}
	intent.ProjectUID = scope.ProjectUID
	intent.ScopeProjectUID = scope.ProjectUID
	intent.ScopeRootIssueUID = scope.RootIssueUID
	return nil
}

func applyIssueScopeToUIReferences(
	ctx context.Context,
	store db.Storage,
	intent *normalizedUIReferencesIntent,
	query *db.UIReferencesQuery,
) (db.Project, []int64, error) {
	scope := issueScopeFromContext(ctx)
	if scope == nil {
		return db.Project{}, nil, nil
	}
	if intent.ProjectUID != "" && intent.ProjectUID != scope.ProjectUID {
		return db.Project{}, nil, api.NewError(http.StatusNotFound, "project_not_found", "project not found", "", nil)
	}
	project, issueIDs, err := issueScopedMembership(ctx, store)
	if err != nil {
		return db.Project{}, nil, err
	}
	intent.ProjectUID = scope.ProjectUID
	query.ProjectUID = scope.ProjectUID
	for _, issueUID := range intent.IssueUIDs {
		issue, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedNo)
		if err != nil {
			return db.Project{}, nil, api.NewError(http.StatusNotFound, "not_found", "resource not found", "", nil)
		}
		if err := authorizeIssueScopedIssue(ctx, store, issue); err != nil {
			return db.Project{}, nil, api.NewError(http.StatusNotFound, "not_found", "resource not found", "", nil)
		}
	}
	return project, issueIDs, nil
}

func filterScopedUISnapshot(data *db.UISnapshotData, allowedIDs []int64, projectUID string) {
	allowed := make(map[int64]struct{}, len(allowedIDs))
	for _, issueID := range allowedIDs {
		allowed[issueID] = struct{}{}
	}
	allowedUIDs := make(map[string]struct{}, len(allowedIDs))
	filterIssues := func(issues []db.UIIssue) []db.UIIssue {
		out := make([]db.UIIssue, 0, len(issues))
		for _, issue := range issues {
			if _, ok := allowed[issue.ID]; ok {
				out = append(out, issue)
				allowedUIDs[issue.UID] = struct{}{}
			}
		}
		return out
	}
	data.Issues = filterIssues(data.Issues)
	data.GraphIssues = filterIssues(data.GraphIssues)
	if data.SelectedIssue != nil {
		allowedUIDs[data.SelectedIssue.UID] = struct{}{}
	}
	filterLinks := func(links []db.UILink) []db.UILink {
		out := make([]db.UILink, 0, len(links))
		for _, link := range links {
			_, fromOK := allowed[link.FromIssueID]
			_, toOK := allowed[link.ToIssueID]
			if fromOK && toOK {
				out = append(out, link)
			}
		}
		return out
	}
	data.CollectionLinks = filterLinks(data.CollectionLinks)
	data.SelectedLinks = filterLinks(data.SelectedLinks)
	data.GraphLinks = filterLinks(data.GraphLinks)
	edges := make([]db.UIGraphEdge, 0, len(data.GraphEdges))
	for _, edge := range data.GraphEdges {
		_, fromOK := allowedUIDs[edge.FromUID]
		_, toOK := allowedUIDs[edge.ToUID]
		if fromOK && toOK {
			edges = append(edges, edge)
		}
	}
	data.GraphEdges = edges
	data.GraphUnresolvedRefs = []db.UIGraphUnresolvedRef{}
	data.Recurrences = []db.Recurrence{}
	history := make([]db.Event, 0, len(data.History))
	for _, event := range data.History {
		if projected, ok := projectIssueScopedEvent(event, allowed, projectUID); ok {
			history = append(history, projected)
		}
	}
	data.History = history
	projects := make([]db.UIProject, 0, 1)
	for _, entry := range data.Projects {
		if entry.Project.UID == projectUID {
			entry.Project.Metadata = db.JSONBlob("")
			entry.Project.CreatedAt = time.Time{}
			entry.Project.DeletedAt = nil
			entry.Stats = db.ProjectStats{}
			projects = append(projects, entry)
			break
		}
	}
	data.Projects = projects
}

func filterScopedUIReferences(data *db.UIReferencesData, projectUID string) {
	projects := make([]db.Project, 0, 1)
	for _, project := range data.Projects {
		if project.UID == projectUID {
			project.Metadata = db.JSONBlob("")
			project.CreatedAt = time.Time{}
			project.DeletedAt = nil
			projects = append(projects, project)
			break
		}
	}
	data.Projects = projects
}

func normalizeUISnapshotIntent(in *api.UISnapshotRequest) (normalizedUISnapshotIntent, error) {
	view := strings.ToLower(strings.TrimSpace(in.View))
	if view == "" {
		view = "inbox"
	}
	projectUID, err := normalizeUIUID(in.ProjectUID, "project_uid", false)
	if err != nil {
		return normalizedUISnapshotIntent{}, err
	}
	selectedUID, err := normalizeUIUID(in.SelectedIssueUID, "selected_issue_uid", true)
	if err != nil {
		return normalizedUISnapshotIntent{}, err
	}
	limit, err := normalizeUILimit(in.Limit, uiSnapshotLimitDefault, uiSnapshotLimitMax)
	if err != nil {
		return normalizedUISnapshotIntent{}, err
	}
	localDate := strings.TrimSpace(in.LocalDate)
	timeZone := strings.TrimSpace(in.TimeZone)
	if localDate != "" {
		if parsed, err := time.Parse("2006-01-02", localDate); err != nil || parsed.Format("2006-01-02") != localDate {
			return normalizedUISnapshotIntent{}, uiValidationError("local_date", "local_date must use YYYY-MM-DD", nil)
		}
	}
	if timeZone != "" {
		if _, err := time.LoadLocation(timeZone); err != nil {
			return normalizedUISnapshotIntent{}, uiValidationError("time_zone", "time_zone must be a valid IANA timezone", nil)
		}
	}
	if dateSensitiveUIView(view) {
		if localDate == "" {
			return normalizedUISnapshotIntent{}, uiValidationError("local_date", "local_date is required for this view", nil)
		}
		if timeZone == "" {
			return normalizedUISnapshotIntent{}, uiValidationError("time_zone", "time_zone is required for this view", nil)
		}
	} else {
		localDate = ""
	}
	return normalizedUISnapshotIntent{
		View: view, ProjectUID: projectUID,
		Statuses:      normalizedUIValues(in.Status, true),
		Owners:        normalizedUIValues(in.Owner, false),
		Labels:        normalizedUIValues(in.Label, false),
		Relationships: normalizedUIValues(in.Relationship, true),
		Text:          strings.TrimSpace(in.Text), SelectedIssueUID: selectedUID,
		IncludeGraph: in.IncludeGraph, IncludeHistory: in.IncludeHistory,
		LocalDate: localDate, TimeZone: timeZone, Limit: limit,
	}, nil
}

func normalizeUIReferencesIntent(in *api.UIReferencesRequest) (normalizedUIReferencesIntent, error) {
	projectUID, err := normalizeUIUID(in.ProjectUID, "project_uid", false)
	if err != nil {
		return normalizedUIReferencesIntent{}, err
	}
	issueUIDs := make([]string, 0, len(in.IssueUID))
	seenIssueUIDs := make(map[string]struct{}, len(in.IssueUID))
	for _, raw := range in.IssueUID {
		issueUID, err := normalizeUIUID(raw, "issue_uid", false)
		if err != nil {
			return normalizedUIReferencesIntent{}, err
		}
		if issueUID == "" {
			continue
		}
		if _, seen := seenIssueUIDs[issueUID]; seen {
			continue
		}
		seenIssueUIDs[issueUID] = struct{}{}
		issueUIDs = append(issueUIDs, issueUID)
	}
	if len(issueUIDs) > uiReferencesLimitMax {
		return normalizedUIReferencesIntent{}, uiValidationError(
			"issue_uid",
			"issue_uid accepts at most 200 distinct values",
			map[string]any{"limit": uiReferencesLimitMax},
		)
	}
	limit, err := normalizeUILimit(
		in.Limit,
		max(uiReferencesLimitDefault, len(issueUIDs)),
		uiReferencesLimitMax,
	)
	if err != nil {
		return normalizedUIReferencesIntent{}, err
	}
	sort.Strings(issueUIDs)
	return normalizedUIReferencesIntent{
		Query: strings.TrimSpace(in.Query), ProjectUID: projectUID, IssueUIDs: issueUIDs, Limit: limit,
	}, nil
}

func normalizeUIUID(raw, field string, routeReference bool) (string, error) {
	value := strings.ToUpper(strings.TrimSpace(raw))
	if value == "" {
		return "", nil
	}
	if uid.Valid(value) {
		return value, nil
	}
	data := map[string]any{"field": field}
	if routeReference {
		data["search_ref"] = raw
	}
	return "", uiValidationError(field, field+" must be a full 26-character UID", data)
}

func normalizeUILimit(raw api.OptionalInt, defaultValue, maxValue int) (int, error) {
	if !raw.IsSet {
		return defaultValue, nil
	}
	if raw.Value <= 0 {
		return 0, uiValidationError("limit", "limit must be a positive integer", nil)
	}
	return min(raw.Value, maxValue), nil
}

func normalizedUIValues(values []string, lowercase bool) []string {
	seen := make(map[string]struct{}, len(values))
	normalized := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if lowercase {
			value = strings.ToLower(value)
		}
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	sort.Strings(normalized)
	return normalized
}

func dateSensitiveUIView(view string) bool {
	switch view {
	case "today", "upcoming", "deadlines":
		return true
	default:
		return false
	}
}

func effectiveUIReadyAt(statuses []string, clock func() time.Time) string {
	ready := false
	for _, status := range statuses {
		switch status {
		case "all":
			return ""
		case "ready":
			ready = true
		}
	}
	if !ready {
		return ""
	}
	if clock == nil {
		clock = time.Now
	}
	return clock().UTC().Format(time.RFC3339Nano)
}

func uiValidationError(field, message string, data map[string]any) error {
	if data == nil {
		data = map[string]any{"field": field}
	}
	return api.NewError(http.StatusBadRequest, "validation", message, "", data)
}

func effectiveUIPolicy(ctx context.Context, cfg ServerConfig) uiPolicy {
	policy := uiPolicy{Capabilities: api.UICapabilities{
		Writable: !cfg.InsecureReadonly, Updates: "sse", ActorPolicy: "request",
	}}
	if cfg.WebSessions != nil {
		principal, _ := PrincipalFromContext(ctx)
		policy.Capabilities.Writable = cfg.WebSessions.CanWrite(principal)
		policy.Capabilities.Updates = cfg.WebSessions.Updates()
		policy.Origin = cfg.WebSessions.Origin()
		policy.OriginStable = cfg.WebSessions.OriginStable()
	}
	if insecureReadonlyRequest(ctx) {
		policy.Capabilities.Writable = false
		policy.Capabilities.Updates = "poll"
	}
	if principal, ok := PrincipalFromContext(ctx); ok && principal.Actor != "" {
		policy.Capabilities.ActorPolicy = "identity"
	}
	policy.Capabilities.CloseRequiresEvidence = closeRequiresEvidence(ctx)
	policy.Capabilities.TokenAuditRead = tokenAuditReadAllowed(ctx)
	if principal, ok := PrincipalFromContext(ctx); ok {
		if principal.Scope != nil {
			policy.Capabilities.Scope = tokenScopeOut(principal.Scope)
			policy.Capabilities.ExpiresAt = principal.ExpiresAt
			policy.Capabilities.AllowedActions = issueScopedAllowedActions(policy.Capabilities.Writable)
		}
	}
	return policy
}

func makeUIETag(intent any, cursor int64, policy uiPolicy) (string, error) {
	encoded, err := json.Marshal(uiETagBasis{
		ContractVersion: api.UISnapshotContractVersion,
		Intent:          intent,
		Cursor:          cursor,
		Policy:          policy,
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return `"` + hex.EncodeToString(digest[:]) + `"`, nil
}

func matchesStrongETag(header, current string) bool {
	for value := range strings.SplitSeq(header, ",") {
		candidate := strings.TrimSpace(value)
		if candidate == "*" || (!strings.HasPrefix(candidate, "W/") && candidate == current) {
			return true
		}
	}
	return false
}

func snapshotResponse(data db.UISnapshotData, intent normalizedUISnapshotIntent,
	policy uiPolicy, validator string,
) *api.UISnapshotResponse {
	out := &api.UISnapshotResponse{Status: http.StatusOK, ETag: validator}
	out.Body.ContractVersion = api.UISnapshotContractVersion
	out.Body.Cursor = data.Cursor
	out.Body.Capabilities = policy.Capabilities
	out.Body.Origin = policy.Origin
	out.Body.OriginStable = policy.OriginStable
	out.Body.Catalog = make([]api.UIProject, 0, len(data.Projects))
	for _, entry := range data.Projects {
		project := dbProjectToOut(entry.Project)
		var stats *db.ProjectStats
		if issueScopeFromPolicy(policy) == nil {
			value := entry.Stats
			stats = &value
		} else {
			project = scopedProjectOut(entry.Project)
		}
		out.Body.Catalog = append(out.Body.Catalog, api.UIProject{Project: project, Stats: stats})
	}
	out.Body.Collection = nonNil(data.Issues)
	out.Body.CollectionLinks = nonNil(data.CollectionLinks)
	if intent.SelectedIssueUID != "" {
		state := data.SelectedState
		if state == "" {
			state = "missing"
		}
		out.Body.Selected = &api.UISelectedAuthority{
			State: state, Issue: data.SelectedIssue,
			Comments: nonNil(data.Comments), Labels: nonNil(data.SelectedLabels),
			Links: nonNil(data.SelectedLinks), Recurrences: nonNil(data.Recurrences),
			History: nonNil(data.History),
		}
	}
	if intent.IncludeGraph {
		out.Body.Graph = &api.UIGraph{
			Issues: nonNil(data.GraphIssues), Links: nonNil(data.GraphLinks),
			Edges: nonNil(data.GraphEdges), UnresolvedRefs: nonNil(data.GraphUnresolvedRefs),
		}
	}
	return out
}

func referencesResponse(data db.UIReferencesData, policy uiPolicy, validator string) *api.UIReferencesResponse {
	out := &api.UIReferencesResponse{Status: http.StatusOK, ETag: validator}
	out.Body.ContractVersion = api.UISnapshotContractVersion
	out.Body.Cursor = data.Cursor
	out.Body.Capabilities = policy.Capabilities
	out.Body.Origin = policy.Origin
	out.Body.OriginStable = policy.OriginStable
	out.Body.Projects = make([]api.ProjectOut, 0, len(data.Projects))
	for _, project := range data.Projects {
		if issueScopeFromPolicy(policy) != nil {
			out.Body.Projects = append(out.Body.Projects, scopedProjectOut(project))
		} else {
			out.Body.Projects = append(out.Body.Projects, dbProjectToOut(project))
		}
	}
	out.Body.Issues = nonNil(data.Issues)
	out.Body.Owners = nonNil(data.Owners)
	out.Body.Labels = nonNil(data.Labels)
	return out
}

func issueScopeFromPolicy(policy uiPolicy) *api.TokenScopeOut {
	return policy.Capabilities.Scope
}

func sortUIReferences(data *db.UIReferencesData) {
	sort.Slice(data.Projects, func(i, j int) bool {
		return data.Projects[i].Name < data.Projects[j].Name ||
			(data.Projects[i].Name == data.Projects[j].Name && data.Projects[i].UID < data.Projects[j].UID)
	})
	sort.Slice(data.Issues, func(i, j int) bool {
		return data.Issues[i].QualifiedID < data.Issues[j].QualifiedID ||
			(data.Issues[i].QualifiedID == data.Issues[j].QualifiedID && data.Issues[i].UID < data.Issues[j].UID)
	})
	sort.Strings(data.Owners)
	sort.Strings(data.Labels)
}

func nonNil[S ~[]E, E any](value S) S {
	if value == nil {
		return S{}
	}
	return value
}
