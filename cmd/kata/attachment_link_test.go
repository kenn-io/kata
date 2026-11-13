package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
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
	require.Contains(t, out, "size=7")
}

func TestAttachmentLinkCLIRequiresCurrentAPIVersion(t *testing.T) {
	for _, tc := range []struct {
		name, version, floor string
		args                 []string
	}{
		{"link", "0.37.0", "0.38.0", []string{"link", "$ref", "42"}},
		{"link reuse unsupported", "0.38.0", "0.39.0", []string{"link", "$ref", "42", "--reuse-existing"}},
		{"link reuse supported", "0.39.0", "", []string{"link", "$ref", "42", "--reuse-existing"}},
		{"add reuse unsupported", "0.46.0", "0.47.0", []string{"add", "$ref", "$file", "--reuse-existing"}},
		{"add reuse supported", "0.47.0", "", []string{"add", "$ref", "$file", "--reuse-existing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, f, ref := attachmentCLIEnv(t)
			f.Seed("/reports/report.pdf", docbank.Node{ID: 42, Name: "report.pdf", Kind: "file", CurrentVersionID: uuid.MustParse(cliLinkVersion)}, map[uuid.UUID][]byte{uuid.MustParse(cliLinkVersion): []byte("sample bytes")})
			project, err := env.DB.ProjectByName(t.Context(), "spoke-project")
			require.NoError(t, err)
			target, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(target)
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"` + tc.version + `"}`))
					return
				}
				base := "/api/v1/projects/" + itoa(project.ID) + "/issues/" + ref + "/attachments"
				if r.Method == http.MethodPost && (r.URL.Path == base || r.URL.Path == base+"/links") {
					writes.Add(1)
				}
				// Keep the fixture request owner-local through the version proxy.
				r.Host = target.Host
				r.Header["X-Forwarded-For"] = nil
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			args := append([]string(nil), tc.args...)
			args[1] = ref
			if args[2] == "$file" {
				args[2] = filepath.Join(t.TempDir(), "upload.txt")
				require.NoError(t, os.WriteFile(args[2], []byte("upload bytes"), 0600))
			}
			_, err = executeAttachmentCLI(t, server.URL, args...)
			if tc.floor == "" {
				require.NoError(t, err)
				require.EqualValues(t, 1, writes.Load())
			} else {
				require.ErrorContains(t, err, "requires daemon API "+tc.floor+" or newer")
				require.Zero(t, writes.Load())
			}
		})
	}
}

func TestAttachmentLinkCLIInvalidSelector(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests++; http.Error(w, "unexpected", 500) }))
	defer server.Close()
	for _, selector := range []string{"report.pdf", "0"} {
		t.Run(selector, func(t *testing.T) {
			_, err := executeAttachmentCLI(t, server.URL, "link", "abc4", selector)
			require.ErrorContains(t, err, "Docbank file must be a positive node ID or an absolute path")
			require.Zero(t, requests)
		})
	}
}

func TestAttachmentReuseCLI(t *testing.T) {
	env, f, ref := attachmentCLIEnv(t)
	f.Seed("/reports/report.pdf", docbank.Node{ID: 42, Name: "report.pdf", Kind: "file", CurrentVersionID: uuid.MustParse(cliLinkVersion)}, map[uuid.UUID][]byte{uuid.MustParse(cliLinkVersion): []byte("sample bytes")})
	var first, second api.AddAttachmentResponseBody
	out, err := executeAttachmentCLI(t, env.URL, "link", ref, "42", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &first))
	out, err = executeAttachmentCLI(t, env.URL, "link", ref, "42", "--reuse-existing", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &second))
	require.Equal(t, first.ID, second.ID)

	file := filepath.Join(t.TempDir(), "upload.txt")
	require.NoError(t, os.WriteFile(file, []byte("upload bytes"), 0600))
	var ids []string
	for _, args := range [][]string{{"add", ref, file, "--json"}, {"add", ref, file, "--reuse-existing", "--json"}} {
		out, err = executeAttachmentCLI(t, env.URL, args...)
		require.NoError(t, err)
		var added struct {
			Results []struct {
				ID string `json:"attachment_id"`
			} `json:"results"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &added))
		require.Len(t, added.Results, 1)
		ids = append(ids, added.Results[0].ID)
	}
	require.NotEmpty(t, ids[0])
	require.Equal(t, ids[0], ids[1])
}
