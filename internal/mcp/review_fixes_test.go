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

func TestAuditClosesRedactsForeignQualifiedParents(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case "/api/v1/audit/closes":
			writeJSON(writer, map[string]any{"rows": []any{
				map[string]any{"time": "2026-08-12T00:00:00Z", "actor": "example-agent", "reason": "done", "issue": "abc1", "parent": "other-project#zz9"},
				map[string]any{"time": "2026-08-12T00:00:00Z", "actor": "example-agent", "reason": "done", "issue": "def2", "parent": "ghi3"},
			}})
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
	require.Len(t, output.Rows, 2)
	require.Nil(t, output.Rows[0].Parent)
	require.NotNil(t, output.Rows[1].Parent)
	require.Equal(t, "ghi3", *output.Rows[1].Parent)
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
