package daemon_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// R2: membership permits ordinary work, never permanent purge or project administration.
func TestProjectAccessMemberAdministration(t *testing.T) {
	for _, action := range []string{"purge", "rename", "archive"} {
		t.Run(action, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				f := newProjectAccessFixture(t, store)
				method := http.MethodPatch
				path := fmt.Sprintf("/api/v1/projects/%d", f.private.ID)
				var payload any = map[string]string{"name": "renamed-project", "actor": "member"}
				headers := map[string]string{}
				switch action {
				case "purge":
					method = http.MethodPost
					path = fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/purge", f.private.ID, f.issue.ShortID)
					payload = map[string]string{"actor": "member"}
					headers["X-Kata-Confirm"] = "PURGE " + f.private.Name + "#" + f.issue.ShortID
				case "archive":
					method = http.MethodDelete
					path += "?actor=member&force=true"
					payload = nil
				}
				status, _, body := f.request(t, method, path, "member", payload, headers)
				assert.Equal(t, http.StatusNotFound, status, "member must not gain admin authority: %s", body)
				issue, err := store.IssueByUID(t.Context(), f.issue.UID, db.IncludeDeletedYes)
				require.NoError(t, err, "denial must preserve the issue")
				require.Equal(t, f.issue.UID, issue.UID)
				project, err := store.ProjectByID(t.Context(), f.private.ID)
				require.NoError(t, err)
				require.Equal(t, f.private.Name, project.Name)
				require.Nil(t, project.DeletedAt)

				// A configured static owner credential retains its established
				// administration contract; identity bootstrap stays non-writable.
				server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "bootstrap-test-token"}})
				t.Cleanup(func() { require.NoError(t, server.Close()) })
				ownerHTTP := httptest.NewServer(server.Handler())
				t.Cleanup(ownerHTTP.Close)
				f.server = ownerHTTP
				if payload != nil {
					fields := payload.(map[string]string)
					fields["actor"] = "admin"
				}
				if action == "archive" {
					path = fmt.Sprintf("/api/v1/projects/%d?actor=admin&force=true", f.private.ID)
				}
				status, _, body = f.request(t, method, path, "admin", payload, headers)
				require.Equal(t, http.StatusOK, status, "real owner must retain admin authority: %s", body)
			})
		})
	}
}
