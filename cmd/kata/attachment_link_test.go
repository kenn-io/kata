package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/docbank"
)

const cliLinkVersion = "6f1c2e8a-2222-4222-8222-222222222222"
const cliOldLinkVersion = "6f1c2e8a-2222-4222-8222-222222222223"

func TestAttachmentLinkCLI(t *testing.T) {
	env, f, ref := attachmentCLIEnv(t)
	f.Seed("/reports/report.pdf", docbank.Node{ID: 42, Name: "report.pdf", Kind: "file", CurrentVersionID: uuid.MustParse(cliLinkVersion)}, map[uuid.UUID][]byte{uuid.MustParse(cliLinkVersion): []byte("sample bytes"), uuid.MustParse(cliOldLinkVersion): []byte("old pdf")})
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

func TestAttachmentLinkCLIInvalidSelector(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests++; http.Error(w, "unexpected", 500) }))
	defer server.Close()
	_, err := executeAttachmentCLI(t, server.URL, "link", "abc4", "invalid")
	require.Error(t, err)
	require.Zero(t, requests)
}
