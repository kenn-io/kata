package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/testenv"
)

// Product contract: a children page is authorized by its own directory snapshot,
// never an earlier path lookup, and native extensions cannot escape projection.
func TestDocbankBrowseNativeProjectionAndPageProof(t *testing.T) {
	for _, scenario := range []string{"valid", "moved-outside", "moved-inside", "wrong-id", "missing-proof", "bad-name", "bad-kind", "negative-size", "wrong-limit", "negative-total", "false-total", "missing-items", "missing-total", "null-total", "beyond-end"} {
		t.Run(scenario, func(t *testing.T) {
			directory := map[string]any{"id": 10, "name": "kata", "kind": "dir", "path": "/kata", "size": 0, "source_metadata": map[string]any{"key": "private-fixture-sentinel"}}
			child := map[string]any{"id": 11, "name": "report.pdf", "kind": "file", "size": 7, "current_version_id": "6f1c2e8a-2222-4222-8222-222222222220", "parent_id": 10, "source_metadata": map[string]any{"url": "https://private.invalid/sentinel"}, "blob_hash": "private-fixture-sentinel"}
			page := map[string]any{"directory": directory, "items": []any{child}, "total": 1, "limit": 100, "offset": 0}
			query := ""
			switch scenario {
			case "moved-outside":
				directory["path"] = "/private"
			case "moved-inside":
				directory["path"] = "/kata/renamed"
			case "wrong-id":
				directory["id"] = 12
			case "missing-proof":
				delete(page, "directory")
			case "bad-name":
				child["name"] = "../secret"
			case "bad-kind":
				child["kind"] = "unknown"
			case "negative-size":
				child["size"] = -1
			case "wrong-limit":
				page["limit"] = 500
			case "negative-total":
				page["total"] = -1
			case "false-total":
				page["total"] = 0
			case "missing-items":
				delete(page, "items")
				page["total"] = 0
			case "missing-total":
				delete(page, "total")
				page["items"] = []any{}
			case "null-total":
				page["total"] = nil
				page["items"] = []any{}
			case "beyond-end":
				page["offset"] = math.MaxInt
				page["items"] = []any{}
				query = "?offset=" + strconv.Itoa(math.MaxInt)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "test-key", r.Header.Get("X-Api-Key"))
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/path":
					_ = json.MarshalWrite(w, map[string]any{"id": 10, "name": "kata", "kind": "dir", "path": "/kata", "size": 0, "source_metadata": map[string]any{"key": "private-fixture-sentinel"}})
				case "/api/v1/nodes/10/children":
					assert.Equal(t, "100", r.URL.Query().Get("limit"))
					_ = json.MarshalWrite(w, page)
				default:
					http.Error(w, "private-fixture-sentinel", http.StatusBadRequest)
				}
			}))
			t.Cleanup(upstream.Close)
			env := testenv.New(t, testenv.WithAuthToken("browse-fixture-token"), func(cfg *daemon.ServerConfig) {
				cfg.Docbank = docbank.NewClient(upstream.URL, func() (string, error) { return "test-key", nil })
				cfg.DocbankFolder = "/kata"
			})
			project, err := env.DB.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/docbank/children%s", env.URL, project.ID, query), nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer browse-fixture-token")
			resp, err := env.HTTP.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "private-fixture-sentinel")
			require.NotContains(t, string(raw), "private.invalid")
			require.NotContains(t, string(raw), "source_metadata")
			require.NotContains(t, string(raw), "parent_id")
			require.NotContains(t, string(raw), "blob_hash")
			if scenario != "valid" && scenario != "beyond-end" {
				require.GreaterOrEqual(t, resp.StatusCode, 400, string(raw))
				return
			}
			require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
			var result struct {
				Items []struct {
					Path string `json:"path"`
				} `json:"items"`
				Total  int `json:"total"`
				Limit  int `json:"limit"`
				Offset int `json:"offset"`
			}
			require.NoError(t, json.Unmarshal(raw, &result))
			require.Equal(t, 1, result.Total)
			require.Equal(t, 100, result.Limit)
			if scenario == "valid" {
				require.Len(t, result.Items, 1)
				require.Equal(t, "/kata/report.pdf", result.Items[0].Path)
			} else {
				require.Empty(t, result.Items)
				require.Equal(t, math.MaxInt, result.Offset)
			}
		})
	}
}

func TestDocbankBrowseProjectedPageByteBudget(t *testing.T) {
	vault := docbanktest.New(t)
	vault.Seed("/kata", docbank.Node{ID: 10, Name: "kata", Kind: "dir"}, nil)
	for i := range 200 {
		name := strings.Repeat("n", 3000) + strconv.Itoa(i)
		vault.Seed("/kata/"+name, docbank.Node{ID: int64(100 + i), Name: name, Kind: "file"}, nil)
	}
	env := testenv.New(t, testenv.WithAuthToken("browse-fixture-token"), func(cfg *daemon.ServerConfig) {
		cfg.Docbank = docbank.NewClient(vault.Server.URL, func() (string, error) { return "test-key", nil })
		cfg.DocbankFolder = "/kata"
	})
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/docbank/children?limit=200", env.URL, project.ID), nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer browse-fixture-token")
	response, err := env.HTTP.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadGateway, response.StatusCode, string(raw))
	require.Contains(t, string(raw), "docbank_browse_invalid")
	require.NotContains(t, string(raw), strings.Repeat("n", 64))
}
