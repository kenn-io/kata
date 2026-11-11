package daemon_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
)

func TestPostgresAttachmentWriteDuringFederationEnable(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL fixture requires a container")
	}
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	for _, operation := range []string{"upload", "link"} {
		t.Run(operation, func(t *testing.T) {
			vault := docbanktest.New(t)
			seedLinkFile(vault)
			project, err := store.CreateProject(t.Context(), "project-"+operation)
			require.NoError(t, err)
			issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "test issue", Author: "operator"})
			require.NoError(t, err)
			var once sync.Once
			enabled := make(chan error, 1)
			vault.BeforeResponse = func(method, path string) {
				if operation == "upload" && method == http.MethodPost && path == "/api/v1/uploads" || operation == "link" && method == http.MethodGet && path == "/api/v1/path" {
					once.Do(func() {
						_, err := store.EnableProjectFederation(t.Context(), project.ID, "operator")
						enabled <- err
					})
				}
			}
			server := daemon.NewServer(daemon.ServerConfig{DB: store, Docbank: docbank.NewClient(vault.Server.URL, func() (string, error) { return "test-key", nil }), DocbankFolder: "/kata"})
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			httpServer := httptest.NewServer(server.Handler())
			t.Cleanup(httpServer.Close)
			endpoint := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/attachments", httpServer.URL, project.ID, issue.UID)
			body, contentType := "fixture", "application/octet-stream"
			if operation == "upload" {
				endpoint += "?name=file.pdf&actor=operator"
			} else {
				endpoint += "/links"
				body, contentType = `{"path":"/reports/report.pdf","actor":"operator"}`, "application/json"
			}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader(body))
			require.NoError(t, err)
			request.Header.Set("Content-Type", contentType)
			response, err := httpServer.Client().Do(request)
			require.NoError(t, err)
			raw, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.NoError(t, <-enabled)
			require.Equal(t, http.StatusConflict, response.StatusCode, string(raw))
			require.Contains(t, string(raw), "attachment_route_changed")
			stored, err := store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.NotContains(t, stored.Metadata, "attachment.")
		})
	}
}
