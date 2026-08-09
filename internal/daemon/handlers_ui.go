package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
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
	View             string   `json:"view"`
	ProjectUID       string   `json:"project_uid,omitempty"`
	Statuses         []string `json:"statuses,omitempty"`
	Owners           []string `json:"owners,omitempty"`
	Labels           []string `json:"labels,omitempty"`
	Relationships    []string `json:"relationships,omitempty"`
	Text             string   `json:"text,omitempty"`
	SelectedIssueUID string   `json:"selected_issue_uid,omitempty"`
	IncludeGraph     bool     `json:"include_graph"`
	IncludeHistory   bool     `json:"include_history"`
	LocalDate        string   `json:"local_date,omitempty"`
	TimeZone         string   `json:"time_zone,omitempty"`
	ReadyAt          string   `json:"ready_at,omitempty"`
	DefaultTimezone  string   `json:"default_timezone,omitempty"`
	Limit            int      `json:"limit"`
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
	Query      string `json:"query,omitempty"`
	ProjectUID string `json:"project_uid,omitempty"`
	Limit      int    `json:"limit"`
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

			authorityKey, err := uiSnapshotAuthorityKey(intent)
			if err != nil {
				return nil, internalAPIError(err)
			}
			cachedAuthority, authorityCached := authorityCache.get(authorityKey)
			if !authorityCached && intent.ProjectUID != "" && intent.Limit == 0 {
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
			cachedEnrichment, enrichmentCached := enrichmentCache.get(enrichmentKey)
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
			} else {
				authorityCache.put(authorityKey, data)
			}
			if uiSnapshotHasEnrichment(intent) {
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
			data, err := cfg.UIStore.ReadUIReferences(ctx, db.UIReferencesQuery{
				Query: intent.Query, ProjectUID: intent.ProjectUID, Limit: intent.Limit,
			})
			if err != nil {
				return nil, internalAPIError(err)
			}
			sortUIReferences(&data)
			validator, err := makeUIETag(intent, data.Cursor, policy)
			if err != nil {
				return nil, internalAPIError(err)
			}
			return referencesResponse(data, policy, validator), nil
		})
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
		timeZone = ""
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
	limit, err := normalizeUILimit(in.Limit, uiReferencesLimitDefault, uiReferencesLimitMax)
	if err != nil {
		return normalizedUIReferencesIntent{}, err
	}
	return normalizedUIReferencesIntent{
		Query: strings.TrimSpace(in.Query), ProjectUID: projectUID, Limit: limit,
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
	out.Body.Catalog = nonNil(data.Projects)
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
	out.Body.Projects = nonNil(data.Projects)
	out.Body.Issues = nonNil(data.Issues)
	out.Body.Owners = nonNil(data.Owners)
	out.Body.Labels = nonNil(data.Labels)
	return out
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
