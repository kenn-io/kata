package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilteredReadyAllRejectsOldDaemonBeforeQuery(t *testing.T) {
	var readyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.7.0"}`))
		case "/api/v1/ready":
			readyCalls.Add(1)
			_, _ = w.Write([]byte(`{"issues":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	_, _, err := executeRootCapture(t,
		contextWithBaseURL(context.Background(), server.URL),
		"ready", "--all", "--label", "handoff")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires daemon API 0.8.0 or newer")
	assert.Contains(t, err.Error(), "reports 0.7.0")
	assert.Contains(t, err.Error(), "upgrade the daemon")
	assert.Zero(t, readyCalls.Load(), "the unfiltered old endpoint must not be queried")
}

func TestFilteredSearchRejectsOldDaemonBeforeQuery(t *testing.T) {
	var searchCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.7.0"}`))
		case "/api/v1/projects/resolve":
			_, _ = w.Write([]byte(`{"project":{"id":1,"name":"example-project"}}`))
		case "/api/v1/projects/1/search":
			searchCalls.Add(1)
			_, _ = w.Write([]byte(`{"query":"handoff","mode":"lexical","results":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	_, _, err := executeRootCapture(t,
		contextWithBaseURL(context.Background(), server.URL),
		"--project", "example-project", "search", "handoff", "--no-label", "parked")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires daemon API 0.8.0 or newer")
	assert.Contains(t, err.Error(), "reports 0.7.0")
	assert.Zero(t, searchCalls.Load(), "the unfiltered old endpoint must not be queried")
}

func TestFilteredListAllRejectsDaemonBeforeGlobalListFilters(t *testing.T) {
	var listCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.8.0"}`))
		case "/api/v1/issues":
			listCalls.Add(1)
			_, _ = w.Write([]byte(`{"issues":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	_, _, err := executeRootCapture(t,
		contextWithBaseURL(context.Background(), server.URL),
		"list", "--all", "--unowned")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires daemon API 0.9.0 or newer")
	assert.Contains(t, err.Error(), "reports 0.8.0")
	assert.Zero(t, listCalls.Load(), "the unfiltered old endpoint must not be queried")
}

func TestCloseRetryFlagsMakeOldDaemonRejectCloseBeforeMutation(t *testing.T) {
	var closeCalls, mutations atomic.Int32
	var retryProtocol string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/resolve":
			_, _ = w.Write([]byte(`{"project":{"id":1,"name":"example-project"}}`))
		case "/api/v1/projects/1/issues/abc1/actions/close":
			closeCalls.Add(1)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			retryProtocol, _ = body["retry_protocol"].(string)
			if retryProtocol != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"status":400,"error":{"code":"validation","message":"retry_protocol: unexpected property"}}`))
				return
			}
			mutations.Add(1)
			_, _ = w.Write([]byte(`{"changed":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	_, _, err := executeRootCapture(t,
		contextWithBaseURL(context.Background(), server.URL),
		"--project", "example-project", "close", "abc1",
		"--wontfix",
		"--message", "Reviewed the request and recorded why the work should stop here.",
		"--idempotency-key", "close-request-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "retry_protocol")
	assert.Equal(t, "close-v1", retryProtocol)
	assert.Equal(t, int32(1), closeCalls.Load())
	assert.Zero(t, mutations.Load(), "the legacy request schema must reject before mutation")
}

func TestListAllDefaultsToUnlimited(t *testing.T) {
	var sentLimit atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/issues" {
			http.NotFound(w, r)
			return
		}
		sentLimit.Store(r.URL.Query().Has("limit"))
		_, _ = w.Write([]byte(`{"issues":[]}`))
	}))
	t.Cleanup(server.Close)

	_, _, err := executeRootCapture(t,
		contextWithBaseURL(context.Background(), server.URL), "list", "--all")
	require.NoError(t, err)
	assert.False(t, sentLimit.Load(), "list --all must not silently cap a fleet scan at 200 rows")
}

func TestAPIVersionAtLeast(t *testing.T) {
	tests := []struct {
		reported string
		required string
		want     bool
		valid    bool
	}{
		{reported: "0.8.0", required: "0.8.0", want: true, valid: true},
		{reported: "0.9.0", required: "0.8.0", want: true, valid: true},
		{reported: "1.0.0", required: "0.9.0", want: true, valid: true},
		{reported: "0.7.9", required: "0.8.0", want: false, valid: true},
		{reported: "0.9.0-dev", required: "0.9.0", want: true, valid: true},
		{reported: "", required: "0.8.0", want: false, valid: false},
		{reported: "not-semver", required: "0.8.0", want: false, valid: false},
	}
	for _, tt := range tests {
		t.Run(tt.reported+"_requires_"+tt.required, func(t *testing.T) {
			got, valid := apiVersionAtLeast(tt.reported, tt.required)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.valid, valid)
		})
	}
}
