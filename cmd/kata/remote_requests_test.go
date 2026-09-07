package main

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/testfix"
)

// A remote command should spend its requests on project resolution and the
// operation itself, not a preliminary liveness check. Exercise every remote
// selection source against a real, authenticated daemon and persisted writes.
func TestRemoteMutationsWithoutPreflight(t *testing.T) {
	for _, source := range []string{"environment", "workspace", "active", "named"} {
		t.Run(source, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("fixture-token"))
			workspace := t.TempDir()
			testfix.WriteKataToml(t, workspace, "example-project")
			t.Chdir(workspace)
			t.Setenv("KATA_SERVER", "")
			project, err := env.DB.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			target, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(target)
			var mu sync.Mutex
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.Method+" "+r.URL.Path)
				mu.Unlock()
				r.Host = target.Host
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			args := []string{"--project", project.Name, "--json"}
			switch source {
			case "environment":
				t.Setenv("KATA_SERVER", server.URL)
			case "workspace":
				require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"),
					[]byte(fmt.Sprintf("version = 1\n[server]\nurl = %q\n", server.URL)), 0o600))
			case "active", "named":
				t.Setenv("KATA_AUTH_TOKEN", "")
				config := fmt.Sprintf("[[daemon]]\nname = \"example-remote\"\nurl = %q\ntoken = \"fixture-token\"\n", server.URL)
				if source == "active" {
					config = "active_daemon = \"example-remote\"\n" + config
				} else {
					args = append(args, "--daemon", "example-remote")
				}
				require.NoError(t, os.WriteFile(filepath.Join(env.Home, "config.toml"), []byte(config), 0o600))
			}
			var ref string
			var issueID int64
			for _, operation := range []string{"create", "edit", "comment", "label"} {
				var command []string
				var wantMutation string
				issuePath := fmt.Sprintf("/api/v1/projects/%d/issues", project.ID)
				switch operation {
				case "create":
					command = []string{"create", "Example task", "--body", "Original description"}
					wantMutation = "POST " + issuePath
				case "edit":
					command = []string{"edit", ref, "--body", "Updated description"}
					wantMutation = "PATCH " + issuePath + "/" + ref
				case "comment":
					command = []string{"comment", ref, "--body", "Example comment"}
					wantMutation = "POST " + issuePath + "/" + ref + "/comments"
				case "label":
					command = []string{"label", "add", ref, "example-label"}
					wantMutation = "POST " + issuePath + "/" + ref + "/labels"
				}
				mu.Lock()
				paths = nil
				mu.Unlock()
				stdout, stderr, err := executeRootCapture(t, t.Context(), append(command, args...)...)
				require.NoError(t, err, "%s: %s", operation, stderr)
				mu.Lock()
				assert.Equal(t, []string{"POST /api/v1/projects/resolve", wantMutation}, paths, operation)
				mu.Unlock()
				if operation == "create" {
					var response struct {
						Issue db.Issue `json:"issue"`
					}
					require.NoError(t, json.Unmarshal([]byte(stdout), &response))
					ref, issueID = response.Issue.ShortID, response.Issue.ID
					require.NotEmpty(t, ref)
				}
			}
			issue, err := env.DB.IssueByID(t.Context(), issueID)
			require.NoError(t, err)
			assert.Equal(t, "Updated description", issue.Body)
			// Read through the CLI as well so the daemon's actual comment and
			// label projections, not just successful status codes, are checked.
			stdout, stderr, err := executeRootCapture(t, t.Context(), append([]string{"show", ref}, args...)...)
			require.NoError(t, err, stderr)
			assert.Contains(t, stdout, "Example comment")
			assert.Contains(t, stdout, "example-label")
		})
	}
}
