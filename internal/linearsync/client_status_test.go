package linearsync

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

const closedStateID = "66666666-6666-4666-8666-666666666666"

func TestStatusWriteRefreshesLiveTargetsAfterCachedRead(t *testing.T) {
	changed := false
	current := stateID
	var writes []string
	c := mockClient(t, func(r *http.Request) (*http.Response, error) {
		query, variables := requestBody(t, r)
		switch {
		case strings.Contains(query, "KataLinearScope"):
			return dataResponse(map[string]any{"viewer": map[string]any{"organization": map[string]any{"id": workspaceID}}, "team": map[string]any{"id": teamID, "name": "Example team", "organization": map[string]any{"id": workspaceID}, "archivedAt": nil}}), nil
		case strings.Contains(query, "KataLinearStates"):
			closedType := "completed"
			if changed {
				closedType = "canceled"
			}
			nodes := []any{map[string]any{"id": stateID, "type": "unstarted", "position": 0, "team": map[string]any{"id": teamID}, "archivedAt": nil}, map[string]any{"id": closedStateID, "type": closedType, "position": 1, "team": map[string]any{"id": teamID}, "archivedAt": nil}}
			if changed {
				nodes = append(nodes, map[string]any{"id": projectID, "type": "completed", "position": 2, "team": map[string]any{"id": teamID}, "archivedAt": nil})
			}
			return dataResponse(map[string]any{"workflowStates": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false}}}), nil
		case strings.Contains(query, "KataLinearStatus"):
			i := wireIssue()
			i["state"] = map[string]any{"id": current}
			return dataResponse(map[string]any{"issue": i}), nil
		case strings.Contains(query, "KataLinearUpdate"):
			current = variables["input"].(map[string]any)["stateId"].(string)
			writes = append(writes, current)
			return dataResponse(map[string]any{"issueUpdate": map[string]any{"success": true, "issue": map[string]any{"id": issueID}}}), nil
		default:
			t.Fatalf("unexpected query %s", query)
			return nil, nil
		}
	})
	session, err := c.ForRun(t.Context(), statusConfig())
	require.NoError(t, err)
	run := &linearStatusRun{session: session, status: session.(StatusSession), config: statusConfig()}
	mapping := db.IssueStatusMapping{Mapping: db.ImportMapping{ExternalID: "issue:" + issueID}}
	_, err = run.ReadStatus(t.Context(), mapping)
	require.NoError(t, err)
	changed = true
	_, err = run.WriteStatus(t.Context(), mapping, "closed", func() error { return nil })
	require.NoError(t, err)
	require.Equal(t, []string{projectID}, writes, "a cached completed state that became canceled must not receive a completion write")
}

func statusConfig() Config { c := testConfig(); c.StatusSync = "two-way"; return c }

func TestStatusRefusesTrashedIssue(t *testing.T) {
	for _, operation := range []string{"read", "write"} {
		t.Run(operation, func(t *testing.T) {
			writes := 0
			c := mockClient(t, statusTransport(t, func(*http.Request, map[string]any) (*http.Response, error) {
				writes++
				return dataResponse(map[string]any{"issueUpdate": map[string]any{"success": true, "issue": map[string]any{"id": issueID}}}), nil
			}, func() map[string]any { i := wireIssue(); i["trashed"] = true; return i }))
			s, err := c.ForRun(t.Context(), statusConfig())
			require.NoError(t, err)
			status := s.(StatusSession)
			if operation == "read" {
				_, err = status.ReadStatus(t.Context(), statusConfig(), issueID)
			} else {
				_, err = status.WriteStatus(t.Context(), statusConfig(), issueID, "closed", func() error { return nil })
			}
			var classified *issuesync.StatusError
			require.ErrorAs(t, err, &classified)
			require.True(t, classified.Blocked)
			require.Zero(t, writes)
		})
	}
}

func TestStatusReadbackTrashRetainsAmbiguity(t *testing.T) {
	mutated := false
	c := mockClient(t, statusTransport(t, func(*http.Request, map[string]any) (*http.Response, error) {
		mutated = true
		return dataResponse(map[string]any{"issueUpdate": map[string]any{"success": true, "issue": map[string]any{"id": issueID}}}), nil
	}, func() map[string]any {
		i := wireIssue()
		if mutated {
			i["state"] = map[string]any{"id": closedStateID}
			i["trashed"] = true
		}
		return i
	}))
	s, err := c.ForRun(t.Context(), statusConfig())
	require.NoError(t, err)
	_, err = s.(StatusSession).WriteStatus(t.Context(), statusConfig(), issueID, "closed", func() error { return nil })
	var classified *issuesync.StatusError
	require.ErrorAs(t, err, &classified)
	require.True(t, classified.Ambiguous)
}

func statusTransport(t *testing.T, mutation func(*http.Request, map[string]any) (*http.Response, error), issue func() map[string]any) roundTrip {
	return func(r *http.Request) (*http.Response, error) {
		query, v := requestBody(t, r)
		switch {
		case strings.Contains(query, "KataLinearScope"):
			return dataResponse(map[string]any{"viewer": map[string]any{"organization": map[string]any{"id": workspaceID}}, "team": map[string]any{"id": teamID, "name": "Example team", "organization": map[string]any{"id": workspaceID}, "archivedAt": nil}}), nil
		case strings.Contains(query, "KataLinearStates"):
			return dataResponse(map[string]any{"workflowStates": map[string]any{"nodes": []any{map[string]any{"id": stateID, "type": "unstarted", "position": 0, "team": map[string]any{"id": teamID}, "archivedAt": nil}, map[string]any{"id": closedStateID, "type": "completed", "position": 1, "team": map[string]any{"id": teamID}, "archivedAt": nil}}, "pageInfo": map[string]any{"hasNextPage": false}}}), nil
		case strings.Contains(query, "KataLinearStatus"):
			require.Equal(t, issueID, v["id"])
			return dataResponse(map[string]any{"issue": issue()}), nil
		case strings.Contains(query, "KataLinearUpdate"):
			return mutation(r, v)
		default:
			t.Fatalf("unexpected query %s", query)
			return nil, nil
		}
	}
}
func TestStatusWritesOnlyStateAndVerifiesReadback(t *testing.T) {
	current := stateID
	writes := 0
	admissions := 0
	c := mockClient(t, statusTransport(t, func(_ *http.Request, v map[string]any) (*http.Response, error) {
		writes++
		require.Equal(t, map[string]any{"id": issueID, "input": map[string]any{"stateId": closedStateID}}, v)
		current = closedStateID
		return dataResponse(map[string]any{"issueUpdate": map[string]any{"success": true, "issue": map[string]any{"id": issueID}}}), nil
	}, func() map[string]any { i := wireIssue(); i["state"] = map[string]any{"id": current}; return i }))
	s, err := c.ForRun(context.Background(), statusConfig())
	require.NoError(t, err)
	status := s.(StatusSession)
	got, err := status.WriteStatus(context.Background(), statusConfig(), issueID, "closed", func() error { admissions++; return nil })
	require.NoError(t, err)
	require.Equal(t, "closed", got.Status)
	require.Equal(t, closedStateID, *got.RawStatus)
	require.Equal(t, "done", got.ClosedReason)
	_, err = status.WriteStatus(context.Background(), statusConfig(), issueID, "closed", func() error { admissions++; return nil })
	require.NoError(t, err)
	require.Equal(t, 1, writes)
	require.Equal(t, 1, admissions)
	_, err = status.WriteStatus(context.Background(), testConfig(), issueID, "open", func() error { return nil })
	require.Error(t, err)
	require.Equal(t, 1, writes)
}
func TestStatusMutationUnprovenOutcomesAreAmbiguous(t *testing.T) {
	for _, body := range []string{`{"data":{"issueUpdate":{"success":true,"issue":{"id":"` + issueID + `"}}},"errors":[{"message":"secret body"}]}`, `{"data":{"issueUpdate":{"success":false}}}`, `{"data":{}}`, `{"data":{"issueUpdate":{"success":true,"issue":{"id":"` + teamID + `"}}}}`} {
		calls := 0
		c := mockClient(t, statusTransport(t, func(*http.Request, map[string]any) (*http.Response, error) { calls++; return response(200, body), nil }, wireIssue))
		s, err := c.ForRun(context.Background(), statusConfig())
		require.NoError(t, err)
		_, err = s.(StatusSession).WriteStatus(context.Background(), statusConfig(), issueID, "closed", func() error { return nil })
		var classified *issuesync.StatusError
		require.ErrorAs(t, err, &classified)
		require.True(t, classified.Ambiguous)
		require.Equal(t, 1, calls)
		require.NotContains(t, err.Error(), "secret body")
	}
}
func TestStatusScopeArchiveAndAdmissionGuards(t *testing.T) {
	for _, change := range []func(map[string]any){func(i map[string]any) { i["team"] = map[string]any{"id": projectID} }, func(i map[string]any) { i["archivedAt"] = "2026-01-03T00:00:00Z" }, func(i map[string]any) { i["updatedAt"] = nil }} {
		writes := 0
		c := mockClient(t, statusTransport(t, func(*http.Request, map[string]any) (*http.Response, error) { writes++; return nil, nil }, func() map[string]any { i := wireIssue(); change(i); return i }))
		s, err := c.ForRun(context.Background(), statusConfig())
		require.NoError(t, err)
		_, err = s.(StatusSession).WriteStatus(context.Background(), statusConfig(), issueID, "closed", func() error { return nil })
		require.Error(t, err)
		require.Zero(t, writes)
	}
	writes := 0
	c := mockClient(t, statusTransport(t, func(*http.Request, map[string]any) (*http.Response, error) { writes++; return nil, nil }, wireIssue))
	s, err := c.ForRun(context.Background(), statusConfig())
	require.NoError(t, err)
	_, err = s.(StatusSession).WriteStatus(context.Background(), statusConfig(), issueID, "closed", func() error { return context.Canceled })
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, writes)
}
func TestStatusReadbackScopeChangeRetainsAmbiguity(t *testing.T) {
	mutated := false
	c := mockClient(t, statusTransport(t, func(*http.Request, map[string]any) (*http.Response, error) {
		mutated = true
		return dataResponse(map[string]any{"issueUpdate": map[string]any{"success": true, "issue": map[string]any{"id": issueID}}}), nil
	}, func() map[string]any {
		i := wireIssue()
		if mutated {
			i["team"] = map[string]any{"id": projectID}
		}
		return i
	}))
	s, err := c.ForRun(context.Background(), statusConfig())
	require.NoError(t, err)
	_, err = s.(StatusSession).WriteStatus(context.Background(), statusConfig(), issueID, "closed", func() error { return nil })
	var classified *issuesync.StatusError
	require.ErrorAs(t, err, &classified)
	require.True(t, classified.Ambiguous)
}
func TestStatusTargetsRequireLiveSelectedTypes(t *testing.T) {
	c := statusConfig()
	states := []State{{ID: stateID, Type: "unstarted", Position: 2}, {ID: closedStateID, Type: "completed", Position: 3}}
	require.NoError(t, ValidateStatusTargets(c, states))
	c.OpenStateID = closedStateID
	require.Error(t, ValidateStatusTargets(c, states))
	c.OpenStateID = projectID
	require.Error(t, ValidateStatusTargets(c, states))
	require.Error(t, ValidateStatusTargets(statusConfig(), states[:1]))
	// Source versions and closure times remain provider-owned timestamps.
	i := testIssue()
	at := i.UpdatedAt.Add(-time.Minute)
	i.CompletedAt = &at
	status, reason, closed, err := issueStatus(i, "completed")
	require.NoError(t, err)
	require.Equal(t, "closed", status)
	require.Equal(t, "done", *reason)
	require.Equal(t, at, *closed)
}
