package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/fileevidence"
	"go.kenn.io/kata/internal/testenv"
)

func TestRestatedStatusClearsDefaultFileEvidence(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "report.md"), []byte("proof"), 0600))
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.FileEvidence = fileevidence.Config{Sources: map[string]fileevidence.Source{"notes": {Root: root}}}
	})
	pid := initLocalWorkspace(t, env, "example-workspace")
	id := createIssueViaHTTP(t, env, pid, "Restated file proof")
	body := map[string]any{"actor": "worker", "reason": "done", "message": "Completed the requested change and retained its full file proof.", "evidence": []any{map[string]any{"type": "file", "file": map[string]any{"source": "notes", "path": "report.md"}}}}
	resp, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, id, "actions/close"), body, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	issue, err := env.DB.IssueByID(t.Context(), id)
	require.NoError(t, err)
	project, err := env.DB.ProjectByID(t.Context(), pid)
	require.NoError(t, err)
	closes, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{IssueUID: issue.UID, Types: []string{"issue.closed"}, Limit: 10})
	require.NoError(t, err)
	require.Len(t, closes, 1)
	for i, status := range []string{"open", "closed"} {
		insertRemoteIssueHistoryEvent(t, env, project, issue, fmt.Sprintf("%026d", i+1), "7ZZZZZZZZZZZZZZZZZZZZZZZZZ", db.RepeatedImportEventType, closes[0].HLCPhysicalMS+int64(i)+1, 0, fmt.Sprintf(`{"title":"Restated file proof","status":%q}`, status), closes[0].CreatedAt.Add(time.Duration(i+1)*time.Millisecond))
	}
	resp, raw = envDoRaw(t, env, http.MethodGet, issuePath(pid, id, ""), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var shown struct {
		CloseEventUID string        `json:"close_event_uid"`
		CloseEvidence []db.Evidence `json:"close_evidence"`
	}
	require.NoError(t, json.Unmarshal(raw, &shown))
	require.Empty(t, shown.CloseEventUID, "restatement transitions do not inherit an older close receipt")
	require.Empty(t, shown.CloseEvidence)
	resp, raw = envDoRaw(t, env, http.MethodGet, issuePath(pid, id, "file-evidence"), nil, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, string(raw))
	resp, raw = envDoRaw(t, env, http.MethodGet, issuePath(pid, id, "file-evidence")+"?event_uid="+closes[0].UID, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "explicit historical close evidence remains accessible")
}

func TestScopedPulledFileEvidence(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store, "file-pulled")
		issue := f.visible
		expiresAt := time.Now().UTC().Add(time.Hour)
		_, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "file-history-token", Actor: "member", AdminActor: db.BootstrapActor, Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.public.UID, RootIssueUID: issue.UID}, ExpiresAt: &expiresAt})
		require.NoError(t, err)
		_, closed, _, err := store.CloseIssueWithEvents(t.Context(), issue.ID, "done", "member", "Closed before receiving a replicated file receipt.", nil)
		require.NoError(t, err)
		require.Len(t, closed, 1)
		remote := db.RemoteEvent{EventUID: "00000000000000000000000001", OriginInstanceUID: "7ZZZZZZZZZZZZZZZZZZZZZZZZZ", ProjectUID: f.public.UID, ProjectName: f.public.Name, IssueUID: &issue.UID, Type: "issue.closed", Actor: "member", HLCPhysicalMS: closed[0].HLCPhysicalMS + 1, CreatedAt: closed[0].CreatedAt}
		remote.Payload = []byte(`{"reason":"done","evidence":[{"type":"file","file":{"source":"notes","path":"report.md","origin":"7ZZZZZZZZZZZZZZZZZZZZZZZZZ","size":0}}]}`)
		remote.ContentHash, err = db.RemoteEventContentHash(remote)
		require.NoError(t, err)
		inserted, err := store.InsertRemoteEvent(t.Context(), f.public.ID, remote)
		require.NoError(t, err)
		require.True(t, inserted)
		base := scopedProjectPath(f.public.ID, "issues/"+issue.UID)
		for _, suffix := range []string{"", "/file-evidence", "/file-evidence?event_uid=" + remote.EventUID} {
			status, _, raw := f.request(t, http.MethodGet, base+suffix, "", nil, map[string]string{"Authorization": "Bearer file-history-token"})
			require.Equal(t, http.StatusOK, status, string(raw))
			require.Contains(t, string(raw), remote.EventUID)
			require.Contains(t, string(raw), "report.md")
			if suffix != "" {
				require.Contains(t, string(raw), "foreign-origin")
			}
		}
	})
}
