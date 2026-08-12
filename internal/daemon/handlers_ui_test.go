package daemon_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

type countingUIStore struct {
	cursor           int64
	snapshotCursor   int64
	cursorReads      int
	snapshotReads    int
	referenceReads   int
	hydrationReads   int
	snapshot         db.UISnapshotData
	projectSnapshots map[string]db.UISnapshotData
	references       db.UIReferencesData
	hydration        db.UIReferenceHydration
	lastSnapshot     db.UISnapshotQuery
	lastReferences   db.UIReferencesQuery
}

func (s *countingUIStore) UIEventCursor(context.Context) (int64, error) {
	s.cursorReads++
	return s.cursor, nil
}

func (s *countingUIStore) ReadUISnapshot(_ context.Context, query db.UISnapshotQuery) (db.UISnapshotData, error) {
	s.snapshotReads++
	s.lastSnapshot = query
	data := s.snapshot
	if projectSnapshot, ok := s.projectSnapshots[query.ProjectUID]; ok {
		data = projectSnapshot
	}
	data.Cursor = s.snapshotCursor
	if query.ReuseAuthorityCursor != nil && *query.ReuseAuthorityCursor == data.Cursor {
		data.Projects = nil
		data.Issues = nil
		data.CollectionLinks = nil
		data.AuthorityReused = true
	}
	return data, nil
}

func (s *countingUIStore) ReadUIReferenceHydration(
	_ context.Context,
	query db.UIReferencesQuery,
) (db.UIReferenceHydration, error) {
	s.hydrationReads++
	s.lastReferences = query
	data := s.hydration
	if data.ResolvedUIDs == nil {
		data.ResolvedUIDs = append([]string(nil), query.IssueUIDs...)
	}
	data.References.Cursor = s.snapshotCursor
	return data, nil
}

func (s *countingUIStore) ReadUIReferences(_ context.Context, query db.UIReferencesQuery) (db.UIReferencesData, error) {
	s.referenceReads++
	s.lastReferences = query
	data := s.references
	data.Cursor = s.snapshotCursor
	return data, nil
}

func newUISnapshotServer(t *testing.T, uiStore db.UIStore, writable bool) *httptest.Server {
	return newUISnapshotServerWithClock(t, uiStore, writable, time.Now)
}

func newUISnapshotServerWithClock(
	t *testing.T,
	uiStore db.UIStore,
	writable bool,
	clock func() time.Time,
) *httptest.Server {
	t.Helper()
	dbh := openTestDB(t)
	manager, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{
		Origin:       "https://daemon.example",
		OriginStable: true,
		InstanceID:   "example",
		Writable:     writable,
		Updates:      "sse",
		Auth:         config.AuthConfig{},
		DB:           dbh.db,
	})
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{
		DB:          dbh.db,
		UIStore:     uiStore,
		UIClock:     clock,
		StartedAt:   dbh.now,
		WebSessions: manager,
	})
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestUISnapshotReadyTimeAdvanceInvalidatesETagAndAuthorityCache(t *testing.T) {
	store := &countingUIStore{cursor: 41, snapshotCursor: 41}
	now := time.Date(2026, 8, 8, 23, 59, 0, 0, time.UTC)
	ts := newUISnapshotServerWithClock(t, store, true, func() time.Time { return now })
	query := url.Values{"view": {"all-open"}, "status": {"ready"}}

	first, _ := getUISnapshot(t, ts, query, "")
	require.Equal(t, http.StatusOK, first.StatusCode)
	require.Equal(t, "2026-08-08T23:59:00Z", store.lastSnapshot.ReadyAt)
	require.Equal(t, 1, store.snapshotReads)

	now = now.Add(2 * time.Minute)
	second, body := getUISnapshot(t, ts, query, first.Header.Get("ETag"))
	require.Equal(t, http.StatusOK, second.StatusCode, string(body))
	require.NotEqual(t, first.Header.Get("ETag"), second.Header.Get("ETag"))
	require.Equal(t, "2026-08-09T00:01:00Z", store.lastSnapshot.ReadyAt)
	require.Equal(t, 2, store.snapshotReads)
}

func getUISnapshot(t *testing.T, ts *httptest.Server, query url.Values, etag string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/ui/snapshot?"+query.Encode(), nil)
	require.NoError(t, err)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, body
}

func TestUISnapshotETagFastPath(t *testing.T) {
	store := &countingUIStore{cursor: 41, snapshotCursor: 41}
	ts := newUISnapshotServer(t, store, true)
	query := url.Values{"view": {"inbox"}, "label": {"backend", "urgent"}}

	first, _ := getUISnapshot(t, ts, query, "")
	require.Equal(t, http.StatusOK, first.StatusCode)
	etag := first.Header.Get("ETag")
	require.NotEmpty(t, etag)
	require.Equal(t, []string{"backend", "urgent"}, store.lastSnapshot.Labels)

	store.cursorReads = 0
	store.snapshotReads = 0
	second, body := getUISnapshot(t, ts, url.Values{
		"view": {"inbox"}, "label": {"urgent", "backend"},
	}, etag)
	require.Equal(t, http.StatusNotModified, second.StatusCode, string(body))
	require.Empty(t, body)
	require.Equal(t, 1, store.cursorReads)
	require.Zero(t, store.snapshotReads, "matching ETag must not build the projection")
}

func TestUISnapshotWithoutLimitRequestsCompleteAuthority(t *testing.T) {
	store := &countingUIStore{cursor: 41, snapshotCursor: 41}
	ts := newUISnapshotServer(t, store, true)

	response, body := getUISnapshot(t, ts, url.Values{"view": {"all-open"}}, "")
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	require.Zero(t, store.lastSnapshot.Limit)
}

func TestUISnapshotPreservesRelationshipIntentAndCollectionLinks(t *testing.T) {
	store := &countingUIStore{
		cursor: 41, snapshotCursor: 41,
		snapshot: db.UISnapshotData{CollectionLinks: []db.UILink{{
			Link: db.Link{FromIssueUID: "01J00000000000000000000001", ToIssueUID: "01J00000000000000000000002", Type: "parent"},
		}}},
	}
	ts := newUISnapshotServer(t, store, true)
	response, body := getUISnapshot(t, ts, url.Values{
		"view": {"all-open"}, "relationship": {"parent", "blocked_by"},
	}, "")
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	require.Equal(t, []string{"blocked_by", "parent"}, store.lastSnapshot.Relationships)
	var envelope struct {
		CollectionLinks []db.UILink `json:"collection_links"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	require.Len(t, envelope.CollectionLinks, 1)
}

func TestUISnapshotPreservesUnresolvedGraphEndpoints(t *testing.T) {
	store := &countingUIStore{
		cursor: 41, snapshotCursor: 41,
		snapshot: db.UISnapshotData{
			GraphEdges: []db.UIGraphEdge{{
				FromUID: "01J00000000000000000000001", ToUID: "01J00000000000000000000002",
				Kind: "blocks", Layout: true,
			}},
			GraphUnresolvedRefs: []db.UIGraphUnresolvedRef{{
				UID: "01J00000000000000000000002", Side: "to", Kind: "blocks",
				OtherUID: "01J00000000000000000000001",
			}},
		},
	}
	ts := newUISnapshotServer(t, store, true)
	response, body := getUISnapshot(t, ts, url.Values{
		"view": {"all-open"}, "include_graph": {"true"},
	}, "")
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	var envelope struct {
		Graph struct {
			Edges          []db.UIGraphEdge          `json:"edges"`
			UnresolvedRefs []db.UIGraphUnresolvedRef `json:"unresolved_refs"`
		} `json:"graph"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	require.Len(t, envelope.Graph.Edges, 1)
	require.Len(t, envelope.Graph.UnresolvedRefs, 1)
}

func TestUISnapshotTemporalIntent(t *testing.T) {
	store := &countingUIStore{cursor: 9, snapshotCursor: 9}
	ts := newUISnapshotServer(t, store, true)

	for _, view := range []string{"today", "upcoming", "deadlines"} {
		t.Run(view, func(t *testing.T) {
			first, _ := getUISnapshot(t, ts, url.Values{
				"view": {view}, "local_date": {"2026-08-01"}, "time_zone": {"America/Chicago"},
			}, "")
			second, _ := getUISnapshot(t, ts, url.Values{
				"view": {view}, "local_date": {"2026-08-02"}, "time_zone": {"America/New_York"},
			}, "")
			require.NotEqual(t, first.Header.Get("ETag"), second.Header.Get("ETag"))
		})
	}

	for _, view := range []string{"inbox", "all-open", "logbook"} {
		t.Run(view, func(t *testing.T) {
			first, _ := getUISnapshot(t, ts, url.Values{
				"view": {view}, "time_zone": {"America/Chicago"},
			}, "")
			require.Equal(t, "America/Chicago", store.lastSnapshot.TimeZone)
			require.Empty(t, store.lastSnapshot.LocalDate)
			second, _ := getUISnapshot(t, ts, url.Values{
				"view": {view}, "time_zone": {"America/New_York"},
			}, "")
			require.NotEqual(t, first.Header.Get("ETag"), second.Header.Get("ETag"))
		})
	}
}

func TestUISnapshotTemporalValidation(t *testing.T) {
	store := &countingUIStore{cursor: 9, snapshotCursor: 9}
	ts := newUISnapshotServer(t, store, true)
	tests := []url.Values{
		{"view": {"today"}, "local_date": {"August 1"}, "time_zone": {"America/Chicago"}},
		{"view": {"today"}, "local_date": {"2026-08-01"}, "time_zone": {"Mars/Olympus"}},
	}
	for _, query := range tests {
		resp, body := getUISnapshot(t, ts, query, "")
		assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "validation")
	}
	require.Zero(t, store.cursorReads)
	require.Zero(t, store.snapshotReads)
}

func TestUISnapshotConsistentCursor(t *testing.T) {
	store := &countingUIStore{cursor: 10, snapshotCursor: 11}
	ts := newUISnapshotServer(t, store, true)
	query := url.Values{"view": {"inbox"}}

	first, body := getUISnapshot(t, ts, query, "")
	require.Equal(t, http.StatusOK, first.StatusCode, string(body))
	var envelope struct {
		Cursor int64 `json:"cursor"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	require.Equal(t, int64(11), envelope.Cursor)

	store.cursor = 11
	store.cursorReads = 0
	store.snapshotReads = 0
	second, _ := getUISnapshot(t, ts, query, first.Header.Get("ETag"))
	require.Equal(t, http.StatusNotModified, second.StatusCode)
	require.Equal(t, 1, store.cursorReads)
	require.Zero(t, store.snapshotReads)
}

func TestUISnapshotCapabilities(t *testing.T) {
	writableStore := &countingUIStore{cursor: 3, snapshotCursor: 3}
	readonlyStore := &countingUIStore{cursor: 3, snapshotCursor: 3}
	writable := newUISnapshotServer(t, writableStore, true)
	readonly := newUISnapshotServer(t, readonlyStore, false)
	query := url.Values{"view": {"inbox"}}

	writableResponse, writableBody := getUISnapshot(t, writable, query, "")
	readonlyResponse, readonlyBody := getUISnapshot(t, readonly, query, "")
	require.NotEqual(t, writableResponse.Header.Get("ETag"), readonlyResponse.Header.Get("ETag"))
	var first, second struct {
		Origin       string `json:"origin"`
		OriginStable bool   `json:"origin_stable"`
		Capabilities struct {
			Writable bool   `json:"writable"`
			Updates  string `json:"updates"`
		} `json:"capabilities"`
	}
	require.NoError(t, json.Unmarshal(writableBody, &first))
	require.NoError(t, json.Unmarshal(readonlyBody, &second))
	require.True(t, first.Capabilities.Writable)
	require.False(t, second.Capabilities.Writable)
	require.Equal(t, "sse", first.Capabilities.Updates)
	require.Equal(t, "https://daemon.example", first.Origin)
	require.True(t, first.OriginStable)
}

func TestUISnapshotETagRejectsShortRouteReference(t *testing.T) {
	store := &countingUIStore{cursor: 1, snapshotCursor: 1}
	ts := newUISnapshotServer(t, store, true)
	resp, body := getUISnapshot(t, ts, url.Values{
		"view": {"inbox"}, "selected_issue_uid": {"abc4"},
	}, "")
	assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "validation")
	var envelope struct {
		Error struct {
			Data map[string]any `json:"data"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	require.Equal(t, "abc4", envelope.Error.Data["search_ref"])
	require.Zero(t, store.cursorReads)
	require.Zero(t, store.snapshotReads)
}

var _ db.UIStore = (*countingUIStore)(nil)

func TestUISnapshotSelectedContext(t *testing.T) {
	selectedUID := "01J00000000000000000000000"
	store := &countingUIStore{
		cursor: 5, snapshotCursor: 5,
		snapshot: db.UISnapshotData{SelectedState: "archived"},
	}
	ts := newUISnapshotServer(t, store, true)
	resp, body := getUISnapshot(t, ts, url.Values{
		"view": {"inbox"}, "selected_issue_uid": {selectedUID},
	}, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var envelope struct {
		Selected struct {
			State string `json:"state"`
		} `json:"selected"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	require.Equal(t, "archived", envelope.Selected.State)
}

func TestUISnapshotReusesCollectionAuthorityAcrossSelection(t *testing.T) {
	selectedUID := "01J00000000000000000000000"
	store := &countingUIStore{
		cursor: 5, snapshotCursor: 5,
		snapshot: db.UISnapshotData{
			Projects:      []db.UIProject{{Project: db.Project{UID: "01J00000000000000000000001", Name: "example-project"}}},
			Issues:        []db.UIIssue{{Issue: db.Issue{UID: selectedUID, Title: "Selected issue"}}},
			SelectedState: "available",
			SelectedIssue: &db.UIIssue{Issue: db.Issue{UID: selectedUID, Title: "Selected issue"}},
		},
	}
	ts := newUISnapshotServer(t, store, true)

	first, _ := getUISnapshot(t, ts, url.Values{"view": {"all-open"}}, "")
	require.Equal(t, http.StatusOK, first.StatusCode)
	second, body := getUISnapshot(t, ts, url.Values{
		"view": {"all-open"}, "selected_issue_uid": {selectedUID}, "include_history": {"true"},
	}, "")
	require.Equal(t, http.StatusOK, second.StatusCode, string(body))
	require.NotNil(t, store.lastSnapshot.ReuseAuthorityCursor)
	require.Equal(t, int64(5), *store.lastSnapshot.ReuseAuthorityCursor)

	var envelope struct {
		Catalog    []db.UIProject `json:"catalog"`
		Collection []db.UIIssue   `json:"collection"`
		Selected   struct {
			State string `json:"state"`
		} `json:"selected"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	require.Len(t, envelope.Catalog, 1)
	require.Len(t, envelope.Collection, 1)
	require.Equal(t, "available", envelope.Selected.State)
}

func TestUISnapshotReusesRecentSelectedEnrichment(t *testing.T) {
	firstUID := "01J00000000000000000000000"
	secondUID := "01J00000000000000000000001"
	store := &countingUIStore{cursor: 8, snapshotCursor: 8}
	ts := newUISnapshotServer(t, store, true)
	query := func(uid string) url.Values {
		return url.Values{
			"view": {"all-open"}, "selected_issue_uid": {uid}, "include_history": {"true"},
		}
	}

	first, _ := getUISnapshot(t, ts, query(firstUID), "")
	require.Equal(t, http.StatusOK, first.StatusCode)
	second, _ := getUISnapshot(t, ts, query(secondUID), "")
	require.Equal(t, http.StatusOK, second.StatusCode)
	revisited, _ := getUISnapshot(t, ts, query(firstUID), "")
	require.Equal(t, http.StatusOK, revisited.StatusCode)
	require.Equal(t, 2, store.snapshotReads)

	store.cursor = 9
	store.snapshotCursor = 9
	invalidated, _ := getUISnapshot(t, ts, query(firstUID), "")
	require.Equal(t, http.StatusOK, invalidated.StatusCode)
	require.Equal(t, 3, store.snapshotReads)
}

func TestUISnapshotDerivesProjectAuthorityFromCachedGlobalCollection(t *testing.T) {
	firstProjectUID := "01J00000000000000000000001"
	secondProjectUID := "01J00000000000000000000002"
	firstIssueUID := "01J00000000000000000000003"
	secondIssueUID := "01J00000000000000000000004"
	store := &countingUIStore{
		cursor: 13, snapshotCursor: 13,
		snapshot: db.UISnapshotData{
			Projects: []db.UIProject{
				{Project: db.Project{ID: 1, UID: firstProjectUID, Name: "example-project"}},
				{Project: db.Project{ID: 2, UID: secondProjectUID, Name: "example-peer"}},
			},
			Issues: []db.UIIssue{
				{Issue: db.Issue{ID: 3, UID: firstIssueUID, ProjectID: 1, ProjectUID: firstProjectUID}},
				{Issue: db.Issue{ID: 4, UID: secondIssueUID, ProjectID: 2, ProjectUID: secondProjectUID}},
			},
			CollectionLinks: []db.UILink{{Link: db.Link{
				FromIssueID: 3, FromIssueUID: firstIssueUID,
				ToIssueID: 4, ToIssueUID: secondIssueUID,
			}}},
		},
		projectSnapshots: map[string]db.UISnapshotData{
			firstProjectUID: {
				Projects: []db.UIProject{
					{Project: db.Project{ID: 1, UID: firstProjectUID, Name: "example-project"}},
					{Project: db.Project{ID: 2, UID: secondProjectUID, Name: "example-peer"}},
				},
				Issues: []db.UIIssue{
					{Issue: db.Issue{ID: 3, UID: firstIssueUID, ProjectID: 1, ProjectUID: firstProjectUID}},
				},
				CollectionLinks: []db.UILink{{Link: db.Link{
					FromIssueID: 3, FromIssueUID: firstIssueUID,
					ToIssueID: 4, ToIssueUID: secondIssueUID,
				}}},
			},
		},
	}
	directServer := newUISnapshotServer(t, store, true)
	_, directBody := getUISnapshot(t, directServer, url.Values{
		"view": {"all-open"}, "project_uid": {firstProjectUID},
	}, "")
	var directEnvelope struct {
		Collection      []db.UIIssue `json:"collection"`
		CollectionLinks []db.UILink  `json:"collection_links"`
	}
	require.NoError(t, json.Unmarshal(directBody, &directEnvelope))

	cachedServer := newUISnapshotServer(t, store, true)

	global, _ := getUISnapshot(t, cachedServer, url.Values{"view": {"all-open"}}, "")
	require.Equal(t, http.StatusOK, global.StatusCode)
	project, body := getUISnapshot(t, cachedServer, url.Values{
		"view": {"all-open"}, "project_uid": {firstProjectUID},
	}, "")
	require.Equal(t, http.StatusOK, project.StatusCode, string(body))
	require.Equal(t, 2, store.snapshotReads)

	var envelope struct {
		Collection      []db.UIIssue `json:"collection"`
		CollectionLinks []db.UILink  `json:"collection_links"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	require.Equal(t, directEnvelope.Collection, envelope.Collection)
	require.Equal(t, directEnvelope.CollectionLinks, envelope.CollectionLinks)
}

func TestUIReferencesBoundedCapabilities(t *testing.T) {
	store := &countingUIStore{
		cursor: 7, snapshotCursor: 7,
		references: db.UIReferencesData{
			Projects: []db.Project{{Name: "z-project", UID: "01J00000000000000000000002"}, {Name: "a-project", UID: "01J00000000000000000000001"}},
			Issues: []db.UIIssueReference{
				{QualifiedID: "z-project#z9", UID: "01J00000000000000000000004"},
				{QualifiedID: "a-project#a1", UID: "01J00000000000000000000003"},
			},
			Owners: []string{"user-b", "user-a"}, Labels: []string{"urgent", "backend"},
		},
	}
	ts := newUISnapshotServer(t, store, false)
	resp, body := getUIReferences(t, ts, url.Values{"q": {"example"}, "limit": {"999"}}, "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Equal(t, 1, store.referenceReads)
	require.Equal(t, 200, store.lastReferences.Limit)
	var envelope struct {
		ContractVersion string `json:"contract_version"`
		Origin          string `json:"origin"`
		OriginStable    bool   `json:"origin_stable"`
		Capabilities    struct {
			Writable    bool   `json:"writable"`
			Updates     string `json:"updates"`
			ActorPolicy string `json:"actor_policy"`
		} `json:"capabilities"`
		Projects []db.Project          `json:"projects"`
		Issues   []db.UIIssueReference `json:"issues"`
		Owners   []string              `json:"owners"`
		Labels   []string              `json:"labels"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	require.NotEmpty(t, envelope.ContractVersion)
	require.Equal(t, "https://daemon.example", envelope.Origin)
	require.True(t, envelope.OriginStable)
	require.False(t, envelope.Capabilities.Writable)
	require.Equal(t, "sse", envelope.Capabilities.Updates)
	require.Equal(t, "request", envelope.Capabilities.ActorPolicy)
	require.Equal(t, []string{"a-project", "z-project"}, []string{envelope.Projects[0].Name, envelope.Projects[1].Name})
	require.Equal(t, []string{"a-project#a1", "z-project#z9"}, []string{envelope.Issues[0].QualifiedID, envelope.Issues[1].QualifiedID})
	require.Equal(t, []string{"user-a", "user-b"}, envelope.Owners)
	require.Equal(t, []string{"backend", "urgent"}, envelope.Labels)
}

func TestUIReferencesPreservesStableIssueUIDFilters(t *testing.T) {
	store := &countingUIStore{cursor: 7, snapshotCursor: 7}
	ts := newUISnapshotServer(t, store, false)
	first := "01J00000000000000000000002"
	second := "01J00000000000000000000001"

	resp, body := getUIReferences(t, ts, url.Values{
		"issue_uid": {" " + first + " ", second, first},
	}, "")

	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Equal(t, 1, store.hydrationReads)
	require.Zero(t, store.referenceReads)
	require.Equal(t, []string{second, first}, store.lastReferences.IssueUIDs)
}

func TestUIReferencesExpandsDefaultLimitForStableIssueUIDFilters(t *testing.T) {
	store := &countingUIStore{cursor: 7, snapshotCursor: 7}
	ts := newUISnapshotServer(t, store, false)
	issueUIDs := make([]string, 0, 150)
	for range 150 {
		issueUID, err := uid.New()
		require.NoError(t, err)
		issueUIDs = append(issueUIDs, issueUID)
	}

	resp, body := getUIReferences(t, ts, url.Values{"issue_uid": issueUIDs}, "")

	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Equal(t, 150, store.lastReferences.Limit)
}

func TestUIReferencesPreservesExplicitLimitWithStableIssueUIDFilters(t *testing.T) {
	store := &countingUIStore{cursor: 7, snapshotCursor: 7}
	ts := newUISnapshotServer(t, store, false)
	issueUIDs := make([]string, 0, 150)
	for range 150 {
		issueUID, err := uid.New()
		require.NoError(t, err)
		issueUIDs = append(issueUIDs, issueUID)
	}

	resp, body := getUIReferences(t, ts, url.Values{
		"issue_uid": issueUIDs,
		"limit":     {"50"},
	}, "")

	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Equal(t, 50, store.lastReferences.Limit)
}

type scopedUIReferencesHostAccess struct {
	deniedProjectID int64
	requests        []daemon.HostAccessRequest
}

func (a *scopedUIReferencesHostAccess) Authorize(
	_ context.Context,
	request daemon.HostAccessRequest,
) (daemon.HostAccessDecision, error) {
	a.requests = append(a.requests, request)
	for _, projectID := range request.Operation.ProjectIDs {
		if projectID == a.deniedProjectID {
			return daemon.HostAccessDecision{}, daemon.ErrHostAccessDenied
		}
	}
	return daemon.HostAccessDecision{}, nil
}

func TestUIReferencesHostHydrationAuthorizesCapturedScope(t *testing.T) {
	firstIssueUID, err := uid.New()
	require.NoError(t, err)
	secondIssueUID, err := uid.New()
	require.NoError(t, err)
	resolvedUIDs := []string{firstIssueUID, secondIssueUID}
	slices.Sort(resolvedUIDs)
	projectUID, err := uid.New()
	require.NoError(t, err)
	reference := db.UIIssueReference{
		UID: firstIssueUID, ProjectUID: projectUID, ProjectName: "restricted-project",
		ShortID: "a1", QualifiedID: "restricted-project#a1", Title: "Restricted issue", Status: "open",
	}
	store := &countingUIStore{
		cursor: 31, snapshotCursor: 31,
		references: db.UIReferencesData{Issues: []db.UIIssueReference{reference}},
		hydration: db.UIReferenceHydration{
			References:   db.UIReferencesData{Issues: []db.UIIssueReference{reference}},
			ResolvedUIDs: resolvedUIDs,
			ProjectIDs:   []int64{42, 84},
		},
	}
	access := &scopedUIReferencesHostAccess{}
	dbh := openTestDB(t)
	manager, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{
		Origin: "https://daemon.example", OriginStable: true, InstanceID: "example",
		Auth: config.AuthConfig{}, DB: dbh.db,
	})
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{
		DB: dbh.db, UIStore: store, StartedAt: dbh.now, WebSessions: manager, HostAccess: access,
	})
	ts := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx := daemon.WithPrincipal(request.Context(), daemon.Principal{
			Kind: daemon.PrincipalHost, Subject: "host-user", Actor: "user-a",
		})
		server.Handler().ServeHTTP(writer, request.WithContext(ctx))
	}))
	t.Cleanup(ts.Close)
	query := url.Values{"issue_uid": {firstIssueUID, secondIssueUID}, "q": {"Restricted"}}

	first, body := getUIReferences(t, ts, query, "")
	require.Equal(t, http.StatusOK, first.StatusCode, string(body))
	etag := first.Header.Get("ETag")
	require.NotEmpty(t, etag)

	access.deniedProjectID = 84
	second, body := getUIReferences(t, ts, query, etag)
	require.Equal(t, http.StatusNotFound, second.StatusCode, string(body))
	require.JSONEq(t,
		`{"status":404,"error":{"code":"not_found","message":"resource not found"}}`,
		string(body),
	)
	require.Equal(t, 2, store.hydrationReads)
	require.Zero(t, store.referenceReads)
	require.Len(t, access.requests, 2)
	require.Equal(t, []int64{42, 84}, access.requests[1].Operation.ProjectIDs)
	require.False(t, access.requests[1].Operation.AllProjects)
}

func TestUIReferencesHostHydrationConcealsUnavailableUID(t *testing.T) {
	firstIssueUID, err := uid.New()
	require.NoError(t, err)
	missingIssueUID, err := uid.New()
	require.NoError(t, err)
	store := &countingUIStore{
		cursor: 31, snapshotCursor: 31,
		hydration: db.UIReferenceHydration{
			ResolvedUIDs: []string{firstIssueUID},
			ProjectIDs:   []int64{42},
		},
	}
	access := &scopedUIReferencesHostAccess{}
	dbh := openTestDB(t)
	manager, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{
		Origin: "https://daemon.example", OriginStable: true, InstanceID: "example",
		Auth: config.AuthConfig{}, DB: dbh.db,
	})
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{
		DB: dbh.db, UIStore: store, StartedAt: dbh.now, WebSessions: manager, HostAccess: access,
	})
	ts := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx := daemon.WithPrincipal(request.Context(), daemon.Principal{
			Kind: daemon.PrincipalHost, Subject: "host-user", Actor: "user-a",
		})
		server.Handler().ServeHTTP(writer, request.WithContext(ctx))
	}))
	t.Cleanup(ts.Close)

	resp, body := getUIReferences(t, ts, url.Values{
		"issue_uid": {firstIssueUID, missingIssueUID},
	}, "")

	require.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
	require.JSONEq(t,
		`{"status":404,"error":{"code":"not_found","message":"resource not found"}}`,
		string(body),
	)
	require.Empty(t, access.requests)
}

func TestUIReferencesRejectsMalformedIssueUIDFilter(t *testing.T) {
	store := &countingUIStore{cursor: 7, snapshotCursor: 7}
	ts := newUISnapshotServer(t, store, false)

	resp, body := getUIReferences(t, ts, url.Values{"issue_uid": {"not-a-uid"}}, "")

	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
	require.Zero(t, store.referenceReads)
	require.Zero(t, store.hydrationReads)
}

func TestUIReferencesRejectsTooManyIssueUIDFilters(t *testing.T) {
	store := &countingUIStore{cursor: 7, snapshotCursor: 7}
	ts := newUISnapshotServer(t, store, false)
	issueUIDs := make([]string, 0, 201)
	for range 201 {
		issueUID, err := uid.New()
		require.NoError(t, err)
		issueUIDs = append(issueUIDs, issueUID)
	}

	resp, body := getUIReferences(t, ts, url.Values{"issue_uid": issueUIDs}, "")

	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
	require.Zero(t, store.referenceReads)
	require.Zero(t, store.hydrationReads)
}

func getUIReferences(t *testing.T, ts *httptest.Server, query url.Values, etag string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/ui/references?"+query.Encode(), nil)
	require.NoError(t, err)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, body
}
