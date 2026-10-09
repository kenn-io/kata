package twentysync

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClientRejectsRepeatedTaskCursorsAndDuplicateTasks(t *testing.T) {
	for _, scenario := range []string{"repeated-cursor", "duplicate-id"} {
		t.Run(scenario, func(t *testing.T) {
			var pages atomic.Int32
			client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/metadata" {
					workspaceResponse(t, w, workspaceID)
					return
				}
				page := pages.Add(1)
				id := taskID
				if scenario == "repeated-cursor" && page > 1 {
					id = workspaceID
				}
				writeJSON(t, w, map[string]any{"data": map[string]any{"tasks": []any{taskWire(id)}}, "pageInfo": map[string]any{"hasNextPage": true, "endCursor": "same-cursor"}})
			})
			session, err := client.ForRun(t.Context(), c)
			require.NoError(t, err)
			rows, err := session.Tasks(t.Context(), c)
			require.Error(t, err)
			require.Nil(t, rows)
			require.Equal(t, int32(2), pages.Load())
		})
	}
}

func TestClientTaskPageLimitStopsEndlessEmptyPages(t *testing.T) {
	var pages atomic.Int32
	client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metadata" {
			workspaceResponse(t, w, workspaceID)
			return
		}
		page := pages.Add(1)
		writeJSON(t, w, map[string]any{"data": map[string]any{"tasks": []any{}}, "pageInfo": map[string]any{"hasNextPage": true, "endCursor": fmt.Sprint(page)}})
	})
	session, err := client.ForRun(t.Context(), c)
	require.NoError(t, err)
	_, err = session.Tasks(t.Context(), c)
	require.Error(t, err)
	require.Equal(t, int32(1000), pages.Load())
}

func TestClientResponseAndMarkdownLimits(t *testing.T) {
	for _, scenario := range []string{"response", "markdown"} {
		t.Run(scenario, func(t *testing.T) {
			client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/metadata" {
					workspaceResponse(t, w, workspaceID)
					return
				}
				if scenario == "response" {
					_, _ = w.Write([]byte(strings.Repeat(" ", 8<<20) + " "))
					return
				}
				row := taskWire(taskID)
				row["bodyV2"] = map[string]any{"markdown": strings.Repeat("a", (1<<20)+1)}
				writeJSON(t, w, map[string]any{"data": map[string]any{"tasks": []any{row}}, "pageInfo": map[string]any{"hasNextPage": false}})
			})
			session, err := client.ForRun(t.Context(), c)
			require.NoError(t, err)
			rows, err := session.Tasks(t.Context(), c)
			require.Error(t, err)
			require.Nil(t, rows)
		})
	}
}

func TestClientMetadataRejectsErrorsAndIncompatibleFields(t *testing.T) {
	for _, scenario := range []string{"graphql-error", "wrong-type", "missing-body", "unknown-option", "duplicate-options"} {
		t.Run(scenario, func(t *testing.T) {
			client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if scenario == "graphql-error" {
					writeJSON(t, w, map[string]any{"errors": []any{map[string]any{"message": "example-token secret-detail"}}})
					return
				}
				fields := defaultFields()
				switch scenario {
				case "wrong-type":
					fields[2]["type"] = "TEXT"
				case "missing-body":
					fields = append(fields[:1], fields[2:]...)
				case "unknown-option":
					fields[2]["options"] = []map[string]any{{"value": "CUSTOM"}}
				case "duplicate-options":
					fields[2]["options"] = []map[string]any{{"value": "TODO"}, {"value": "TODO"}}
				}
				standardMetadata(t, w, r, workspaceID, fields)
			})
			session, err := client.ForRun(t.Context(), c)
			require.NoError(t, err)
			_, err = session.Schema(t.Context(), c)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "example-token")
			require.NotContains(t, err.Error(), "secret-detail")
		})
	}
}

func TestClientPersistentReadFailuresHaveFiniteRetryBudget(t *testing.T) {
	var requests atomic.Int32
	client, c, clock := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "86400")
		w.WriteHeader(503)
	})
	session, err := client.ForRun(t.Context(), c)
	require.NoError(t, err)
	_, err = session.Workspace(t.Context(), c)
	require.Error(t, err)
	require.Equal(t, int32(4), requests.Load())
	require.Len(t, clock.waits, 3)
	for _, delay := range clock.waits {
		require.Equal(t, 20*time.Minute, delay)
	}
}

func TestClientMetadataBudgetCountsWholeWireResponse(t *testing.T) {
	client, c, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat(" ", 1000) + `{"data":{"currentWorkspace":{"id":"11111111-1111-4111-8111-111111111111","displayName":"Example workspace"}}}`))
	})
	session, err := client.ForRun(t.Context(), c)
	require.NoError(t, err)
	used := (128 << 20) - 200
	_, err = session.(*clientSession).graphqlBounded(t.Context(), workspaceQuery, nil, &used)
	require.Error(t, err)
}
