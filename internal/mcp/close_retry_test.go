package mcpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
	"hegel.dev/go/hegel"
)

func TestCloseForwardsRetryGuardsAndReturnsOriginalReceipt(t *testing.T) {
	var idempotencyKey, ifMatch, retryProtocol string
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			idempotencyKey = request.Header.Get("Idempotency-Key")
			ifMatch = request.Header.Get("If-Match")
			var body map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			retryProtocol, _ = body["retry_protocol"].(string)
			issue := issueJSON(1, "spoke-project", "abc1")
			issue["status"] = "closed"
			writeJSON(writer, map[string]any{
				"issue":          issue,
				"event":          nil,
				"original_event": closeRetryEventJSON(),
				"changed":        false,
				"reused":         true,
			})
		default:
			http.NotFound(writer, request)
		}
	})
	handlers := toolHandlers{options: Options{
		Client: client, Scope: NewAllScope(), Actor: "example-agent",
	}}
	revision := int64(7)
	_, output, err := handlers.close(t.Context(), nil, CloseInput{
		Ref: "spoke-project#abc1", Reason: "wontfix",
		Message:        "Reviewed the request and recorded why the work should stop here.",
		IdempotencyKey: "close-request-1", Revision: &revision,
	})
	require.NoError(t, err)
	require.Equal(t, "close-request-1", idempotencyKey)
	require.Equal(t, `"rev-7"`, ifMatch)
	require.Equal(t, "close-v1", retryProtocol)
	require.NotNil(t, output.Reused)
	require.True(t, *output.Reused)
	require.NotNil(t, output.Event)
	require.Equal(t, "01HCCCCCCCCCCCCCCCCCCCCCCC", output.Event.UID)
}

// A close receipt must fail closed when its returned issue cannot be projected
// into the MCP startup scope. Warning projection must not erase that error.
func TestClosePreservesMutationProjectionError(t *testing.T) {
	const projectUID = "01HAAAAAAAAAAAAAAAAAAAAAAA"
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{projectJSON(1, projectUID, "spoke-project")}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/projects/1/issues/abc1":
			issue := issueJSON(1, "spoke-project", "abc1")
			issue["project_uid"] = projectUID
			writeJSON(writer, map[string]any{"issue": issue, "comments": []any{}, "links": []any{}, "labels": []any{}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			issue := issueJSON(2, "other-project", "abc1")
			issue["project_uid"] = projectUID
			writeJSON(writer, map[string]any{"issue": issue, "changed": false})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: projectUID, Name: "spoke-project"}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "example-agent"}}
	_, _, err = handlers.close(t.Context(), nil, CloseInput{
		Ref: "spoke-project#abc1", Reason: "wontfix",
		Message: "Reviewed the request and recorded why the work should stop here.",
	})
	require.ErrorContains(t, err, "outside the MCP startup scope")
}

func TestCloseRejectsHistoricalRouteAfterIssueLeavesFixedScope(t *testing.T) {
	const (
		allowedProjectUID = "01HAAAAAAAAAAAAAAAAAAAAAAA"
		foreignProjectUID = "01HBBBBBBBBBBBBBBBBBBBBBBB"
	)
	var closeCalls int
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{projectJSON(1, allowedProjectUID, "spoke-project")}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/projects/1/issues/abc1":
			issue := issueJSON(2, "other-project", "def1")
			issue["project_uid"] = foreignProjectUID
			writeJSON(writer, map[string]any{"issue": issue, "comments": []any{}, "links": []any{}, "labels": []any{}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			closeCalls++
			writeJSON(writer, map[string]any{"issue": issueJSON(2, "other-project", "def1"), "changed": false})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: allowedProjectUID, Name: "spoke-project"}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "example-agent"}}
	_, _, err = handlers.close(t.Context(), nil, CloseInput{
		Ref: "spoke-project#abc1", Reason: "done",
		Message: "Reviewed the request and verified the proposed completion carefully.",
	})
	require.ErrorContains(t, err, "outside the MCP startup scope")
	require.Zero(t, closeCalls, "a historical route cannot close an issue after it leaves the fixed scope")
}

func TestScopedCloseRejectsResponseAfterProjectLeavesActiveScope(t *testing.T) {
	const projectUID = "01HAAAAAAAAAAAAAAAAAAAAAAA"
	var projectsRead int
	var archived bool
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			projectsRead++
			projects := []any{}
			if !archived {
				projects = append(projects, projectJSON(1, projectUID, "spoke-project"))
			}
			writeJSON(writer, map[string]any{"projects": projects})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/projects/1/issues/abc1":
			issue := issueJSON(1, "spoke-project", "abc1")
			issue["project_uid"] = projectUID
			writeJSON(writer, map[string]any{"issue": issue, "comments": []any{}, "links": []any{}, "labels": []any{}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			archived = true
			issue := issueJSON(1, "spoke-project", "abc1")
			issue["project_uid"] = projectUID
			writeJSON(writer, map[string]any{"issue": issue, "changed": true, "warnings": []any{}})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: projectUID, Name: "spoke-project"}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "example-agent"}}
	_, output, err := handlers.close(t.Context(), nil, CloseInput{
		Ref: "spoke-project#abc1", Reason: "done",
		Message: "Reviewed the request and verified the proposed completion carefully.",
	})
	require.ErrorContains(t, err, "MCP startup scope")
	require.Empty(t, output)
	require.Equal(t, 3, projectsRead, "close response visibility must revalidate active project membership")
}

func TestScopedCloseRedactsOpenRefutationDetailsWithoutEndpointProvenance(t *testing.T) {
	const projectUID = "01HAAAAAAAAAAAAAAAAAAAAAAA"
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{projectJSON(1, projectUID, "spoke-project")}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/projects/1/issues/abc1":
			issue := issueJSON(1, "spoke-project", "abc1")
			issue["project_uid"] = projectUID
			writeJSON(writer, map[string]any{"issue": issue, "comments": []any{}, "links": []any{}, "labels": []any{}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			writer.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(writer).Encode(map[string]any{"status": http.StatusConflict, "error": map[string]any{
				"code": "open_refutations", "message": "Hidden review excerpt from hidden-reviewer",
				"hint": "Inspect the hidden verdict comment",
			}})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: projectUID, Name: "spoke-project"}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "example-agent"}}
	_, _, err = handlers.close(t.Context(), nil, CloseInput{
		Ref: "spoke-project#abc1", Reason: "done",
		Message: "Reviewed the request and verified the proposed completion carefully.",
	})
	require.ErrorContains(t, err, "open_refutations")
	require.NotContains(t, err.Error(), "Hidden review excerpt")
	require.NotContains(t, err.Error(), "hidden-reviewer")
	require.NotContains(t, err.Error(), "hidden verdict comment")
}

func TestAuditClosesRedactsForeignVerdictReferences(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
		case "/api/v1/audit/closes":
			writeJSON(writer, map[string]any{"rows": []any{map[string]any{
				"time": "2026-08-12T00:00:00Z", "actor": "worker", "reason": "done", "issue": "abc1", "event_id": 1,
				"overruled_refutations": []string{"c:abc123", "other-project#def2:def456", "01HAAAAAAAAAAAAAAAAAAAAAAA"},
				"verdict_evidence":      []any{map[string]any{"type": "confirmed", "comment": "c:abc123"}, map[string]any{"type": "confirmed", "comment": "other-project#def2:def456"}},
				"verification_endpoint_projects": map[string]string{
					"c:abc123":                  "01HAAAAAAAAAAAAAAAAAAAAAAA",
					"other-project#def2:def456": "01HBBBBBBBBBBBBBBBBBBBBBBB",
				},
			}}})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	_, out, err := handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project"})
	require.NoError(t, err)
	require.Len(t, out.Rows, 1)
	require.Equal(t, []string{"c:abc123"}, out.Rows[0].OverruledRefutations)
	require.Len(t, out.Rows[0].VerdictEvidence, 1)
	require.Equal(t, "c:abc123", *out.Rows[0].VerdictEvidence[0].Comment)
	require.Nil(t, out.Rows[0].VerificationEndpointProjects, "internal provenance must not be exposed in MCP output")
}

func TestScopedVerificationWarningsUseEndpointProjectUIDs(t *testing.T) {
	const (
		allowedProjectUID = "01HAAAAAAAAAAAAAAAAAAAAAAA"
		foreignProjectUID = "01HBBBBBBBBBBBBBBBBBBBBBBB"
	)
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/projects" {
			writeJSON(writer, map[string]any{"projects": []any{projectJSON(1, allowedProjectUID, "spoke-project")}})
			return
		}
		http.NotFound(writer, request)
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: allowedProjectUID, Name: "spoke-project"}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	warnings := []generated.VerificationWarning{{
		Code: "open_refutations",
		Refutations: []generated.OpenRefutation{
			{Refutation: "c:local123", Target: "c:claim123", Author: "reviewer"},
			{Refutation: "c:foreign1", Target: "c:claim123", Author: "reviewer"},
		},
	}}
	endpointProjects := map[string]string{
		"c:local123": allowedProjectUID, "c:claim123": allowedProjectUID,
		"c:foreign1": foreignProjectUID,
	}

	filtered, err := handlers.scopedVerificationWarnings(t.Context(), warnings, endpointProjects)
	require.NoError(t, err)
	require.Len(t, filtered, 1)
	require.Len(t, filtered[0].Refutations, 1)
	require.Equal(t, "c:local123", filtered[0].Refutations[0].Refutation)

	filtered, err = handlers.scopedVerificationWarnings(t.Context(), warnings, nil)
	require.NoError(t, err)
	require.Empty(t, filtered, "handles without canonical project provenance must fail closed")
}

func TestScopedCloseDryRunUsesResponseEndpointProjectUIDs(t *testing.T) {
	const (
		allowedProjectUID = "01HAAAAAAAAAAAAAAAAAAAAAAA"
		foreignProjectUID = "01HBBBBBBBBBBBBBBBBBBBBBBB"
	)
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{projectJSON(1, allowedProjectUID, "spoke-project")}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/projects/1/issues/abc1":
			issue := issueJSON(1, "spoke-project", "abc1")
			issue["project_uid"] = allowedProjectUID
			writeJSON(writer, map[string]any{"issue": issue, "comments": []any{}, "links": []any{}, "labels": []any{}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			issue := issueJSON(1, "spoke-project", "abc1")
			issue["project_uid"] = allowedProjectUID
			writeJSON(writer, map[string]any{
				"issue": issue, "changed": false,
				"warnings": []any{map[string]any{"code": "open_refutations", "refutations": []any{
					map[string]any{"refutation": "c:local1", "target": "c:claim1", "author": "reviewer", "summary": "Visible review"},
					map[string]any{"refutation": "hub-project#other1:c:foreign1", "target": "c:claim1", "author": "foreign-reviewer", "summary": "Hidden review"},
				}}},
				"verification_endpoint_projects": map[string]string{
					"c:local1": allowedProjectUID, "c:claim1": allowedProjectUID,
					"hub-project#other1:c:foreign1": foreignProjectUID,
				},
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: allowedProjectUID, Name: "spoke-project"}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "example-agent"}}
	_, output, err := handlers.close(t.Context(), nil, CloseInput{
		Ref: "spoke-project#abc1", Reason: "done", DryRun: true,
		Message: "Reviewed the request and verified the proposed completion carefully.",
	})
	require.NoError(t, err)
	require.Len(t, output.Warnings, 1)
	require.Len(t, output.Warnings[0].Refutations, 1)
	require.Equal(t, "c:local1", output.Warnings[0].Refutations[0].Refutation)
	require.NotNil(t, output.Warnings[0].Refutations[0].Summary)
	require.NotContains(t, *output.Warnings[0].Refutations[0].Summary, "Hidden review")
}

func TestVerificationReferenceScopeUsesProjectUIDs(t *testing.T) {
	// A comment handle's display form cannot establish its owning project.
	hegel.Test(t, func(ht *hegel.T) {
		provenance := hegel.Draw(ht, hegel.Booleans())
		projectMatches := hegel.Draw(ht, hegel.Booleans())
		suffix := hegel.Draw(ht, hegel.Text())

		const allowedProjectUID = "01HAAAAAAAAAAAAAAAAAAAAAAA"
		ownerProjectUID := "01HBBBBBBBBBBBBBBBBBBBBBBB"
		if projectMatches {
			ownerProjectUID = allowedProjectUID
		}
		reference := "c:" + suffix
		endpointProjects := map[string]string{}
		if provenance {
			endpointProjects[reference] = ownerProjectUID
		}

		require.Equal(ht, provenance && projectMatches, verificationReferenceInScope(
			reference,
			endpointProjects,
			map[string]bool{allowedProjectUID: true},
		))
		require.False(ht, verificationReferenceInScope(
			reference+":near-miss",
			endpointProjects,
			map[string]bool{allowedProjectUID: true},
		), "similar display handles must not borrow endpoint provenance")
	}, hegel.WithTestCases(40), hegel.WithDatabase(t.TempDir()))
}

func TestScopedVerificationWarningsRequireProvenanceForBothEndpoints(t *testing.T) {
	const (
		allowedProjectUID = "01HAAAAAAAAAAAAAAAAAAAAAAA"
		foreignProjectUID = "01HBBBBBBBBBBBBBBBBBBBBBBB"
	)
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/projects" {
			writeJSON(writer, map[string]any{"projects": []any{projectJSON(1, allowedProjectUID, "spoke-project")}})
			return
		}
		http.NotFound(writer, request)
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: allowedProjectUID, Name: "spoke-project"}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	hegel.Test(t, func(ht *hegel.T) {
		refHasProvenance := hegel.Draw(ht, hegel.Booleans())
		refMatchesScope := hegel.Draw(ht, hegel.Booleans())
		targetHasProvenance := hegel.Draw(ht, hegel.Booleans())
		targetMatchesScope := hegel.Draw(ht, hegel.Booleans())
		refutationRef, targetRef := "c:refutation", "c:claim"
		endpointProjects := map[string]string{}
		if refHasProvenance {
			endpointProjects[refutationRef] = foreignProjectUID
			if refMatchesScope {
				endpointProjects[refutationRef] = allowedProjectUID
			}
		}
		if targetHasProvenance {
			endpointProjects[targetRef] = foreignProjectUID
			if targetMatchesScope {
				endpointProjects[targetRef] = allowedProjectUID
			}
		}
		summary := "Visible only with both endpoint proofs"
		warnings := []generated.VerificationWarning{{
			Code: "open_refutations",
			Refutations: []generated.OpenRefutation{{
				Refutation: refutationRef, Target: targetRef,
				Author: "reviewer", Summary: &summary,
			}},
		}}

		filtered, err := handlers.scopedVerificationWarnings(t.Context(), warnings, endpointProjects)
		require.NoError(ht, err)
		if refHasProvenance && refMatchesScope && targetHasProvenance && targetMatchesScope {
			require.Equal(ht, warnings, filtered)
			return
		}
		require.Empty(ht, filtered)
	}, hegel.WithTestCases(32), hegel.WithDatabase(t.TempDir()))
}

func TestAuditClosesFailsClosedOnUnqualifiedReferencesWithoutProvenance(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
		case "/api/v1/audit/closes":
			writeJSON(writer, map[string]any{"rows": []any{map[string]any{
				"time": "2026-08-12T00:00:00Z", "actor": "worker", "reason": "done", "issue": "abc1", "event_id": 1,
				"overruled_refutations": []string{"c:foreign1"},
				"verdict_evidence":      []any{map[string]any{"type": "confirmed", "comment": "c:foreign1"}},
			}}})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	_, out, err := handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project"})
	require.NoError(t, err)
	require.Len(t, out.Rows, 1)
	require.Empty(t, out.Rows[0].OverruledRefutations)
	require.Empty(t, out.Rows[0].VerdictEvidence)
}

func TestCloseRejectsReusedIssueMovedOutsideFixedScope(t *testing.T) {
	const (
		allowedProjectUID = "01HAAAAAAAAAAAAAAAAAAAAAAA"
		foreignProjectUID = "01HBBBBBBBBBBBBBBBBBBBBBBB"
	)
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, allowedProjectUID, "spoke-project"),
				projectJSON(2, foreignProjectUID, "other-project"),
			}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/projects/1/issues/abc1":
			issue := issueJSON(1, "spoke-project", "abc1")
			issue["project_uid"] = allowedProjectUID
			writeJSON(writer, map[string]any{"issue": issue, "comments": []any{}, "links": []any{}, "labels": []any{}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			issue := issueJSON(2, "other-project", "def1")
			issue["project_uid"] = foreignProjectUID
			issue["status"] = "closed"
			writeJSON(writer, map[string]any{
				"issue": issue, "original_event": closeRetryEventJSON(),
				"changed": false, "reused": true,
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: allowedProjectUID, Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{
		Client: client, Scope: scope, Actor: "example-agent",
	}}

	_, _, err = handlers.close(t.Context(), nil, CloseInput{
		Ref: "spoke-project#abc1", Reason: "wontfix",
		Message:        "Reviewed the request and recorded why the work should stop here.",
		IdempotencyKey: "close-request-1",
	})
	require.ErrorContains(t, err, "outside the MCP startup scope")
}

func TestCloseUsesCurrentProjectForReusedIssueMovedWithinScope(t *testing.T) {
	const (
		sourceProjectUID = "01HAAAAAAAAAAAAAAAAAAAAAAA"
		targetProjectUID = "01HBBBBBBBBBBBBBBBBBBBBBBB"
	)
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, sourceProjectUID, "source-project"),
				projectJSON(2, targetProjectUID, "target-project"),
			}})
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/projects/1/issues/abc1":
			issue := issueJSON(1, "source-project", "abc1")
			issue["project_uid"] = sourceProjectUID
			writeJSON(writer, map[string]any{"issue": issue, "comments": []any{}, "links": []any{}, "labels": []any{}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			issue := issueJSON(2, "target-project", "def1")
			issue["project_uid"] = targetProjectUID
			issue["status"] = "closed"
			writeJSON(writer, map[string]any{
				"issue": issue, "original_event": closeRetryEventJSON(),
				"changed": false, "reused": true,
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{
		{ID: 1, UID: sourceProjectUID, Name: "source-project"},
		{ID: 2, UID: targetProjectUID, Name: "target-project"},
	})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{
		Client: client, Scope: scope, Actor: "example-agent",
	}}

	_, output, err := handlers.close(t.Context(), nil, CloseInput{
		Ref: "source-project#abc1", Reason: "wontfix",
		Message:        "Reviewed the request and recorded why the work should stop here.",
		IdempotencyKey: "close-request-1",
	})
	require.NoError(t, err)
	require.Equal(t, ProjectIdentity{ID: 2, UID: targetProjectUID, Name: "target-project"}, output.Project)
	require.Equal(t, "target-project#def1", output.Issue.QualifiedRef)
}

func TestCloseRetryProtocolMakesLegacyDaemonRejectRequest(t *testing.T) {
	var closeCalls, mutations int
	var retryProtocol string
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			closeCalls++
			var body map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			retryProtocol, _ = body["retry_protocol"].(string)
			if retryProtocol != "" {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusBadRequest)
				writeJSON(writer, map[string]any{
					"status": http.StatusBadRequest,
					"error": map[string]any{
						"code": "validation", "message": "retry_protocol: unexpected property",
					},
				})
				return
			}
			mutations++
			writeJSON(writer, map[string]any{
				"issue": issueJSON(1, "spoke-project", "abc1"), "changed": true,
			})
		default:
			http.NotFound(writer, request)
		}
	})
	handlers := toolHandlers{options: Options{
		Client: client, Scope: NewAllScope(), Actor: "example-agent",
	}}
	_, _, err := handlers.close(t.Context(), nil, CloseInput{
		Ref: "spoke-project#abc1", Reason: "wontfix",
		Message:        "Reviewed the request and recorded why the work should stop here.",
		IdempotencyKey: "close-request-1",
	})
	require.Error(t, err)
	require.Equal(t, "close-v1", retryProtocol)
	require.Equal(t, 1, closeCalls)
	require.Zero(t, mutations)
}

func closeRetryEventJSON() map[string]any {
	return map[string]any{
		"id": 9, "uid": "01HCCCCCCCCCCCCCCCCCCCCCCC",
		"origin_instance_uid": "01HDDDDDDDDDDDDDDDDDDDDDDD",
		"project_id":          1, "project_uid": "01HAAAAAAAAAAAAAAAAAAAAAAA",
		"project_name": "spoke-project", "type": "issue.closed",
		"actor": "example-agent", "payload": `{}`,
		"hlc_physical_ms": 1, "hlc_counter": 0,
		"content_hash": strings.Repeat("a", 64),
		"created_at":   "2026-08-11T00:00:00Z",
	}
}
