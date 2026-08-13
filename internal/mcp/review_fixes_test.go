package mcpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
		case "/api/v1/projects/1/issues/abc1":
			issue := issueJSON(1, "spoke-project", "abc1")
			issue["uid"] = "01HCCCCCCCCCCCCCCCCCCCCCCC"
			writeJSON(writer, map[string]any{
				"issue": issue, "labels": []any{}, "comments": []any{},
				"links": []any{map[string]any{
					"id": 1, "type": "blocks", "author": "example-agent", "created_at": "2026-08-11T00:00:00Z",
					"from": linkPeerJSON("spoke-project", "abc1", "01HCCCCCCCCCCCCCCCCCCCCCCC"),
					"to":   linkPeerJSON("spoke-project", "def2", "01HEEEEEEEEEEEEEEEEEEEEEEE"),
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
	require.Equal(t, []string{"created", "labeled:bug", "blocks:def2", "unparent", "related", "commented:2"}, actions,
		"link-vouched peer survives; unvouched targets are stripped to their action type even when a same-short-ID issue exists")
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

func linkPeerJSON(project, shortID, uid string) map[string]any {
	return map[string]any{
		"project": project, "qualified_id": project + "#" + shortID,
		"short_id": shortID, "status": "open", "uid": uid,
	}
}

func TestEditFiltersLinkChangePeersOutsideScope(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
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
				map[string]any{"time": "2026-08-12T00:00:00Z", "actor": "example-agent", "reason": "done", "issue": "abc1", "parent": "other-project#zz9"},
				map[string]any{"time": "2026-08-12T00:00:00Z", "actor": "example-agent", "reason": "done", "issue": "def2", "parent": "ghi3"},
				map[string]any{"time": "2026-08-12T00:00:00Z", "actor": "example-agent", "reason": "done", "issue": "jkl4", "parent": "gone5"},
			}})
		case "/api/v1/projects/1/issues/def2":
			issue := issueJSON(1, "spoke-project", "def2")
			issue["uid"] = "01HCCCCCCCCCCCCCCCCCCCCCCC"
			writeJSON(writer, map[string]any{
				"issue": issue, "labels": []any{}, "comments": []any{},
				"links": []any{map[string]any{
					"id": 1, "type": "parent", "author": "example-agent", "created_at": "2026-08-11T00:00:00Z",
					"from": linkPeerJSON("spoke-project", "def2", "01HCCCCCCCCCCCCCCCCCCCCCCC"),
					"to":   linkPeerJSON("spoke-project", "ghi3", "01HEEEEEEEEEEEEEEEEEEEEEEE"),
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

	_, output, err := handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project"})
	require.NoError(t, err)
	require.Len(t, output.Rows, 3)
	require.Nil(t, output.Rows[0].Parent, "foreign qualified parent must be blanked")
	require.NotNil(t, output.Rows[1].Parent, "parent vouched by the child's current in-scope parent link survives")
	require.Equal(t, "ghi3", *output.Rows[1].Parent)
	require.Nil(t, output.Rows[2].Parent, "bare parent without link provenance must be blanked")

	// A bounded limit truncates before the result can exceed the transport
	// message cap; the offset cursor pages the stable row order without
	// skipping or repeating rows that share one timestamp.
	_, limited, err := handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Limit: 2})
	require.NoError(t, err)
	require.Len(t, limited.Rows, 2)
	require.True(t, limited.Truncated)
	require.NotNil(t, limited.NextOffset)
	require.EqualValues(t, 2, *limited.NextOffset)
	_, page, err := handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Limit: 2, Offset: *limited.NextOffset})
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)
	require.False(t, page.Truncated)
	require.Nil(t, page.NextOffset)
	require.Equal(t, "jkl4", page.Rows[0].Issue)
	_, _, err = handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Limit: 101})
	require.ErrorContains(t, err, "limit must be between 1 and 100")

	// Foreign or unprovable parent filters are rejected before any request.
	before := auditRequests
	_, _, err = handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Parent: "other-project#abc9"})
	require.ErrorContains(t, err, "outside the MCP startup scope")
	_, _, err = handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project", Parent: "01HDDDDDDDDDDDDDDDDDDDDDDD"})
	require.ErrorContains(t, err, "unscoped UID")
	require.Equal(t, before, auditRequests, "rejected parent filters must not reach the daemon")
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
					issue["updated_at"] = "2026-08-10T00:00:00Z"
					writeJSON(writer, map[string]any{"issues": []any{issue}})
				case "/api/v1/projects/2/issues", "/api/v1/projects/2/ready":
					issue := issueJSON(2, "hub-project", "hub1")
					issue["project_uid"] = "01HBBBBBBBBBBBBBBBBBBBBBBB"
					issue["updated_at"] = "2026-08-11T00:00:00Z"
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
			issue["updated_at"] = "2026-08-11T00:00:00Z"
			writeJSON(writer, map[string]any{"issues": []any{issue}})
		case "/api/v1/projects/2/ready":
			issue := issueJSON(2, "hub-project", "hub1")
			issue["project_uid"] = "01HBBBBBBBBBBBBBBBBBBBBBBB"
			issue["priority"] = 1
			issue["updated_at"] = "2026-08-10T00:00:00Z"
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

func reviewClient(t *testing.T, handler http.HandlerFunc) *kataclient.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	return client
}
