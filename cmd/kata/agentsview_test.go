package main

import (
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentsviewservice"
	daemonclient "go.kenn.io/kata/internal/client"
)

func cliAgentsViewDiscovery(t *testing.T) agentsviewservice.Discovery {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	now := time.Now()
	d, err := agentsviewservice.SignDescriptor(agentsviewservice.Descriptor{Version: 1, ProjectUID: "00000000000000000000000001", AuthorityUID: "00000000000000000000000002", ServiceID: strings.Repeat("a", 64), LocatorBase: "https://archive.example", EnvelopeVersion: 1, MaxBytes: agentsviewservice.MaxSnapshotBytes, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()}, key)
	require.NoError(t, err)
	return agentsviewservice.Discovery{Descriptor: d, InstanceUID: d.AuthorityUID, Route: []agentsviewservice.RouteObservation{{InstanceUID: d.AuthorityUID, ProjectUID: d.ProjectUID}}}
}

func TestAgentsViewTargetRejectsOtherProjectAndRedirect(t *testing.T) {
	d := cliAgentsViewDiscovery(t)
	var foreignCalls atomic.Int64
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { foreignCalls.Add(1); w.WriteHeader(http.StatusOK) }))
	t.Cleanup(foreign.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/8/transcript-service" {
			http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
			return
		}
		require.NoError(t, json.MarshalWrite(w, d))
	}))
	t.Cleanup(server.Close)
	_, err := fetchAgentsViewTarget(t.Context(), server.Client(), server.URL, 7, "00000000000000000000000003")
	require.Error(t, err)
	got, err := fetchAgentsViewTarget(t.Context(), server.Client(), server.URL, 7, d.Descriptor.ProjectUID)
	require.NoError(t, err)
	require.Equal(t, d, got)
	_, err = fetchAgentsViewTarget(t.Context(), server.Client(), server.URL, 8, d.Descriptor.ProjectUID)
	require.Error(t, err)
	require.Zero(t, foreignCalls.Load())
}

func TestAgentsViewTargetRejectsOversizedValidResponse(t *testing.T) {
	d, err := json.Marshal(cliAgentsViewDiscovery(t))
	require.NoError(t, err)
	oversized := append(d, bytes.Repeat([]byte{' '}, agentsviewservice.MaxDiscoveryBytes-len(d)+1)...)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(oversized)
	}))
	t.Cleanup(server.Close)
	_, err = fetchAgentsViewTarget(t.Context(), server.Client(), server.URL, 7, "00000000000000000000000001")
	require.Error(t, err)
}

func TestAgentsViewTargetRejectsCompressedResponse(t *testing.T) {
	d, err := json.Marshal(cliAgentsViewDiscovery(t))
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		_, _ = writer.Write(d)
		require.NoError(t, writer.Close())
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed.Bytes())
	}))
	t.Cleanup(server.Close)
	_, err = fetchAgentsViewTarget(t.Context(), server.Client(), server.URL, 7, "00000000000000000000000001")
	require.ErrorContains(t, err, "compressed service response rejected")
}

func TestAgentsViewTargetBoundsResponseHeaders(t *testing.T) {
	d, err := json.Marshal(cliAgentsViewDiscovery(t))
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Oversized", strings.Repeat("x", 16<<10+1))
		_, _ = w.Write(d)
	}))
	t.Cleanup(server.Close)
	client, err := httpClientForResolvedWithOpts(t.Context(), daemonclient.ResolvedDaemon{BaseURL: server.URL, Direct: true}, daemonclient.Opts{
		Timeout: 5 * time.Second, ResponseHeaderTimeout: 2 * time.Second,
		MaxResponseHeaderBytes: 16 << 10, DisableCompression: true,
	})
	require.NoError(t, err)
	_, err = fetchAgentsViewTarget(t.Context(), client, server.URL, 7, "00000000000000000000000001")
	require.Error(t, err)
}

func TestAgentsViewTargetHumanSummaryOmitsAuthorityDigestsAndIncludesBounds(t *testing.T) {
	d := cliAgentsViewDiscovery(t)
	got := formatAgentsViewTarget(d)
	require.Contains(t, got, d.Descriptor.LocatorBase)
	require.Contains(t, got, "Maximum bytes: 67108864")
	require.Contains(t, got, "Expires:")
	require.NotContains(t, got, d.Descriptor.AuthorityUID)
	require.NotContains(t, got, d.Descriptor.ServiceID)
	require.NotContains(t, got, "Route nodes:")
}
