package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/testenv"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func executeDocbankBrowseCLI(t *testing.T, base string, args ...string) (string, error) {
	t.Helper()
	resetFlags(t)
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(contextWithBaseURL(t.Context(), base))
	cmd.SetArgs(append([]string{"--project", "spoke-project", "docbank"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestDocbankBrowseCLIUsesProjectAndAllOutputModes(t *testing.T) {
	vault := docbanktest.New(t)
	version := uuid.MustParse(cliLinkVersion)
	vault.Seed("/kata", docbank.Node{ID: 30, Name: "kata", Kind: "dir"}, nil)
	vault.Seed("/kata/report.pdf", docbank.Node{ID: 31, Name: "report.pdf", Kind: "file", CurrentVersionID: version}, map[uuid.UUID][]byte{version: []byte("browse fixture")})
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.Docbank = docbank.NewClient(vault.Server.URL, func() (string, error) { return "test-key", nil })
		cfg.DocbankFolder = "/kata"
	})
	_, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	out, err := executeDocbankBrowseCLI(t, env.URL, "ls", "--limit", "1", "--offset", "0", "--json")
	require.NoError(t, err, out)
	var page struct {
		Items []struct {
			Path string `json:"path"`
		} `json:"items"`
		Total, Limit, Offset int
	}
	require.NoError(t, json.Unmarshal([]byte(out), &page))
	require.Equal(t, 1, page.Total)
	require.Equal(t, 1, page.Limit)
	require.Zero(t, page.Offset)
	require.Len(t, page.Items, 1)
	require.Equal(t, "/kata/report.pdf", page.Items[0].Path)
	out, err = executeDocbankBrowseCLI(t, env.URL, "resolve", "/kata/report.pdf", "--agent")
	require.NoError(t, err, out)
	require.Contains(t, out, "OK docbank-resolve")
	require.Contains(t, out, "path=/kata/report.pdf")
	require.Contains(t, out, "current_version_id="+cliLinkVersion)
	out, err = executeDocbankBrowseCLI(t, env.URL, "ls", "/kata")
	require.NoError(t, err, out)
	require.Contains(t, out, "report.pdf")
}

func TestDocbankBrowseCLIRequiresForwardingAPI(t *testing.T) {
	for _, version := range []string{"0.42.0", "missing", "invalid"} {
		t.Run(version, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					body := map[string]any{"ok": true}
					if version != "missing" {
						body["api_schema_version"] = version
					}
					_ = json.NewEncoder(w).Encode(body)
					return
				}
				calls.Add(1)
				http.Error(w, "fixture unexpected request", http.StatusBadRequest)
			}))
			t.Cleanup(server.Close)
			for _, args := range [][]string{{"ls"}, {"resolve", "/kata/file.pdf"}} {
				out, err := executeDocbankBrowseCLI(t, server.URL, args...)
				require.ErrorContains(t, err, "requires daemon API 0.43.0", out)
			}
			require.Zero(t, calls.Load())
		})
	}
}

func TestDocbankBrowseCLIRejectsUnboundedPageBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "fixture unexpected request", http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	for _, args := range [][]string{{"ls", "--limit", "0"}, {"ls", "--limit", "201"}, {"ls", "--offset", "-1"}, {"ls", "--limit", "9223372036854775808"}} {
		out, err := executeDocbankBrowseCLI(t, server.URL, args...)
		require.Error(t, err, out)
	}
	require.Zero(t, calls.Load())
}

func TestDocbankBrowseRootResponseMatchesGeneratedContract(t *testing.T) {
	root := map[string]any{"id": 1, "name": "", "kind": "dir", "path": "/", "size": 0}
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "test-key" {
			http.Error(w, "fixture auth", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/v1/path" {
			_ = json.NewEncoder(w).Encode(root)
			return
		}
		if r.URL.Path == "/api/v1/nodes/1/children" {
			_ = json.NewEncoder(w).Encode(map[string]any{"directory": root, "items": []any{}, "total": 0, "limit": 100, "offset": 0})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(native.Close)
	env := testenv.New(t, testenv.WithAuthToken("browse-fixture-token"), func(cfg *daemon.ServerConfig) {
		cfg.Docbank = docbank.NewClient(native.URL, func() (string, error) { return "test-key", nil })
		cfg.DocbankFolder = "/"
	})
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	client, err := kataclient.NewWithHTTPClient(env.URL, env.HTTP)
	require.NoError(t, err)
	auth := func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer browse-fixture-token")
		return nil
	}
	listed, err := client.DocbankChildrenWithResponse(t.Context(), &generated.DocbankChildrenRequestOptions{PathParams: &generated.DocbankChildrenPath{ProjectID: project.ID}}, auth)
	require.NoError(t, err)
	require.NotNil(t, listed.JSON200)
	require.NoError(t, listed.JSON200.Validate())
	resolved, err := client.DocbankResolveWithResponse(t.Context(), &generated.DocbankResolveRequestOptions{PathParams: &generated.DocbankResolvePath{ProjectID: project.ID}, Query: &generated.DocbankResolveQuery{Path: "/"}}, auth)
	require.NoError(t, err)
	require.NotNil(t, resolved.JSON200)
	require.NoError(t, resolved.JSON200.Validate())
}
