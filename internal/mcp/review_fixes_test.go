package mcpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/storageadmin"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestReadEventStreamAcceptsLargeEvents(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"body": strings.Repeat("x", 70<<10), "link_to_uid": "foreign-uid", "type": "related",
	})
	require.NoError(t, err)
	event := generated.EventEnvelope{
		EventID: 1, EventUID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Type: "links_changed",
		Actor: "example-agent", CreatedAt: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
		ProjectID: 1, ProjectUID: "01HBBBBBBBBBBBBBBBBBBBBBBB", ProjectName: "spoke-project",
		Payload: payload, RelatedIssueUID: optionalString("foreign-uid"), RelatedIssueShortID: optionalString("out1"),
	}
	encoded, err := json.Marshal(event)
	require.NoError(t, err)
	stream := "event: links_changed\ndata: " + string(encoded) + "\n\n"

	output, err := readEventStream(t.Context(), strings.NewReader(stream), 0, 1)
	require.NoError(t, err)
	require.Len(t, output.Events, 1)
	require.Equal(t, "foreign-uid", output.Events[0].RelatedIssueUID)
	decoded := output.Events[0].Payload.(map[string]any)
	require.Len(t, decoded["body"], 70<<10)
}

func TestEventPeerRedactionPreservesAllowedPeers(t *testing.T) {
	allowedUID := "01HCCCCCCCCCCCCCCCCCCCCCCC"
	foreignUID := "01HDDDDDDDDDDDDDDDDDDDDDDD"
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
				projectJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "other-project"),
			}})
		case strings.HasPrefix(request.URL.Path, "/api/v1/issues/"):
			uid := strings.TrimPrefix(request.URL.Path, "/api/v1/issues/")
			projectID, projectName := int64(1), "spoke-project"
			if uid == foreignUID {
				projectID, projectName = 2, "other-project"
			}
			issue := issueJSON(projectID, projectName, "peer1")
			issue["uid"] = uid
			writeJSON(writer, map[string]any{
				"issue": issue, "labels": []any{}, "comments": []any{}, "links": []any{},
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	output := EventsOutput{Events: []StreamEvent{
		{RelatedIssueUID: allowedUID, RelatedIssueShortID: "in1", Payload: map[string]any{"link_to_uid": allowedUID}},
		{RelatedIssueUID: foreignUID, RelatedIssueShortID: "out1", Payload: map[string]any{"link_to_uid": foreignUID}},
	}}

	require.NoError(t, handlers.redactEventsOutsideScope(t.Context(), &output))
	require.Equal(t, allowedUID, output.Events[0].RelatedIssueUID)
	require.Equal(t, allowedUID, output.Events[0].Payload.(map[string]any)["link_to_uid"])
	require.Empty(t, output.Events[1].RelatedIssueUID)
	require.Empty(t, output.Events[1].RelatedIssueShortID)
	require.NotContains(t, output.Events[1].Payload.(map[string]any), "link_to_uid")
}

func TestEventPeerRedactionPropagatesTransientLookupFailure(t *testing.T) {
	peerUID := "01HCCCCCCCCCCCCCCCCCCCCCCC"
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case "/api/v1/issues/" + peerUID:
			writer.WriteHeader(http.StatusInternalServerError)
			writeJSON(writer, map[string]any{
				"status": http.StatusInternalServerError,
				"error":  map[string]any{"code": "internal_error", "message": "try again"},
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	output := EventsOutput{Events: []StreamEvent{{
		RelatedIssueUID: peerUID, RelatedIssueShortID: "in1",
		Payload: map[string]any{"link_to_uid": peerUID},
	}}}

	err = handlers.redactEventsOutsideScope(t.Context(), &output)
	require.ErrorContains(t, err, "resolve event peer")
	require.NotContains(t, err.Error(), peerUID)
	require.NotContains(t, err.Error(), "try again")
	require.Equal(t, peerUID, output.Events[0].RelatedIssueUID)
	require.Equal(t, peerUID, output.Events[0].Payload.(map[string]any)["link_to_uid"])
}

func TestEventPeerRedactionCoversParentLinkAndArrayShapes(t *testing.T) {
	allowedUID := "01HCCCCCCCCCCCCCCCCCCCCCCC"
	foreignUID := "01HDDDDDDDDDDDDDDDDDDDDDDD"
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case strings.HasPrefix(request.URL.Path, "/api/v1/issues/"):
			uid := strings.TrimPrefix(request.URL.Path, "/api/v1/issues/")
			projectID, projectName := int64(1), "spoke-project"
			if uid == foreignUID {
				projectID, projectName = 2, "other-project"
			}
			issue := issueJSON(projectID, projectName, "peer1")
			issue["uid"] = uid
			writeJSON(writer, map[string]any{
				"issue": issue, "labels": []any{}, "comments": []any{}, "links": []any{},
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	output := EventsOutput{Events: []StreamEvent{
		{Payload: map[string]any{
			"parent_set": "in1", "parent_set_uid": allowedUID,
			"blocks_added": []any{"in1", "out1"}, "blocks_added_uids": []any{allowedUID, foreignUID},
			"links": []any{map[string]any{"type": "blocks", "to_short_id": "out1", "to_issue_uid": foreignUID}},
		}},
		{Payload: map[string]any{
			"parent_removed": "out1", "parent_removed_uid": foreignUID,
			"related_added": []any{"out1"}, "related_added_uids": []any{foreignUID},
		}},
	}}

	require.NoError(t, handlers.redactEventsOutsideScope(t.Context(), &output))
	first := output.Events[0].Payload.(map[string]any)
	require.Equal(t, allowedUID, first["parent_set_uid"])
	require.Equal(t, "in1", first["parent_set"])
	require.Equal(t, []any{allowedUID}, first["blocks_added_uids"])
	require.Equal(t, []any{"in1"}, first["blocks_added"])
	link := first["links"].([]any)[0].(map[string]any)
	require.NotContains(t, link, "to_issue_uid")
	require.NotContains(t, link, "to_short_id")
	second := output.Events[1].Payload.(map[string]any)
	require.NotContains(t, second, "parent_removed_uid")
	require.NotContains(t, second, "parent_removed")
	require.NotContains(t, second, "related_added_uids")
	require.NotContains(t, second, "related_added")
}

func TestEventRedactionCoversClosedParentFields(t *testing.T) {
	foreignUID := "01HDDDDDDDDDDDDDDDDDDDDDDD"
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case strings.HasPrefix(request.URL.Path, "/api/v1/issues/"):
			issue := issueJSON(2, "other-project", "peer1")
			issue["uid"] = foreignUID
			writeJSON(writer, map[string]any{
				"issue": issue, "labels": []any{}, "comments": []any{}, "links": []any{},
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	output := EventsOutput{Events: []StreamEvent{{
		Type:    "issue.closed",
		Payload: map[string]any{"reason": "done", "parent_uid": foreignUID, "parent_short_id": "p1"},
	}}}

	require.NoError(t, handlers.redactEventsOutsideScope(t.Context(), &output))
	payload := output.Events[0].Payload.(map[string]any)
	require.NotContains(t, payload, "parent_uid")
	require.NotContains(t, payload, "parent_short_id")
	require.Equal(t, "done", payload["reason"])
}

func TestDigestRedactsForeignLinkActionTargets(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case "/api/v1/projects/1/digest":
			writeJSON(writer, map[string]any{
				"project_id": 1, "event_count": 2,
				"since": "2026-08-11T00:00:00Z", "until": "2026-08-12T00:00:00Z",
				"totals": map[string]any{},
				"actors": []any{map[string]any{
					"actor": "example-agent", "totals": map[string]any{},
					"issues": []any{map[string]any{
						"issue_uid": "01HCCCCCCCCCCCCCCCCCCCCCCC", "issue_short_id": "abc1",
						"project_id": 1, "project_name": "spoke-project",
						"actions": []any{"created", "labeled:bug", "blocks:def2", "unparent:zz9", "related:xyz9", "commented:2"},
					}},
				}},
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, output, err := handlers.digest(t.Context(), nil, DigestInput{Project: "spoke-project", Since: "2026-08-11T00:00:00Z"})
	require.NoError(t, err)
	require.Len(t, output.Digests, 1)
	actions := output.Digests[0].Digest.Actors[0].Issues[0].Actions
	require.Equal(t, []string{"created", "labeled:bug", "blocks", "unparent", "related", "commented:2"}, actions,
		"historical link tokens carry no provable identity, so every target is stripped to its action type; a current same-short-ID link must not vouch a historical peer")
}

func TestAllProjectActivityUsesOnlyActiveProjectEndpoints(t *testing.T) {
	const (
		activeProjectUID   = "01HAAAAAAAAAAAAAAAAAAAAAAA"
		archivedProjectUID = "01HBBBBBBBBBBBBBBBBBBBBBBB"
	)
	activeEvent := reviewActivityEventJSON(7, 1, activeProjectUID, "active-project")
	archivedEvent := reviewActivityEventJSON(8, 2, archivedProjectUID, "archived-project")
	archivedStreamEvent, err := json.Marshal(archivedEvent)
	require.NoError(t, err)
	var globalRequests atomic.Int64
	var projectDigestRequests atomic.Int64
	var projectEventRequests atomic.Int64
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, activeProjectUID, "active-project"),
			}})
		case "/api/v1/projects/1/digest":
			projectDigestRequests.Add(1)
			writeJSON(writer, reviewDigestBodyJSON(1, "active-project"))
		case "/api/v1/projects/1/events":
			projectEventRequests.Add(1)
			writeJSON(writer, map[string]any{"events": []any{activeEvent}, "next_after_id": 7, "reset_required": false})
		case "/api/v1/digest":
			globalRequests.Add(1)
			writeJSON(writer, reviewDigestBodyJSON(2, "archived-project"))
		case "/api/v1/events":
			globalRequests.Add(1)
			writeJSON(writer, map[string]any{"events": []any{archivedEvent}, "next_after_id": 8, "reset_required": false})
		case "/api/v1/events/stream":
			globalRequests.Add(1)
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(writer, "event: issue.edited\ndata: %s\n\n", archivedStreamEvent)
		default:
			http.NotFound(writer, request)
		}
	})
	handlers := toolHandlers{options: Options{Client: client, LongRunningClient: client, Scope: NewAllScope()}}

	_, digest, err := handlers.digest(t.Context(), nil, DigestInput{Since: "2026-08-11T00:00:00Z"})
	require.NoError(t, err)
	require.Len(t, digest.Digests, 1)
	require.NotNil(t, digest.Digests[0].Project)
	require.Equal(t, "active-project", digest.Digests[0].Project.Name)
	require.Equal(t, "active-project", digest.Digests[0].Digest.Actors[0].Issues[0].ProjectName)

	_, polled, err := handlers.events(t.Context(), nil, EventsInput{Mode: "poll"})
	require.NoError(t, err)
	require.Len(t, polled.Events, 1)
	require.Equal(t, activeProjectUID, polled.Events[0].ProjectUID)

	_, waited, err := handlers.events(t.Context(), nil, EventsInput{Mode: "wait", WaitSeconds: 1})
	require.NoError(t, err)
	require.Len(t, waited.Events, 1)
	require.Equal(t, activeProjectUID, waited.Events[0].ProjectUID)
	require.Zero(t, globalRequests.Load(), "all-project activity must not use archive-inclusive global endpoints")
	require.EqualValues(t, 1, projectDigestRequests.Load())
	require.EqualValues(t, 2, projectEventRequests.Load())
}

func TestAllProjectEventPollToleratesProjectArchivedAfterCatalogRead(t *testing.T) {
	const activeProjectUID = "01HAAAAAAAAAAAAAAAAAAAAAAA"
	activeEvent := reviewActivityEventJSON(7, 1, activeProjectUID, "active-project")
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, activeProjectUID, "active-project"),
				projectJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "soon-archived-project"),
			}})
		case "/api/v1/projects/1/events":
			writeJSON(writer, map[string]any{"events": []any{activeEvent}, "next_after_id": 7, "reset_required": false})
		case "/api/v1/projects/2/events":
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusNotFound)
			writeJSON(writer, map[string]any{
				"status": 404, "error": map[string]any{"code": "project_not_found", "message": "project was archived"},
			})
		default:
			http.NotFound(writer, request)
		}
	})
	handlers := toolHandlers{options: Options{Client: client, Scope: NewAllScope()}}

	output, err := handlers.pollScopedEvents(t.Context(), "", 0, 20)
	require.NoError(t, err)
	require.Len(t, output.Events, 1)
	require.Equal(t, activeProjectUID, output.Events[0].ProjectUID)
}

func TestAllProjectDigestToleratesProjectArchivedAfterCatalogRead(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "active-project"),
				projectJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "soon-archived-project"),
			}})
		case "/api/v1/projects/1/digest":
			writeJSON(writer, reviewDigestBodyJSON(1, "active-project"))
		case "/api/v1/projects/2/digest":
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusNotFound)
			writeJSON(writer, map[string]any{
				"status": 404, "error": map[string]any{"code": "project_not_found", "message": "project was archived"},
			})
		default:
			http.NotFound(writer, request)
		}
	})
	handlers := toolHandlers{options: Options{Client: client, Scope: NewAllScope()}}

	_, output, err := handlers.digest(t.Context(), nil, DigestInput{Since: "2026-08-11T00:00:00Z"})
	require.NoError(t, err)
	require.Len(t, output.Digests, 1)
	require.NotNil(t, output.Digests[0].Project)
	require.Equal(t, "active-project", output.Digests[0].Project.Name)
}

func TestAuditPaginationInvalidatesWhenHistoryBelowCursorChanges(t *testing.T) {
	auditRow := func(eventID int, issue string) map[string]any {
		return map[string]any{"time": "2026-08-12T00:00:00Z", "actor": "example-agent", "reason": "done", "issue": issue, "event_id": eventID}
	}
	merged := false
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case "/api/v1/audit/closes":
			rows := []any{auditRow(11, "abc1"), auditRow(14, "def2"), auditRow(15, "ghi3")}
			if merged {
				// A project merge rehomed an older close event whose ID
				// falls below the first page's cursor.
				rows = []any{auditRow(11, "abc1"), auditRow(12, "hij4"), auditRow(14, "def2"), auditRow(15, "ghi3")}
			}
			writeJSON(writer, map[string]any{"rows": rows})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, first, err := handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Limit: 2})
	require.NoError(t, err)
	require.True(t, first.Truncated)
	require.NotNil(t, first.NextCursor)

	merged = true
	_, _, err = handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Limit: 2, Cursor: *first.NextCursor})
	require.ErrorContains(t, err, "restart without a cursor",
		"history merged in below the cursor must invalidate pagination, not be skipped")

	merged = false
	_, resumed, err := handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Limit: 2, Cursor: *first.NextCursor})
	require.NoError(t, err, "an unchanged prefix keeps the cursor valid")
	require.Len(t, resumed.Rows, 1)
	require.Equal(t, "ghi3", resumed.Rows[0].Issue)
}

func TestScopedServersCannotAdministerProjects(t *testing.T) {
	var requests int
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		requests++
		http.NotFound(writer, request)
	})
	scope, err := NewBoundScope(ProjectIdentity{ID: 1, Name: "spoke-project"})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, _, err = handlers.projectRemove(t.Context(), nil, ProjectRemoveInput{Project: "spoke-project"})
	require.ErrorContains(t, err, "requires the --all-projects daemon-wide scope")
	_, _, err = handlers.projectPurge(t.Context(), nil, ProjectPurgeInput{Project: "spoke-project", Confirm: "PURGE spoke-project"})
	require.ErrorContains(t, err, "requires the --all-projects daemon-wide scope")
	_, _, err = handlers.projectUpdate(t.Context(), nil, ProjectUpdateInput{Project: "spoke-project", Action: "rename", Name: "renamed"})
	require.ErrorContains(t, err, "requires the --all-projects daemon-wide scope")
	_, _, err = handlers.projectMerge(t.Context(), nil, ProjectMergeInput{Source: "spoke-project", Target: "spoke-project"})
	require.ErrorContains(t, err, "requires the --all-projects daemon-wide scope")
	_, _, err = handlers.projectRestore(t.Context(), nil, ProjectRestoreInput{Project: "spoke-project"})
	require.ErrorContains(t, err, "requires the --all-projects daemon-wide scope")
	require.Zero(t, requests, "scoped project administration must fail before any daemon request")
}

func TestMultiProjectSearchFusesByRankAndOmitsSingularProject(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
				projectJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "hub-project"),
			}})
		case "/api/v1/projects/1/search":
			// Hybrid mode: request-local reciprocal-rank scores.
			writeJSON(writer, map[string]any{"query": "work", "mode": "hybrid", "results": []any{
				map[string]any{"issue": issueJSON(1, "spoke-project", "spk1"), "score": 0.03, "matched_in": []string{"title"}},
				map[string]any{"issue": issueJSON(1, "spoke-project", "spk2"), "score": 0.02, "matched_in": []string{"title"}},
			}})
		case "/api/v1/projects/2/search":
			// Lexical fallback: large corpus-local scores.
			writeJSON(writer, map[string]any{"query": "work", "mode": "lexical", "results": []any{
				map[string]any{"issue": issueJSON(2, "hub-project", "hub1"), "score": 9.0, "matched_in": []string{"title"}},
				map[string]any{"issue": issueJSON(2, "hub-project", "hub2"), "score": 5.0, "matched_in": []string{"title"}},
			}})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{
		{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"},
		{ID: 2, UID: "01HBBBBBBBBBBBBBBBBBBBBBBB", Name: "hub-project"},
	})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, output, err := handlers.search(t.Context(), nil, SearchInput{Query: "work"})
	require.NoError(t, err)
	refs := make([]string, 0, len(output.Results))
	for _, hit := range output.Results {
		refs = append(refs, hit.Issue.QualifiedRef)
	}
	require.Equal(t, []string{"hub-project#hub1", "spoke-project#spk1", "hub-project#hub2", "spoke-project#spk2"}, refs,
		"per-project ranks interleave instead of comparing request-local scores globally")
	require.Equal(t, "mixed", output.Mode, "differing effective modes must not be mislabeled")
	require.Nil(t, output.Project, "multi-project responses omit the singular project")
	require.Len(t, output.Projects, 2)
	serialized := mustJSON(t, output)
	require.NotContains(t, string(serialized), `"project":{"id":0`)

	_, single, err := handlers.search(t.Context(), nil, SearchInput{Query: "work", Project: "spoke-project"})
	require.NoError(t, err)
	require.NotNil(t, single.Project)
	require.Equal(t, "spoke-project", single.Project.Name)
	require.Equal(t, "hybrid", single.Mode)
}

func TestEventsResponsesSerializeEmptyCollections(t *testing.T) {
	reset := false
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case "/api/v1/projects/1/events":
			if reset {
				writeJSON(writer, map[string]any{"events": []any{}, "next_after_id": 0, "reset_required": true, "reset_after_id": 7})
				return
			}
			writeJSON(writer, map[string]any{"events": []any{}, "next_after_id": 0})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, empty, err := handlers.events(t.Context(), nil, EventsInput{Project: "spoke-project", Mode: "poll"})
	require.NoError(t, err)
	require.Contains(t, string(mustJSON(t, empty)), `"events":[]`, "empty poll must serialize an array, not null")

	reset = true
	_, resetOutput, err := handlers.events(t.Context(), nil, EventsInput{Project: "spoke-project", Mode: "poll"})
	require.NoError(t, err)
	require.True(t, resetOutput.ResetRequired)
	require.Contains(t, string(mustJSON(t, resetOutput)), `"events":[]`, "reset responses must serialize an array, not null")

	reset = false
	_, timedOut, err := handlers.events(t.Context(), nil, EventsInput{Mode: "wait", WaitSeconds: 1})
	require.NoError(t, err)
	require.True(t, timedOut.TimedOut)
	require.Contains(t, string(mustJSON(t, timedOut)), `"events":[]`, "timeouts must serialize an array, not null")
}

func TestScopedCloseGuardErrorsHideForeignIdentities(t *testing.T) {
	daemonMessage := `refusing close: 2 open children remain:
  other-project#zz9  Secret roadmap item
  abc1  Local child`
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			writer.WriteHeader(http.StatusConflict)
			writeJSON(writer, map[string]any{
				"status": http.StatusConflict,
				"error":  map[string]any{"code": "parent_has_open_children", "message": daemonMessage},
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	scoped := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "example-agent"}}

	_, _, err = scoped.close(t.Context(), nil, CloseInput{
		Ref: "spoke-project#par1", Reason: "done", Message: "Completed all child work and verified the results.",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "parent_has_open_children")
	require.NotContains(t, err.Error(), "other-project", "foreign child refs must not leak through guard errors")
	require.NotContains(t, err.Error(), "Secret roadmap item", "foreign child titles must not leak through guard errors")

	wide := toolHandlers{options: Options{Client: client, Scope: NewAllScope(), Actor: "example-agent"}}
	_, _, err = wide.close(t.Context(), nil, CloseInput{
		Ref: "spoke-project#par1", Reason: "done", Message: "Completed all child work and verified the results.",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Secret roadmap item", "daemon-wide servers keep the actionable daemon message")
}

func TestScopedStorageExportRequiresDaemonWideScope(t *testing.T) {
	admin, err := storageadmin.New(storageadmin.Config{Root: t.TempDir(), SourceDSN: "source.db"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		http.NotFound(writer, request)
	})
	bound, err := NewBoundScope(ProjectIdentity{ID: 1, Name: "spoke-project"})
	require.NoError(t, err)
	allowlist, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)

	for name, scope := range map[string]*Scope{"bound": bound, "allowlist": allowlist} {
		handlers := toolHandlers{options: Options{Client: client, Scope: scope, StorageAdmin: admin}}
		_, _, err := handlers.storageExport(t.Context(), nil, StorageExportInput{Artifact: "backup.jsonl"})
		require.ErrorContains(t, err, "requires the --all-projects daemon-wide scope",
			"%s scope must not export cross-project link and payload data", name)
	}
}

func TestWaitDeadlineDuringLookupReturnsTimeout(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/projects" {
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
			return
		}
		// Hold every issue lookup past the wait deadline.
		select {
		case <-request.Context().Done():
		case <-time.After(5 * time.Second):
		}
		http.NotFound(writer, request)
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, output, err := handlers.wait(t.Context(), nil, WaitInput{
		Refs: []string{"spoke-project#abc1"}, Status: "closed", TimeoutSeconds: 1,
	})
	require.NoError(t, err, "a deadline expiring mid-lookup is the documented timeout outcome, not a tool error")
	require.Equal(t, "timeout", output.Reason)
	require.NotNil(t, output.States)
}

func TestEventsWaitDeadlineDuringPollReturnsTimeout(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/projects" {
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
				projectJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "hub-project"),
			}})
			return
		}
		select {
		case <-request.Context().Done():
		case <-time.After(5 * time.Second):
		}
		http.NotFound(writer, request)
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{
		{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"},
		{ID: 2, UID: "01HBBBBBBBBBBBBBBBBBBBBBBB", Name: "hub-project"},
	})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, output, err := handlers.events(t.Context(), nil, EventsInput{Mode: "wait", WaitSeconds: 1})
	require.NoError(t, err, "a deadline expiring mid-poll is the documented timeout outcome, not a tool error")
	require.True(t, output.TimedOut)
	require.Contains(t, string(mustJSON(t, output)), `"events":[]`)
}

func TestScopedFederationStatusRedactsErrorText(t *testing.T) {
	foreignUID := "01HDDDDDDDDDDDDDDDDDDDDDDD"
	quarantineError := "apply batch: issue.linked references unknown issue " + foreignUID
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case "/api/v1/federation/status":
			writeJSON(writer, map[string]any{"statuses": []any{map[string]any{
				"project_id": 1, "enabled": true, "enrollment_count": 0,
				"active_quarantine_count": 1,
				"last_error":              "push failed: " + foreignUID,
				"active_quarantines": []any{map[string]any{
					"id": 4, "direction": "pull", "error": quarantineError,
					"created_at": "2026-08-13T00:00:00Z", "first_event_id": 1, "last_event_id": 2,
				}},
			}}})
		case "/api/v1/federation/enrollments":
			writeJSON(writer, map[string]any{"enrollments": []any{}})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	scoped := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, output, err := scoped.federationStatus(t.Context(), nil, FederationStatusInput{})
	require.NoError(t, err)
	require.Len(t, output.Statuses, 1)
	serialized := string(mustJSON(t, output))
	require.NotContains(t, serialized, foreignUID, "stored federation errors must not leak foreign issue references to scoped servers")
	require.Equal(t, scopedFederationErrorNotice, *output.Statuses[0].LastError)
	require.Equal(t, scopedFederationErrorNotice, output.Statuses[0].ActiveQuarantines[0].ErrorData)

	wide := toolHandlers{options: Options{Client: client, Scope: NewAllScope()}}
	_, wideOutput, err := wide.federationStatus(t.Context(), nil, FederationStatusInput{})
	require.NoError(t, err)
	require.Equal(t, quarantineError, wideOutput.Statuses[0].ActiveQuarantines[0].ErrorData,
		"daemon-wide servers keep the raw diagnostic")
}

func TestEventDisplayRefsUseOnlyOwnHistoricalProjectName(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/projects" {
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
			return
		}
		http.NotFound(writer, request)
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	// Both events belong to the in-scope project; the first was stamped
	// before a rename, and "former-name" may now belong to a foreign
	// project.
	output := EventsOutput{Events: []StreamEvent{
		{Type: "close.throttled", ProjectUID: "01HAAAAAAAAAAAAAAAAAAAAAAA", ProjectName: "former-name",
			Payload: map[string]any{"reason": "sibling-burst", "parent": "former-name#zz9", "prior": "spoke-project#abc9"}},
		{Type: "issue.closed", ProjectUID: "01HAAAAAAAAAAAAAAAAAAAAAAA", ProjectName: "spoke-project",
			Payload: map[string]any{"reason": "duplicate", "evidence": []any{
				map[string]any{"type": "duplicate-of", "issue_ref": "former-name#abc2"},
			}}},
	}}

	require.NoError(t, handlers.redactEventsOutsideScope(t.Context(), &output))
	first := output.Events[0].Payload.(map[string]any)
	require.Equal(t, "former-name#zz9", first["parent"],
		"an event's own historical project name vouches its refs")
	require.NotContains(t, first, "prior",
		"a current in-scope name must not vouch an older event's ref: when the historical text was written, that name could have belonged to a foreign project")
	evidence := output.Events[1].Payload.(map[string]any)["evidence"].([]any)
	require.NotContains(t, evidence[0].(map[string]any), "issue_ref",
		"another event's historical name must not vouch this event's refs; the name may now be foreign")
}

func TestProjectsListingSurvivesMissingAllowlistMember(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/projects" {
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
			return
		}
		http.NotFound(writer, request)
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{
		{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"},
		{ID: 2, UID: "01HBBBBBBBBBBBBBBBBBBBBBBB", Name: "hub-project"},
	})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, output, err := handlers.projects(t.Context(), nil, ProjectsInput{})
	require.NoError(t, err)
	require.Len(t, output.Projects, 1)
	require.Equal(t, "spoke-project", output.Projects[0].Name)

	_, _, err = handlers.projects(t.Context(), nil, ProjectsInput{Project: "hub-project"})
	require.EqualError(t, err, `project "hub-project" in the MCP startup scope is no longer available`)
}

func TestScopedSyncEnableRequiresDaemonWideScope(t *testing.T) {
	var syncRequests []string
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case strings.Contains(request.URL.Path, "/issue-sync/"):
			syncRequests = append(syncRequests, request.URL.Path)
			writeJSON(writer, map[string]any{"status": map[string]any{"binding_id": 1, "enabled": false, "last_comments": 0, "last_created": 0}})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	scoped := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, _, err = scoped.syncUpdate(t.Context(), nil, SyncUpdateInput{
		Project: "spoke-project", Action: "enable",
		Config: map[string]any{"owner": "victim-org", "repo": "private-repo"},
	})
	require.ErrorContains(t, err, "requires the --all-projects daemon-wide scope")
	require.Empty(t, syncRequests, "scoped enable must not reach the daemon")

	_, _, err = scoped.syncUpdate(t.Context(), nil, SyncUpdateInput{Project: "spoke-project", Action: "disable"})
	require.NoError(t, err, "scoped disable of the operator-configured binding stays allowed")

	wide := toolHandlers{options: Options{Client: client, Scope: NewAllScope()}}
	_, _, err = wide.syncUpdate(t.Context(), nil, SyncUpdateInput{
		Project: "spoke-project", Action: "enable",
		Config: map[string]any{"owner": "example-org", "repo": "example-repo"},
	})
	require.NoError(t, err, "daemon-wide enable remains an operator action")
	require.Len(t, syncRequests, 2)
}

func TestEventRedactionCoversThrottleAndEvidenceDisplayRefs(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/projects" {
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
			return
		}
		http.NotFound(writer, request)
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	output := EventsOutput{Events: []StreamEvent{
		{Type: "close.throttled", ProjectName: "spoke-project", Payload: map[string]any{
			"reason": "sibling-burst",
			"parent": "other-project#zz9",
			"prior":  "hub-project#abc4",
			"cohort": []any{"abc1", "other-project#def2", "01HDDDDDDDDDDDDDDDDDDDDDDD"},
		}},
		{Type: "issue.closed", ProjectName: "spoke-project", Payload: map[string]any{
			"reason": "duplicate",
			"evidence": []any{
				map[string]any{"type": "duplicate-of", "issue_ref": "other-project#ghi3"},
				map[string]any{"type": "duplicate-of", "issue_ref": "def2"},
				map[string]any{"type": "commit", "sha": "abc1234"},
			},
		}},
	}}

	require.NoError(t, handlers.redactEventsOutsideScope(t.Context(), &output))
	throttled := output.Events[0].Payload.(map[string]any)
	require.NotContains(t, throttled, "parent", "foreign qualified throttle parent must be blanked")
	require.NotContains(t, throttled, "prior")
	require.Equal(t, []any{"abc1"}, throttled["cohort"], "bare same-project cohort refs survive; foreign and full-length refs do not")
	closed := output.Events[1].Payload.(map[string]any)
	evidence := closed["evidence"].([]any)
	require.NotContains(t, evidence[0].(map[string]any), "issue_ref", "foreign qualified evidence ref must be blanked")
	require.Equal(t, "def2", evidence[1].(map[string]any)["issue_ref"], "bare same-project evidence ref survives")
	require.Equal(t, "abc1234", evidence[2].(map[string]any)["sha"], "non-ref evidence is untouched")
}

func TestEditFiltersLinkChangePeersOutsideScope(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case request.URL.Path == "/api/v1/issues/01HCCCCCCCCCCCCCCCCCCCCCCC":
			issue := issueJSON(1, "spoke-project", "def2")
			issue["uid"] = "01HCCCCCCCCCCCCCCCCCCCCCCC"
			writeJSON(writer, map[string]any{
				"issue": issue, "labels": []any{}, "comments": []any{}, "links": []any{},
			})
		case request.URL.Path == "/api/v1/projects/1/issues/def2":
			issue := issueJSON(1, "spoke-project", "def2")
			issue["uid"] = "01HCCCCCCCCCCCCCCCCCCCCCCC"
			writeJSON(writer, map[string]any{
				"issue": issue, "labels": []any{}, "comments": []any{}, "links": []any{},
			})
		case request.Method == http.MethodPatch:
			issue := issueJSON(1, "spoke-project", "abc1")
			writeJSON(writer, map[string]any{
				"changed": true, "issue": issue,
				"event": map[string]any{
					"event_id": 7, "event_uid": "01HEEEEEEEEEEEEEEEEEEEEEE7", "type": "issue.links_changed",
					"actor": "example-agent", "content_hash": "hash", "created_at": "2026-08-12T00:00:00Z",
					"origin_instance_uid": "01HFFFFFFFFFFFFFFFFFFFFFFF", "payload": "{}",
					"project_id": 1, "project_name": "spoke-project", "project_uid": "01HAAAAAAAAAAAAAAAAAAAAAAA",
				},
				"changes": map[string]any{
					"parent_removed": map[string]any{
						"project": "other-project", "qualified_id": "other-project#zz9",
						"short_id": "zz9", "status": "open", "uid": "01HDDDDDDDDDDDDDDDDDDDDDDD",
					},
					"parent_set": map[string]any{
						"project": "spoke-project", "qualified_id": "spoke-project#def2",
						"short_id": "def2", "status": "open", "uid": "01HCCCCCCCCCCCCCCCCCCCCCCC",
					},
					"related_added": []any{map[string]any{
						"project": "other-project", "qualified_id": "other-project#yy8",
						"short_id": "yy8", "status": "open", "uid": "01HGGGGGGGGGGGGGGGGGGGGGGG",
					}},
				},
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	parent := "spoke-project#def2"
	_, output, err := handlers.edit(t.Context(), nil, EditInput{Ref: "spoke-project#abc1", Parent: &parent})
	require.NoError(t, err)
	require.NotNil(t, output.Changes)
	require.Nil(t, output.Changes.ParentRemoved, "removed foreign parent must not leak")
	require.NotNil(t, output.Changes.ParentSet)
	require.Equal(t, "spoke-project#def2", output.Changes.ParentSet.QualifiedRef)
	require.Empty(t, output.Changes.RelatedAdded)
}

func TestEditFiltersArchivedLinkChangePeerOutsideActiveScope(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			projects := []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}
			if request.URL.Query().Get("include") == "archived" {
				archived := projectJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "archived-project")
				archived["deleted_at"] = "2026-08-12T00:00:00Z"
				projects = append(projects, archived)
			}
			writeJSON(writer, map[string]any{"projects": projects})
		case request.URL.Path == "/api/v1/projects/1/issues/def2":
			issue := issueJSON(1, "spoke-project", "def2")
			issue["uid"] = "01HCCCCCCCCCCCCCCCCCCCCCCC"
			writeJSON(writer, map[string]any{
				"issue": issue, "labels": []any{}, "comments": []any{}, "links": []any{},
			})
		case request.Method == http.MethodPatch:
			writeJSON(writer, map[string]any{
				"changed": true, "issue": issueJSON(1, "spoke-project", "abc1"),
				"event": map[string]any{
					"event_id": 7, "event_uid": "01HEEEEEEEEEEEEEEEEEEEEEE7", "type": "issue.links_changed",
					"actor": "example-agent", "content_hash": "hash", "created_at": "2026-08-12T00:00:00Z",
					"origin_instance_uid": "01HFFFFFFFFFFFFFFFFFFFFFFF", "payload": "{}",
					"project_id": 1, "project_name": "spoke-project", "project_uid": "01HAAAAAAAAAAAAAAAAAAAAAAA",
				},
				"changes": map[string]any{
					"parent_removed": map[string]any{
						"project": "archived-project", "qualified_id": "archived-project#old1",
						"short_id": "old1", "status": "closed", "uid": "01HCCCCCCCCCCCCCCCCCCCCCCC",
					},
				},
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{
		{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"},
		{ID: 2, UID: "01HBBBBBBBBBBBBBBBBBBBBBBB", Name: "archived-project"},
	})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	parent := "spoke-project#def2"
	_, output, err := handlers.edit(t.Context(), nil, EditInput{Ref: "spoke-project#abc1", Parent: &parent})
	require.NoError(t, err)
	require.NotNil(t, output.Changes)
	require.Nil(t, output.Changes.ParentRemoved, "archived relationship peer must not leak through edit changes")
}

func TestEditReportsCommittedMutationWhenLinkChangeScopingFails(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case request.URL.Path == "/api/v1/projects/1/issues/def2":
			issue := issueJSON(1, "spoke-project", "def2")
			issue["uid"] = "01HCCCCCCCCCCCCCCCCCCCCCCC"
			writeJSON(writer, map[string]any{
				"issue": issue, "labels": []any{}, "comments": []any{}, "links": []any{},
			})
		case request.Method == http.MethodPatch:
			writeJSON(writer, map[string]any{
				"changed": true, "issue": issueJSON(1, "spoke-project", "abc1"),
				"event": map[string]any{
					"event_id": 7, "event_uid": "01HEEEEEEEEEEEEEEEEEEEEEE7", "type": "issue.links_changed",
					"actor": "example-agent", "content_hash": "hash", "created_at": "2026-08-12T00:00:00Z",
					"origin_instance_uid": "01HFFFFFFFFFFFFFFFFFFFFFFF", "payload": "{}",
					"project_id": 1, "project_name": "spoke-project", "project_uid": "01HAAAAAAAAAAAAAAAAAAAAAAA",
				},
				"changes": map[string]any{
					"related_added": []any{map[string]any{
						"project": "spoke-project", "qualified_id": "spoke-project#def2",
						"short_id": "def2", "status": "open", "uid": "01HCCCCCCCCCCCCCCCCCCCCCCC",
					}},
				},
			})
		case strings.HasPrefix(request.URL.Path, "/api/v1/issues/"):
			writer.WriteHeader(http.StatusInternalServerError)
			writeJSON(writer, map[string]any{
				"status": http.StatusInternalServerError,
				"error":  map[string]any{"code": "internal_error", "message": "try again"},
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	parent := "spoke-project#def2"
	_, output, err := handlers.edit(t.Context(), nil, EditInput{Ref: "spoke-project#abc1", Parent: &parent})
	require.NoError(t, err)
	require.True(t, output.Changed)
	require.Equal(t, "spoke-project#abc1", output.Issue.QualifiedRef)
	require.Nil(t, output.Changes, "unresolved supplemental changes must be omitted after commit")
}

func TestEditRelationshipTargetSurvivesScopedProjectNameReuse(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	sourceProject, err := store.CreateProject(t.Context(), "source-project")
	require.NoError(t, err)
	targetProject, err := store.CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	foreignProject, err := store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	source, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: sourceProject.ID, Title: "Source issue", Author: "example-agent",
	})
	require.NoError(t, err)
	target, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: targetProject.ID, UID: "01ARZ3NDEKTSV4RRFFQ69GABC1", ShortIDOverride: "abc1",
		Title: "Scoped target", Author: "example-agent",
	})
	require.NoError(t, err)
	foreign, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: foreignProject.ID, UID: "01BRZ3NDEKTSV4RRFFQ69GABC1", ShortIDOverride: "abc1",
		Title: "Foreign target", Author: "example-agent",
	})
	require.NoError(t, err)

	daemonServer := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC()})
	t.Cleanup(func() { require.NoError(t, daemonServer.Close()) })
	var namesReused atomic.Bool
	daemonHandler := daemonServer.Handler()
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPatch && namesReused.CompareAndSwap(false, true) {
			if _, renameErr := store.RenameProject(request.Context(), targetProject.ID, "renamed-target"); renameErr != nil {
				http.Error(writer, renameErr.Error(), http.StatusInternalServerError)
				return
			}
			if _, renameErr := store.RenameProject(request.Context(), foreignProject.ID, "target-project"); renameErr != nil {
				http.Error(writer, renameErr.Error(), http.StatusInternalServerError)
				return
			}
		}
		daemonHandler.ServeHTTP(writer, request)
	}))
	t.Cleanup(httpServer.Close)
	client, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	scope, err := NewAllowlistScope([]ProjectIdentity{
		{ID: sourceProject.ID, UID: sourceProject.UID, Name: sourceProject.Name},
		{ID: targetProject.ID, UID: targetProject.UID, Name: targetProject.Name},
	})
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{
		Client: client, Scope: scope, Actor: "example-agent", Version: "test-version",
	})

	callWorkflowTool(t, session, "kata.edit", map[string]any{
		"ref": sourceProject.Name + "#" + source.ShortID,
		"add_blocks": []any{
			targetProject.Name + "#" + target.ShortID,
		},
	})
	require.True(t, namesReused.Load())
	links, err := store.LinksByIssue(t.Context(), source.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, target.ID, links[0].ToIssueID)
	require.NotEqual(t, foreign.ID, links[0].ToIssueID)
}

func TestEventRedactionPreservesOpaqueMetadataAndDiffValues(t *testing.T) {
	var issueLookups int
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/projects" {
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
			return
		}
		issueLookups++
		http.NotFound(writer, request)
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	diff := map[string]any{
		"from_uid":   map[string]any{"from": nil, "to": "user-value"},
		"source_uid": map[string]any{"from": nil, "to": "build-123"},
	}
	metadata := map[string]any{"source_uid": "build-123", "to_short_id": "deploy-7"}
	output := EventsOutput{Events: []StreamEvent{
		{Type: "issue.metadata_updated", Payload: map[string]any{"diff": diff, "revision_new": float64(2)}},
		{Type: "issue.created", Payload: map[string]any{"short_id": "abc1", "metadata": metadata}},
	}}

	require.NoError(t, handlers.redactEventsOutsideScope(t.Context(), &output))
	updated := output.Events[0].Payload.(map[string]any)
	require.Equal(t, diff, updated["diff"], "user metadata diff keys and values are opaque data")
	created := output.Events[1].Payload.(map[string]any)
	require.Equal(t, metadata, created["metadata"], "user metadata keys named like relationship fields survive")
	require.Zero(t, issueLookups, "opaque values must not trigger peer resolution")
}

func TestEventRedactionScopesMovedProjectReferences(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/projects" {
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
			return
		}
		http.NotFound(writer, request)
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	output := EventsOutput{Events: []StreamEvent{{
		ProjectUID: "01HAAAAAAAAAAAAAAAAAAAAAAA",
		Payload: map[string]any{
			"from_project_uid": "01HEEEEEEEEEEEEEEEEEEEEEEE", "from_short_id": "old1",
			"to_project_uid": "01HAAAAAAAAAAAAAAAAAAAAAAA", "to_short_id": "new1",
			"source_uid": "01HEEEEEEEEEEEEEEEEEEEEEEE",
		},
	}}}

	require.NoError(t, handlers.redactEventsOutsideScope(t.Context(), &output))
	payload := output.Events[0].Payload.(map[string]any)
	require.NotContains(t, payload, "from_project_uid")
	require.NotContains(t, payload, "from_short_id")
	require.NotContains(t, payload, "source_uid")
	require.Equal(t, "01HAAAAAAAAAAAAAAAAAAAAAAA", payload["to_project_uid"])
	require.Equal(t, "new1", payload["to_short_id"])
}

func TestAuditClosesRedactsForeignAndUnresolvedParents(t *testing.T) {
	var auditRequests int
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case "/api/v1/audit/closes":
			auditRequests++
			writeJSON(writer, map[string]any{"rows": []any{
				map[string]any{"time": "2026-08-12T00:00:00Z", "actor": "example-agent", "reason": "done", "issue": "abc1", "parent": "other-project#zz9", "event_id": 11},
				map[string]any{"time": "2026-08-12T00:00:00Z", "actor": "example-agent", "reason": "done", "issue": "def2", "parent": "ghi3", "parent_uid": "01HEEEEEEEEEEEEEEEEEEEEEEE", "event_id": 12},
				map[string]any{"time": "2026-08-12T00:00:00Z", "actor": "example-agent", "reason": "done", "issue": "jkl4", "parent": "ghi3", "parent_uid": "01HDDDDDDDDDDDDDDDDDDDDDDD", "event_id": 13},
			}})
		case "/api/v1/issues/01HEEEEEEEEEEEEEEEEEEEEEEE":
			issue := issueJSON(1, "spoke-project", "ghi3")
			issue["uid"] = "01HEEEEEEEEEEEEEEEEEEEEEEE"
			writeJSON(writer, map[string]any{
				"issue": issue, "labels": []any{}, "comments": []any{}, "links": []any{},
			})
		case "/api/v1/projects/1/issues/def2":
			writeJSON(writer, map[string]any{
				"issue": issueJSON(1, "spoke-project", "def2"), "labels": []any{}, "comments": []any{}, "links": []any{},
			})
		default:
			// The purged frozen parent UID resolves nowhere.
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, output, err := handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project"})
	require.NoError(t, err)
	require.Len(t, output.Rows, 3)
	require.Nil(t, output.Rows[0].Parent, "foreign qualified parent without frozen identity must be blanked")
	require.NotNil(t, output.Rows[1].Parent, "parent whose frozen UID resolves in scope survives")
	require.Equal(t, "ghi3", *output.Rows[1].Parent)
	require.Nil(t, output.Rows[2].Parent,
		"a purged frozen parent must be blanked even when its display short ID collides with an in-scope parent")
	require.Nil(t, output.Rows[2].ParentUID)

	// A bounded limit truncates before the result can exceed the transport
	// message cap; the event-ID cursor pages without skipping or repeating
	// rows even when every row shares one timestamp.
	_, limited, err := handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Limit: 2})
	require.NoError(t, err)
	require.Len(t, limited.Rows, 2)
	require.True(t, limited.Truncated)
	require.NotNil(t, limited.NextCursor)
	_, page, err := handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Limit: 2, Cursor: *limited.NextCursor})
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)
	require.False(t, page.Truncated)
	require.Nil(t, page.NextCursor)
	require.Equal(t, "jkl4", page.Rows[0].Issue)
	_, _, err = handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Limit: 101})
	require.ErrorContains(t, err, "limit must be between 1 and 100")
	_, _, err = handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Cursor: "not-a-cursor"})
	require.ErrorContains(t, err, "cursor is not a value returned in next_cursor")

	// Foreign or unprovable parent filters are rejected before any request.
	before := auditRequests
	_, _, err = handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Parent: "other-project#abc9"})
	require.ErrorContains(t, err, "outside the MCP startup scope")
	_, _, err = handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Parent: "01HDDDDDDDDDDDDDDDDDDDDDDD"})
	require.ErrorContains(t, err, "unscoped UID")
	_, _, err = handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Parent: "fed9"})
	require.ErrorContains(t, err, "does not resolve in project", "unresolved bare filters must not probe stored parent snapshots")
	_, _, err = handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Parent: "spoke-project#fed9"})
	require.ErrorContains(t, err, "does not resolve in project", "unresolved qualified filters must not probe stored parent snapshots")
	require.Equal(t, before, auditRequests, "rejected parent filters must not reach the daemon")

	// Bare and qualified filters that resolve in the nominated project are forwarded.
	_, _, err = handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Parent: "def2"})
	require.NoError(t, err)
	_, _, err = handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Parent: "spoke-project#def2"})
	require.NoError(t, err)
	require.Equal(t, before+2, auditRequests)
}

func TestEventPeerRedactionDropsShortIDsWithoutUIDs(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/projects" {
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
			return
		}
		http.NotFound(writer, request)
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	output := EventsOutput{Events: []StreamEvent{{
		RelatedIssueShortID: "out1",
		Payload: map[string]any{
			"from_short_id": "from1", "to_short_id": "to1",
			"peer_short_id": "peer1", "related_issue_short_id": "related1",
		},
	}}}

	require.NoError(t, handlers.redactEventsOutsideScope(t.Context(), &output))
	require.Empty(t, output.Events[0].RelatedIssueShortID)
	payload := output.Events[0].Payload.(map[string]any)
	require.NotContains(t, payload, "from_short_id")
	require.NotContains(t, payload, "to_short_id")
	require.NotContains(t, payload, "peer_short_id")
	require.NotContains(t, payload, "related_issue_short_id")
}

func TestShowFiltersLinksOutsideFixedProjectScope(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
				projectJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "other-project"),
			}})
		case "/api/v1/projects/1/issues/spk1":
			issue := issueJSON(1, "spoke-project", "spk1")
			peer := func(project, ref, uid string) map[string]any {
				return map[string]any{
					"project": project, "qualified_id": project + "#" + ref,
					"short_id": ref, "status": "open", "uid": uid,
				}
			}
			link := func(id int, to map[string]any) map[string]any {
				return map[string]any{
					"id": id, "type": "related", "author": "example-agent",
					"created_at": "2026-08-11T00:00:00Z",
					"from":       peer("spoke-project", "spk1", issue["uid"].(string)), "to": to,
				}
			}
			writeJSON(writer, map[string]any{
				"issue": issue, "labels": []any{}, "comments": []any{},
				"links": []any{
					link(1, peer("spoke-project", "in1", "01HCCCCCCCCCCCCCCCCCCCCCCC")),
					link(2, peer("other-project", "out1", "01HDDDDDDDDDDDDDDDDDDDDDDD")),
				},
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, output, err := handlers.show(t.Context(), nil, ShowInput{Ref: "spoke-project#spk1"})
	require.NoError(t, err)
	require.Equal(t, []LinkSummary{{Type: "related", QualifiedRef: "spoke-project#in1", Status: "open"}}, output.Issue.Links)
}

func TestTokenAdministrationRequiresDaemonWideScope(t *testing.T) {
	var requests atomic.Int64
	client := reviewClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(writer, "unexpected request", http.StatusInternalServerError)
	})
	scope, err := NewBoundScope(ProjectIdentity{ID: 1, Name: "spoke-project"})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "example-agent", EnableTokenAdmin: true}}

	_, _, err = handlers.tokens(t.Context(), nil, TokensInput{})
	require.ErrorContains(t, err, "daemon-wide scope")
	_, _, err = handlers.tokenCreate(t.Context(), nil, TokenCreateInput{TokenActor: "example-agent"})
	require.ErrorContains(t, err, "daemon-wide scope")
	_, _, err = handlers.tokenRevoke(t.Context(), nil, TokenRevokeInput{ID: 1})
	require.ErrorContains(t, err, "daemon-wide scope")
	require.Zero(t, requests.Load())
}

func TestTokenAdministrationRequiresExplicitCapability(t *testing.T) {
	var requests atomic.Int64
	client := reviewClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(writer, "unexpected request", http.StatusInternalServerError)
	})
	handlers := toolHandlers{options: Options{
		Client: client, Scope: NewAllScope(), Actor: "example-agent",
	}}

	_, _, err := handlers.tokens(t.Context(), nil, TokensInput{})
	require.ErrorContains(t, err, "explicit startup capability")
	_, _, err = handlers.tokenCreate(t.Context(), nil, TokenCreateInput{TokenActor: "example-agent"})
	require.ErrorContains(t, err, "explicit startup capability")
	_, _, err = handlers.tokenRevoke(t.Context(), nil, TokenRevokeInput{ID: 1})
	require.ErrorContains(t, err, "explicit startup capability")
	require.Zero(t, requests.Load())
}

func TestFederationLeaveRequiresPhaseAndCommitConfirmation(t *testing.T) {
	var requests atomic.Int64
	client := reviewClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(writer, "unexpected request", http.StatusInternalServerError)
	})
	scope, err := NewBoundScope(ProjectIdentity{ID: 1, Name: "spoke-project"})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "example-agent"}}

	_, _, err = handlers.federationLeave(t.Context(), nil, FederationLeaveInput{Project: "spoke-project"})
	require.ErrorContains(t, err, "phase is required")
	_, _, err = handlers.federationLeave(t.Context(), nil, FederationLeaveInput{Project: "spoke-project", Phase: "commit"})
	require.ErrorContains(t, err, `confirm must equal "COMMIT FEDERATION LEAVE spoke-project"`)
	require.Zero(t, requests.Load())
}

func TestScopedFederationLeaveCannotArchive(t *testing.T) {
	var mutations int
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case "/api/v1/federation/replicas/1/actions/leave":
			mutations++
			writeJSON(writer, map[string]any{
				"project":     projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
				"disposition": "detach", "detached": false,
			})
		default:
			http.NotFound(writer, request)
		}
	})
	bound, err := NewBoundScope(ProjectIdentity{ID: 1, Name: "spoke-project"})
	require.NoError(t, err)
	allowlist, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)

	for name, scope := range map[string]*Scope{"bound": bound, "allowlist": allowlist} {
		handlers := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "example-agent"}}
		before := mutations
		for _, phase := range []string{"preflight", "prepare", "commit"} {
			_, _, err := handlers.federationLeave(t.Context(), nil, FederationLeaveInput{
				Project: "spoke-project", Phase: phase, Disposition: "archive",
				Confirm: "COMMIT FEDERATION LEAVE spoke-project",
			})
			require.ErrorContains(t, err, "requires the --all-projects daemon-wide scope", "%s %s", name, phase)
		}
		require.Equal(t, before, mutations, "scoped archive leave must fail before any daemon mutation")

		_, _, err := handlers.federationLeave(t.Context(), nil, FederationLeaveInput{
			Project: "spoke-project", Phase: "preflight", Disposition: "detach",
		})
		require.NoError(t, err, "%s scope keeps the detach disposition", name)
		require.Equal(t, before+1, mutations)

		_, _, err = handlers.federationRebind(t.Context(), nil, FederationRebindInput{
			Project: "spoke-project", HubCatalog: "other-hub",
		})
		require.ErrorContains(t, err, "requires the --all-projects daemon-wide scope",
			"%s scope must not route the enrollment token to a caller-selected catalog", name)
		require.Equal(t, before+1, mutations)
	}
}

func TestFederationLeaveResolvesArchivedProjectForRetry(t *testing.T) {
	var sawArchivedCatalog atomic.Bool
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			sawArchivedCatalog.Store(request.URL.Query().Get("include") == "archived")
			project := projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")
			project["deleted_at"] = "2026-08-11T00:00:00Z"
			writeJSON(writer, map[string]any{"projects": []any{project}})
		case "/api/v1/federation/replicas/1/actions/leave":
			writeJSON(writer, map[string]any{
				"project":     projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
				"disposition": "archive", "detached": true, "archived": true,
			})
		default:
			http.NotFound(writer, request)
		}
	})
	handlers := toolHandlers{options: Options{
		Client: client, Scope: NewAllScope(), Actor: "example-agent",
	}}

	_, output, err := handlers.federationLeave(t.Context(), nil, FederationLeaveInput{
		Project: "spoke-project", Phase: "commit", Disposition: "archive",
		Confirm: "COMMIT FEDERATION LEAVE spoke-project",
	})
	require.NoError(t, err)
	require.True(t, sawArchivedCatalog.Load())
	require.True(t, output.Detached)
}

func TestAllowlistListAndReadyApplyLimitAfterScopedFanout(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(map[bool]string{false: "list", true: "ready"}[ready], func(t *testing.T) {
			var globalRequests atomic.Int64
			client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api/v1/projects":
					writeJSON(writer, map[string]any{"projects": []any{
						projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
						projectJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "hub-project"),
					}})
				case "/api/v1/projects/1/issues", "/api/v1/projects/1/ready":
					issue := issueJSON(1, "spoke-project", "spk1")
					issue["updated_at"] = "2026-08-11T00:00:00Z"
					writeJSON(writer, map[string]any{"issues": []any{issue}})
				case "/api/v1/projects/2/issues", "/api/v1/projects/2/ready":
					issue := issueJSON(2, "hub-project", "hub1")
					issue["project_uid"] = "01HBBBBBBBBBBBBBBBBBBBBBBB"
					issue["updated_at"] = "2026-08-11T00:00:00.1Z"
					writeJSON(writer, map[string]any{"issues": []any{issue}})
				default:
					globalRequests.Add(1)
					http.Error(writer, "unexpected global request", http.StatusInternalServerError)
				}
			})
			scope, err := NewAllowlistScope([]ProjectIdentity{
				{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"},
				{ID: 2, UID: "01HBBBBBBBBBBBBBBBBBBBBBBB", Name: "hub-project"},
			})
			require.NoError(t, err)
			handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

			var output IssueListOutput
			if ready {
				_, output, err = handlers.ready(t.Context(), nil, ReadyInput{Limit: 1})
			} else {
				_, output, err = handlers.list(t.Context(), nil, ListInput{Limit: 1})
			}
			require.NoError(t, err)
			require.True(t, output.Truncated)
			require.Equal(t, "hub-project#hub1", output.Issues[0].QualifiedRef)
			require.Zero(t, globalRequests.Load())
		})
	}
}

func TestNextExaminesAllReadyIssues(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
		case "/api/v1/ready":
			count := 101
			if request.URL.Query().Has("limit") {
				count = 100
			}
			issues := make([]any, 0, count)
			for index := 0; index < count; index++ {
				issue := globalIssueJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project", fmt.Sprintf("i%03d", index))
				issue["uid"] = fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G%04d", index)
				issue["priority"] = 4
				if index == 100 {
					issue["priority"] = 0
				}
				issues = append(issues, issue)
			}
			writeJSON(writer, map[string]any{"issues": issues})
		default:
			http.NotFound(writer, request)
		}
	})
	handlers := toolHandlers{options: Options{Client: client, Scope: NewAllScope()}}

	_, output, err := handlers.next(t.Context(), nil, NextInput{})
	require.NoError(t, err)
	require.NotNil(t, output.Issue)
	require.Equal(t, "spoke-project#i100", output.Issue.QualifiedRef)
}

func TestAllowlistNextPreservesMergedReadyOrderingForEqualPriority(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
				projectJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "hub-project"),
			}})
		case "/api/v1/projects/1/ready":
			issue := issueJSON(1, "spoke-project", "spk1")
			issue["priority"] = 1
			issue["updated_at"] = "2026-08-11T00:00:00.1Z"
			writeJSON(writer, map[string]any{"issues": []any{issue}})
		case "/api/v1/projects/2/ready":
			issue := issueJSON(2, "hub-project", "hub1")
			issue["project_uid"] = "01HBBBBBBBBBBBBBBBBBBBBBBB"
			issue["priority"] = 1
			issue["updated_at"] = "2026-08-11T00:00:00Z"
			writeJSON(writer, map[string]any{"issues": []any{issue}})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{
		{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"},
		{ID: 2, UID: "01HBBBBBBBBBBBBBBBBBBBBBBB", Name: "hub-project"},
	})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, output, err := handlers.next(t.Context(), nil, NextInput{})
	require.NoError(t, err)
	require.NotNil(t, output.Issue)
	require.Equal(t, "spoke-project#spk1", output.Issue.QualifiedRef)
}

func TestGraphFiltersNodesEdgesAndUnresolvedReferencesToScope(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
				projectJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "hub-project"),
			}})
		case "/api/v1/projects/1/issues/spk1/graph":
			allowed := issueJSON(1, "spoke-project", "spk1")
			allowed["uid"] = "allowed-uid"
			denied := issueJSON(2, "hub-project", "hub1")
			denied["uid"] = "denied-uid"
			writeJSON(writer, map[string]any{
				"source_uid": "allowed-uid", "depth": "full", "hide_done": false,
				"nodes":           []any{allowed, denied},
				"edges":           []any{map[string]any{"from_uid": "allowed-uid", "to_uid": "denied-uid", "kind": "blocks", "layout": true}},
				"unresolved_refs": []any{map[string]any{"uid": "allowed-uid", "other_uid": "denied-uid", "kind": "blocks", "side": "to"}},
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, output, err := handlers.graph(t.Context(), nil, GraphInput{Ref: "spoke-project#spk1"})
	require.NoError(t, err)
	require.Len(t, output.Nodes, 1)
	require.Equal(t, "allowed-uid", output.Nodes[0].UID)
	require.Empty(t, output.Edges)
	require.Empty(t, output.UnresolvedRefs)
}

func TestEditRejectsMixedIssueAndMetadataChangesBeforeMutation(t *testing.T) {
	var requests atomic.Int64
	client := reviewClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(writer, "unexpected request", http.StatusInternalServerError)
	})
	scope, err := NewBoundScope(ProjectIdentity{ID: 1, Name: "spoke-project"})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	title := "Changed"
	scheduledOn := "2026-09-01"

	_, _, err = handlers.edit(t.Context(), nil, EditInput{Ref: "spk1", Title: &title, ScheduledOn: &scheduledOn})
	require.ErrorContains(t, err, "separate calls")
	require.Zero(t, requests.Load())
}

func TestEditMissingRelationshipRemovalRemainsIdempotent(t *testing.T) {
	var patchRequests atomic.Int64
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/projects/1/issues/abc1":
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusNotFound)
			writeJSON(writer, map[string]any{"status": 404, "error": map[string]any{"code": "issue_not_found", "message": "not found"}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/projects/1/issues/src1":
			writeJSON(writer, map[string]any{
				"issue": issueJSON(1, "spoke-project", "src1"), "labels": []any{}, "comments": []any{}, "links": []any{},
			})
		case request.Method == http.MethodPatch:
			patchRequests.Add(1)
			http.Error(writer, "unexpected mutation", http.StatusInternalServerError)
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewBoundScope(ProjectIdentity{
		ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, output, err := handlers.edit(t.Context(), nil, EditInput{Ref: "src1", RemoveBlocks: []string{"abc1"}})
	require.NoError(t, err)
	require.False(t, output.Changed)
	require.Equal(t, "src1", output.Issue.Ref)
	require.Zero(t, patchRequests.Load(), "a missing idempotent removal must not be forwarded as a mutable reference")
}

func TestProjectsListsArchivedRecordsWithoutShowRequest(t *testing.T) {
	var showRequests atomic.Int64
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/projects" {
			showRequests.Add(1)
			http.Error(writer, "archived project cannot be shown", http.StatusNotFound)
			return
		}
		archived := projectJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "archived-project")
		archived["deleted_at"] = "2026-08-11T00:00:00Z"
		writeJSON(writer, map[string]any{"projects": []any{
			projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"), archived,
		}})
	})
	handlers := toolHandlers{options: Options{Client: client, Scope: NewAllScope()}}

	_, output, err := handlers.projects(t.Context(), nil, ProjectsInput{IncludeArchived: true})
	require.NoError(t, err)
	require.Len(t, output.Projects, 2)
	require.True(t, output.Projects[0].Archived || output.Projects[1].Archived)
	require.Zero(t, showRequests.Load())
}

func TestFixedScopeCannotSeeOrRevokeWildcardFederationEnrollment(t *testing.T) {
	var revokeRequests atomic.Int64
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/federation/status":
			writeJSON(writer, map[string]any{"statuses": []any{}})
		case request.URL.Path == "/api/v1/federation/enrollments" && request.Method == http.MethodGet:
			writeJSON(writer, map[string]any{"enrollments": []any{map[string]any{
				"id": 9, "project_id": 0, "spoke_instance_uid": "01HCCCCCCCCCCCCCCCCCCCCCCC",
				"capabilities": "pull", "actor": "example-agent",
				"created_at": "2026-08-11T00:00:00Z", "updated_at": "2026-08-11T00:00:00Z",
			}}})
		case strings.Contains(request.URL.Path, "/federation/enrollments/"):
			revokeRequests.Add(1)
			writeJSON(writer, map[string]any{"id": 9, "revoked": true})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewBoundScope(ProjectIdentity{ID: 1, Name: "spoke-project"})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, status, err := handlers.federationStatus(t.Context(), nil, FederationStatusInput{})
	require.NoError(t, err)
	require.Empty(t, status.Enrollments)
	_, _, err = handlers.federationEnrollmentRevoke(t.Context(), nil, FederationEnrollmentRevokeInput{ID: 9})
	require.ErrorContains(t, err, "outside the MCP startup scope")
	require.Zero(t, revokeRequests.Load())
}

func TestRecurrencePatchRequiresPositiveRevisionBeforeRequest(t *testing.T) {
	var requests atomic.Int64
	client := reviewClient(t, func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(writer, "unexpected request", http.StatusInternalServerError)
	})
	scope, err := NewBoundScope(ProjectIdentity{ID: 1, Name: "spoke-project"})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	_, _, err = handlers.recurrenceUpdate(t.Context(), nil, RecurrenceUpdateInput{Action: "patch", UID: "recurrence-uid"})
	require.ErrorContains(t, err, "positive revision")
	require.Zero(t, requests.Load())
}

func TestAllowlistEventResetAdvancesToResetCursor(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
		case "/api/v1/projects/1/events":
			writeJSON(writer, map[string]any{
				"events": []any{}, "next_after_id": 42, "reset_required": true, "reset_after_id": 42,
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}

	output, err := handlers.pollScopedEvents(t.Context(), "", 7, 20)
	require.NoError(t, err)
	require.True(t, output.ResetRequired)
	require.EqualValues(t, 42, output.NextAfterID)
	require.NotNil(t, output.ResetAfterID)
	require.EqualValues(t, 42, *output.ResetAfterID)
}

func reviewActivityEventJSON(eventID, projectID int64, projectUID, projectName string) map[string]any {
	return map[string]any{
		"actor": "example-agent", "content_hash": "hash", "created_at": "2026-08-12T00:00:00Z",
		"event_id": eventID, "event_uid": fmt.Sprintf("event-%d", eventID), "hlc_counter": 0,
		"hlc_physical_ms": 1, "origin_instance_uid": "01HCCCCCCCCCCCCCCCCCCCCCCC",
		"payload": map[string]any{"body": projectName + " body"}, "project_id": projectID,
		"project_name": projectName, "project_uid": projectUID, "type": "issue.edited",
	}
}

func reviewDigestBodyJSON(projectID int64, projectName string) map[string]any {
	return map[string]any{
		"project_id": projectID, "event_count": 1,
		"since": "2026-08-11T00:00:00Z", "until": "2026-08-12T00:00:00Z",
		"totals": map[string]any{},
		"actors": []any{map[string]any{
			"actor": "example-agent", "totals": map[string]any{},
			"issues": []any{map[string]any{
				"issue_uid": "01HDDDDDDDDDDDDDDDDDDDDDDD", "issue_short_id": "abc1",
				"project_id": projectID, "project_name": projectName, "actions": []any{"edited"},
			}},
		}},
	}
}

func reviewClient(t *testing.T, handler http.HandlerFunc) *kataclient.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	return client
}
