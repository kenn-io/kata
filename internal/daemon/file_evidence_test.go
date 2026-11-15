package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/projectscope"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/fileevidence"
	"go.kenn.io/kata/internal/testenv"
)

func TestFileEvidenceCloseLifecycle(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "report.md"), []byte("complete\r\n"), 0600))
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.FileEvidence = fileevidence.Config{Sources: map[string]fileevidence.Source{"notes": {Root: root, SHA256: true}}}
	})
	pid := initLocalWorkspace(t, env, "example-workspace")
	id := createIssueViaHTTP(t, env, pid, "File proof")
	evidence := []map[string]any{{"type": "file", "file": map[string]any{"source": "notes", "path": "missing.md"}}}
	body := map[string]any{"actor": "worker", "reason": "done", "message": "Completed the requested change and retained its file evidence.", "evidence": evidence}
	closePath := issuePath(pid, id, "actions/close")
	resp, raw := envDoRaw(t, env, http.MethodPost, closePath, body, nil)
	require.Equal(t, 400, resp.StatusCode, string(raw))
	issue, err := env.DB.IssueByID(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "open", issue.Status)
	evidence[0]["file"] = map[string]any{"source": "notes", "path": "report.md"}
	body["dry_run"] = true
	resp, raw = envDoRaw(t, env, http.MethodPost, closePath, body, nil)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	issue, err = env.DB.IssueByID(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "open", issue.Status)
	body["dry_run"] = false
	body["retry_protocol"] = "close-v1"
	headers := map[string]string{"Idempotency-Key": "file-close"}
	resp, raw = envDoRaw(t, env, http.MethodPost, closePath, body, headers)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "sha256")
	var receipt struct {
		Event struct {
			UID string `json:"uid"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(raw, &receipt))
	showPath := issuePath(pid, id, "")
	resp, shown := envDoRaw(t, env, http.MethodGet, showPath, nil, nil)
	require.Equal(t, 200, resp.StatusCode, string(shown))
	require.Contains(t, string(shown), `"file"`)
	readPath := issuePath(pid, id, "file-evidence")
	resp, raw = envDoRaw(t, env, http.MethodGet, readPath+"?content=true", nil, nil)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), `"unchanged"`)
	require.Contains(t, string(raw), `"content"`)
	require.NoError(t, os.WriteFile(filepath.Join(root, "report.md"), []byte("replacement"), 0600))
	resp, raw = envDoRaw(t, env, http.MethodGet, readPath, nil, nil)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), `"changed"`)
	resp, raw = envDoRaw(t, env, http.MethodGet, readPath+"?historical=true", nil, nil)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), `"history-unavailable"`)
	resp, raw = envDoRaw(t, env, http.MethodGet, "/api/v1/audit/closes?project_id="+strconv.FormatInt(pid, 10), nil, nil)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "sha256")
	require.NoError(t, os.Remove(filepath.Join(root, "report.md")))
	resp, raw = envDoRaw(t, env, http.MethodPost, closePath, body, headers)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "sha256")
	other := createIssueViaHTTP(t, env, pid, "Other issue")
	resp, raw = envDoRaw(t, env, http.MethodGet, issuePath(pid, other, "file-evidence")+"?event_uid="+receipt.Event.UID, nil, nil)
	require.Equal(t, 404, resp.StatusCode, string(raw))
}

// A source read may outlive issue movement; the response must use current authority.
func TestFileEvidenceRechecksProjectScopeAfterIO(t *testing.T) {
	barrier := t.TempDir()
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.FileEvidence = fileevidence.Config{Sources: map[string]fileevidence.Source{"notes": {Command: []string{os.Args[0], "-test.run=^TestFileReadBarrierResolver$", "--", barrier}}}}
	})
	pid := initLocalWorkspace(t, env, "example-workspace")
	id := createIssueViaHTTP(t, env, pid, "File proof")
	source, err := env.DB.ProjectByID(t.Context(), pid)
	require.NoError(t, err)
	destination, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	n := int64(7)
	closed, _, _, err := env.DB.CloseIssueGuarded(t.Context(), db.CloseIssueParams{IssueID: id, Reason: "done", Actor: "worker", Evidence: []db.Evidence{{Type: "file", File: &fileevidence.Reference{Source: "notes", Path: "report.md", Origin: env.DB.InstanceUID(), Size: &n}}}})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, env.URL+issuePath(pid, id, "file-evidence")+"?content=true", nil)
	require.NoError(t, err)
	request.Header.Set(projectscope.Header, `["`+source.UID+`"]`)
	type outcome struct {
		status int
		body   []byte
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		response, err := env.HTTP.Do(request)
		if err != nil {
			done <- outcome{err: err}
			return
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		done <- outcome{status: response.StatusCode, body: raw, err: err}
	}()
	defer os.WriteFile(filepath.Join(barrier, "release"), nil, 0600)
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(barrier, "started")); return err == nil }, 3*time.Second, time.Millisecond)
	_, err = env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: id, FromProjectID: pid, ToProjectID: destination.ID, IfMatchRev: closed.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(barrier, "release"), nil, 0600))
	result := <-done
	require.NoError(t, result.err)
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, result.status, string(result.body))
	require.NotContains(t, string(result.body), "Zml4dHVyZQ==")
}

func TestFileReadBarrierResolver(t *testing.T) {
	marker := -1
	for i, arg := range os.Args {
		if arg == "--" {
			marker = i
			break
		}
	}
	if marker < 0 || marker+1 >= len(os.Args) {
		return
	}
	root := os.Args[marker+1]
	var request struct {
		Version int    `json:"version"`
		Source  string `json:"source"`
		Path    string `json:"path"`
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil || json.Unmarshal(raw, &request) != nil {
		os.Exit(2)
	}
	if os.WriteFile(filepath.Join(root, "started"), nil, 0600) != nil {
		os.Exit(2)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "release")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(2)
		}
		time.Sleep(time.Millisecond)
	}
	response := map[string]any{"version": 1, "source": request.Source, "path": request.Path, "status": "ok", "content_base64": "Zml4dHVyZQ=="}
	encoded, _ := json.Marshal(response)
	fmt.Print(string(encoded))
	os.Exit(0)
}

func TestFileEvidenceDoesNotReplaceConfirmedClaim(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "report.md"), []byte("fixture"), 0600))
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.FileEvidence = fileevidence.Config{Sources: map[string]fileevidence.Source{"notes": {Root: root, SHA256: true}}}
	})
	pid := initLocalWorkspace(t, env, "example-workspace")
	id := createIssueViaHTTP(t, env, pid, "Reviewed file proof")
	_, err := env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: id, Actor: "worker", Patch: map[string]jsontext.Value{"verify.required": jsontext.Value("true")}})
	require.NoError(t, err)
	file := map[string]any{"type": "file", "file": map[string]any{"source": "notes", "path": "report.md"}}
	body := map[string]any{"actor": "worker", "reason": "done", "message": "Completed the requested change and retained independently reviewed file evidence.", "evidence": []any{file}}
	response, raw := envDoRaw(t, env, http.MethodPost, issuePath(pid, id, "actions/close"), body, nil)
	require.Equal(t, 409, response.StatusCode, string(raw))
	require.Contains(t, string(raw), "confirmation_required")
	claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Body: "Completed the requested change."})
	require.NoError(t, err)
	_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "reviewer", Body: "Reviewed the completed work independently.", ReplyToUID: claim.UID, ReplyKind: "confirm"})
	require.NoError(t, err)
	body["evidence"] = []any{map[string]any{"type": "confirmed", "comment": claim.UID}, file}
	response, raw = envDoRaw(t, env, http.MethodPost, issuePath(pid, id, "actions/close"), body, nil)
	require.Equal(t, 200, response.StatusCode, string(raw))
	response, raw = envDoRaw(t, env, http.MethodGet, issuePath(pid, id, "file-evidence")+"?index=0", nil, nil)
	require.Equal(t, 200, response.StatusCode, string(raw))
	require.Contains(t, string(raw), `"unchanged"`)
}
