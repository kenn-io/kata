package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/docbank/docbanktest"
)

const cliLinkVersion = "6f1c2e8a-2222-4222-8222-222222222222"
const cliOldLinkVersion = "6f1c2e8a-2222-4222-8222-222222222223"

func TestAttachmentLinkCLI(t *testing.T) {
	env, f, ref := attachmentCLIEnv(t)
	f.Seed("/reports/report.pdf", docbanktest.Node{ID: 42, Name: "report.pdf", Kind: "file", CurrentVersionID: cliLinkVersion}, docbanktest.Version{ID: cliLinkVersion, NodeID: 42, Size: 12, MediaType: "application/pdf"}, docbanktest.Version{ID: cliOldLinkVersion, NodeID: 42, Size: 7, MediaType: "application/pdf"})
	out, err := executeAttachmentCLI(t, env.URL, "link", ref, "/reports/report.pdf", "--json")
	require.NoError(t, err)
	var linked api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal([]byte(out), &linked))
	require.NotEmpty(t, linked.ID)
	require.Equal(t, cliLinkVersion, linked.Attachment.DocbankVersion.String())
	out, err = executeAttachmentCLIWithRootFlags(t, env.URL, []string{"--agent"}, "link", ref, "42", "--version", cliOldLinkVersion)
	require.NoError(t, err)
	require.Contains(t, out, "OK docbank-attachments-link")
	require.NotContains(t, out, linked.ID)
	require.Contains(t, out, "size=7")
}

func TestAttachmentLinkCLIOldDaemon(t *testing.T) {
	env, _, ref := attachmentCLIEnv(t)
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.33.0"}`))
			return
		}
		if r.Method == "POST" && r.URL.Path != "/api/v1/projects/resolve" {
			posts++
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	_, err = executeAttachmentCLI(t, server.URL, "link", ref, "42")
	require.ErrorContains(t, err, "requires daemon API 0.34.0")
	require.Zero(t, posts)
}
