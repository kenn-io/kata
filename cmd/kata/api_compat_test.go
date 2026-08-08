package main

import (
	"context"
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
