package daemon

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
)

func TestAttachmentUploadRejectsLoggedOutSession(t *testing.T) {
	store := openAuthTestDB(t)
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Example", Author: "operator"})
	require.NoError(t, err)
	fake := docbanktest.New(t)
	started, resume := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/uploads" {
			close(started)
			select {
			case <-resume:
			case <-r.Context().Done():
				return
			}
		}
		fake.Server.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	manager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "instance_a")
	issued, err := manager.IssueSession(Principal{Kind: PrincipalWebLocal}, "/kata")
	require.NoError(t, err)
	server := NewServer(ServerConfig{DB: store, StartedAt: time.Now(), WebSessions: manager, Docbank: docbank.NewClient(upstream.URL, func() (string, error) { return "test-key", nil }), DocbankFolder: "/kata"})
	t.Cleanup(func() { _ = server.Close() })
	handler, err := server.HandlerFor(ListenerPolicy{Kind: ListenerBrowser, Origin: "http://127.0.0.1:27123", RequireBrowserSession: true})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:27123/api/v1/projects/%d/issues/%s/attachments?name=shot.png&actor=operator", project.ID, issue.ShortID), strings.NewReader("image"))
	req.RemoteAddr = "127.0.0.1:40123"
	req.Header.Set("Origin", "http://127.0.0.1:27123")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(webSessionHeader, issued.Session)
	req.Header.Set(webCSRFHeader, issued.CSRF)
	req.AddCookie(manager.Cookie(issued.Cookie))
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handler.ServeHTTP(response, req); close(done) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not start")
	}
	manager.Logout(issued.Session)
	close(resume)
	<-done
	require.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
	current, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(current.Metadata))
}
