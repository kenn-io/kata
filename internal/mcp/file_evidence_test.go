package mcpserver

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/fileevidence"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestFileEvidenceMCPConversion(t *testing.T) {
	input := Evidence{Type: "file", File: &fileevidence.Reference{Source: "notes", Path: "report.md"}}
	got, err := (toolHandlers{}).convertEvidence(t.Context(), ProjectIdentity{}, input)
	require.NoError(t, err)
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"file"`)
	require.Contains(t, string(raw), `"report.md"`)
	input.File.Path = "../report.md"
	_, err = (toolHandlers{}).convertEvidence(t.Context(), ProjectIdentity{}, input)
	require.Error(t, err)
}

func TestFileEvidenceMCPUsesLongOperationTransport(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "empty.md"), nil, 0600))
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "fixture.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Author: "worker", Title: "File proof"})
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: store, FileEvidence: fileevidence.Config{Sources: map[string]fileevidence.Source{"notes": {Root: root, SHA256: true}}}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/file-evidence") || strings.HasSuffix(r.URL.Path, "/actions/close") {
			time.Sleep(300 * time.Millisecond)
		}
		server.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	shortHTTP := *httpServer.Client()
	shortHTTP.Timeout = 100 * time.Millisecond
	short, err := kataclient.NewWithHTTPClient(httpServer.URL, &shortHTTP)
	require.NoError(t, err)
	long, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	session := connectTestServerWithOptionsAsProvided(t, Options{Client: short, LongRunningClient: long, ProjectID: project.ID, ProjectName: project.Name, Actor: "worker", Version: "test-version"})
	closed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.close", Arguments: map[string]any{"ref": issue.ShortID, "reason": "done", "message": "Completed the requested change and retained its full file evidence.", "evidence": []any{map[string]any{"type": "file", "file": map[string]any{"source": "notes", "path": "empty.md"}}}}})
	require.NoError(t, err)
	require.False(t, closed.IsError, closed.Content)
	read, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.file_evidence", Arguments: map[string]any{"ref": issue.ShortID, "content": true}})
	require.NoError(t, err)
	require.False(t, read.IsError, read.Content)
	raw, err := json.Marshal(read.StructuredContent)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"content":""`)
	require.Contains(t, string(raw), `"unchanged"`)
	shown, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.show", Arguments: map[string]any{"ref": issue.ShortID}})
	require.NoError(t, err)
	require.False(t, shown.IsError, shown.Content)
	raw, err = json.Marshal(shown.StructuredContent)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"sha256"`)
	require.Contains(t, string(raw), `"close_event_uid"`)
}
