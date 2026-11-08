package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/transcriptservice"
)

func cliTranscriptDiscovery(t *testing.T) transcriptservice.Discovery {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	now := time.Now()
	d, err := transcriptservice.SignDescriptor(transcriptservice.Descriptor{Version: 1, ProjectUID: "00000000000000000000000001", AuthorityUID: "00000000000000000000000002", ServiceID: strings.Repeat("a", 64), LocatorBase: "https://archive.example", EnvelopeVersion: 1, MaxBytes: transcriptservice.MaxSnapshotBytes, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()}, key)
	require.NoError(t, err)
	return transcriptservice.Discovery{Descriptor: d, InstanceUID: d.AuthorityUID, Route: []transcriptservice.RouteObservation{{InstanceUID: d.AuthorityUID, ProjectUID: d.ProjectUID}}}
}

func TestTranscriptTargetRejectsOtherProjectAndRedirect(t *testing.T) {
	d := cliTranscriptDiscovery(t)
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
	_, err := fetchTranscriptTarget(t.Context(), server.Client(), server.URL, 7, "00000000000000000000000003")
	require.Error(t, err)
	got, err := fetchTranscriptTarget(t.Context(), server.Client(), server.URL, 7, d.Descriptor.ProjectUID)
	require.NoError(t, err)
	require.Equal(t, d, got)
	_, err = fetchTranscriptTarget(t.Context(), server.Client(), server.URL, 8, d.Descriptor.ProjectUID)
	require.Error(t, err)
	require.Zero(t, foreignCalls.Load())
}

func TestTranscriptTargetRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat(" ", transcriptservice.MaxDiscoveryBytes+1))
	}))
	t.Cleanup(server.Close)
	_, err := fetchTranscriptTarget(t.Context(), server.Client(), server.URL, 7, "00000000000000000000000001")
	require.Error(t, err)
}
