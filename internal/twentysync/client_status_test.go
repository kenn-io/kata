package twentysync

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

func standardMetadata(t *testing.T, w http.ResponseWriter, r *http.Request, workspace string, fields []map[string]any) {
	t.Helper()
	query, _ := metadataOperation(t, r)
	switch {
	case strings.Contains(query, "KataTwentyWorkspace"):
		workspaceResponse(t, w, workspace)
	case strings.Contains(query, "KataTwentyObjects"):
		writeJSON(t, w, map[string]any{"data": map[string]any{"objects": connection([]map[string]any{{"id": "55555555-5555-4555-8555-555555555552", "nameSingular": "task", "isActive": true}}, false, "")}})
	case strings.Contains(query, "KataTwentyFields"):
		writeJSON(t, w, map[string]any{"data": map[string]any{"object": map[string]any{"id": "55555555-5555-4555-8555-555555555552", "nameSingular": "task", "isActive": true, "fields": connection(fields, false, "")}}})
	default:
		t.Errorf("unexpected query: %s", query)
	}
}

func TestClientStatusReadIsIndependentOfContent(t *testing.T) {
	for _, status := range []any{nil, "IN_PROGRESS", "DONE"} {
		client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/metadata" {
				standardMetadata(t, w, r, workspaceID, defaultFields())
				return
			}
			require.Equal(t, "/rest/tasks/"+taskID, r.URL.Path)
			row := taskWire(taskID)
			row["status"] = status
			delete(row, "bodyV2")
			delete(row, "title")
			writeJSON(t, w, map[string]any{"data": map[string]any{"task": row}})
		})
		session, err := client.ForRun(t.Context(), c)
		require.NoError(t, err)
		observed, err := session.(StatusSession).ReadStatus(t.Context(), c, taskID)
		require.NoError(t, err)
		if status == "DONE" {
			require.Equal(t, "closed", observed.Status)
			require.Equal(t, "done", observed.ClosedReason)
			require.NotNil(t, observed.ClosedAt)
		} else {
			require.Equal(t, "open", observed.Status)
			require.Nil(t, observed.ClosedAt)
		}
		if status == nil {
			require.Nil(t, observed.RawStatus)
		} else {
			require.Equal(t, status, *observed.RawStatus)
		}
	}
}

func TestClientStatusWritePatchesOnlyStatusAndVerifiesReadback(t *testing.T) {
	for _, desired := range []string{"closed", "open"} {
		t.Run(desired, func(t *testing.T) {
			status := "IN_PROGRESS"
			target := "DONE"
			if desired == "open" {
				status = "DONE"
				target = "TODO"
			}
			var patches, admissions int
			client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/metadata" {
					standardMetadata(t, w, r, workspaceID, defaultFields())
					return
				}
				if r.Method == "PATCH" {
					require.Equal(t, 1, admissions)
					patches++
					var payload map[string]any
					require.NoError(t, json.UnmarshalRead(r.Body, &payload))
					require.Equal(t, map[string]any{"status": target}, payload)
					status = target
					writeJSON(t, w, map[string]any{"data": map[string]any{"updateTask": map[string]any{"id": taskID}}})
					return
				}
				row := taskWire(taskID)
				row["status"] = status
				writeJSON(t, w, map[string]any{"data": map[string]any{"task": row}})
			})
			c.StatusSync = "two-way"
			session, err := client.ForRun(t.Context(), c)
			require.NoError(t, err)
			observed, err := session.(StatusSession).WriteStatus(t.Context(), c, taskID, desired, func() error { admissions++; return nil })
			require.NoError(t, err)
			require.Equal(t, desired, observed.Status)
			require.Equal(t, target, *observed.RawStatus)
			require.Equal(t, 1, patches)
		})
	}
}

func TestClientStatusWritePreservesMatchingOpenSubstate(t *testing.T) {
	var patches atomic.Int32
	client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metadata" {
			standardMetadata(t, w, r, workspaceID, defaultFields())
			return
		}
		if r.Method == "PATCH" {
			patches.Add(1)
			w.WriteHeader(500)
			return
		}
		row := taskWire(taskID)
		row["status"] = "IN_PROGRESS"
		writeJSON(t, w, map[string]any{"data": map[string]any{"task": row}})
	})
	c.StatusSync = "two-way"
	session, err := client.ForRun(t.Context(), c)
	require.NoError(t, err)
	observed, err := session.(StatusSession).WriteStatus(t.Context(), c, taskID, "open", func() error { return nil })
	require.NoError(t, err)
	require.Equal(t, "IN_PROGRESS", *observed.RawStatus)
	require.Zero(t, patches.Load())
}

func TestClientStatusWorkspaceMismatchBlocksIndependentReadsAndWrites(t *testing.T) {
	var requests atomic.Int32
	client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metadata" {
			workspaceResponse(t, w, "33333333-3333-4333-8333-333333333333")
			return
		}
		requests.Add(1)
		writeJSON(t, w, map[string]any{"data": map[string]any{"task": taskWire(taskID)}})
	})
	c.StatusSync = "two-way"
	session, err := client.ForRun(t.Context(), c)
	require.NoError(t, err)
	_, err = session.(StatusSession).ReadStatus(t.Context(), c, taskID)
	require.Error(t, err)
	_, err = session.(StatusSession).WriteStatus(t.Context(), c, taskID, "closed", func() error { return nil })
	require.Error(t, err)
	require.Zero(t, requests.Load())
}

func TestClientStatusAdmissionFencePreventsDispatch(t *testing.T) {
	var patches atomic.Int32
	client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metadata" {
			standardMetadata(t, w, r, workspaceID, defaultFields())
			return
		}
		if r.Method == "PATCH" {
			patches.Add(1)
			w.WriteHeader(500)
			return
		}
		writeJSON(t, w, map[string]any{"data": map[string]any{"task": taskWire(taskID)}})
	})
	c.StatusSync = "two-way"
	session, err := client.ForRun(t.Context(), c)
	require.NoError(t, err)
	fenced := errors.New("binding changed")
	_, err = session.(StatusSession).WriteStatus(t.Context(), c, taskID, "closed", func() error { return fenced })
	require.ErrorIs(t, err, fenced)
	require.Zero(t, patches.Load())
}

func TestClientStatusWriteNeverRetriesOrTrustsPatchResponse(t *testing.T) {
	for _, scenario := range []string{"server-error", "wrong-identity", "unchanged-readback", "lost-response"} {
		t.Run(scenario, func(t *testing.T) {
			var patches atomic.Int32
			client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/metadata" {
					standardMetadata(t, w, r, workspaceID, defaultFields())
					return
				}
				if r.Method == "PATCH" {
					patches.Add(1)
					if scenario == "server-error" {
						w.WriteHeader(500)
						return
					}
					if scenario == "lost-response" {
						conn, _, err := w.(http.Hijacker).Hijack()
						require.NoError(t, err)
						require.NoError(t, conn.Close())
						return
					}
					id := taskID
					if scenario == "wrong-identity" {
						id = workspaceID
					}
					writeJSON(t, w, map[string]any{"data": map[string]any{"updateTask": map[string]any{"id": id}}})
					return
				}
				writeJSON(t, w, map[string]any{"data": map[string]any{"task": taskWire(taskID)}})
			})
			c.StatusSync = "two-way"
			session, err := client.ForRun(t.Context(), c)
			require.NoError(t, err)
			_, err = session.(StatusSession).WriteStatus(t.Context(), c, taskID, "closed", func() error { return nil })
			require.Error(t, err)
			require.Equal(t, int32(1), patches.Load())
			var classified *issuesync.StatusError
			require.ErrorAs(t, err, &classified)
			require.True(t, classified.Ambiguous)
		})
	}
}

func TestClientStatusUnavailableOrUnknownRetainsAuthority(t *testing.T) {
	for _, scenario := range []string{"not-found", "deleted", "wrong-id", "unknown-status", "missing-status"} {
		t.Run(scenario, func(t *testing.T) {
			client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/metadata" {
					standardMetadata(t, w, r, workspaceID, defaultFields())
					return
				}
				row := taskWire(taskID)
				switch scenario {
				case "not-found":
					w.WriteHeader(404)
					return
				case "deleted":
					row["deletedAt"] = "2026-10-04T02:00:00Z"
				case "wrong-id":
					row["id"] = workspaceID
				case "unknown-status":
					row["status"] = "CUSTOM"
				case "missing-status":
					delete(row, "status")
				}
				writeJSON(t, w, map[string]any{"data": map[string]any{"task": row}})
			})
			session, err := client.ForRun(t.Context(), c)
			require.NoError(t, err)
			_, err = session.(StatusSession).ReadStatus(t.Context(), c, taskID)
			require.Error(t, err)
			var classified *issuesync.StatusError
			require.ErrorAs(t, err, &classified)
			require.True(t, classified.Blocked)
		})
	}
}

// One status pass shares a request-per-second budget across every mapping, so
// identity and schema checks belong to the run, not to each item.
func TestStatusRunVerifiesWorkspaceAndSchemaOncePerRun(t *testing.T) {
	var requests atomic.Int32
	status := "TODO"
	client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/metadata" {
			standardMetadata(t, w, r, workspaceID, defaultFields())
			return
		}
		require.Equal(t, "/rest/tasks/"+taskID, r.URL.Path)
		if r.Method == "PATCH" {
			status = "DONE"
			writeJSON(t, w, map[string]any{"data": map[string]any{"updateTask": map[string]any{"id": taskID}}})
			return
		}
		row := taskWire(taskID)
		row["status"] = status
		writeJSON(t, w, map[string]any{"data": map[string]any{"task": row}})
	})
	c.StatusSync = "two-way"
	raw, err := EncodeConfig(c)
	require.NoError(t, err)
	binding := db.IssueSyncBinding{Provider: "twenty", SourceKey: c.SourceKey(), RemoteID: c.RemoteID(), Config: raw}
	run, err := NewAdapter(nil, client).OpenStatus(t.Context(), binding, time.Time{})
	require.NoError(t, err)
	require.Equal(t, int32(1), requests.Swap(0), "workspace identity check")
	mapping := db.IssueStatusMapping{}
	mapping.Mapping.ExternalID = "task:" + taskID
	_, err = run.ReadStatus(t.Context(), mapping)
	require.NoError(t, err)
	require.Equal(t, int32(3), requests.Swap(0), "object and field metadata, then the task")
	for range 2 {
		_, err = run.ReadStatus(t.Context(), mapping)
		require.NoError(t, err)
		require.Equal(t, int32(1), requests.Swap(0), "task only")
	}
	observed, err := run.WriteStatus(t.Context(), mapping, "closed", func() error { return nil })
	require.NoError(t, err)
	require.Equal(t, "closed", observed.Status)
	require.Equal(t, int32(3), requests.Swap(0), "read, PATCH, verified readback")
}
