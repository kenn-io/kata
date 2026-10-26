package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

func executeCloseAttachmentCLI(t *testing.T, base string, args ...string) (string, error) {
	t.Helper()
	resetFlags(t)
	cmd := newRootCmd()
	var b bytes.Buffer
	cmd.SetOut(&b)
	cmd.SetErr(&b)
	cmd.SetContext(contextWithBaseURL(context.Background(), base))
	cmd.SetArgs(append([]string{"--project", "spoke-project"}, args...))
	err := cmd.Execute()
	return b.String(), err
}

const attachmentCloseMessage = "Delivered the final report and checked its complete contents carefully."

func TestCloseAttachmentCLI(t *testing.T) {
	env, f, ref := attachmentCLIEnv(t)
	seedCLILink(f)
	out, err := executeCloseAttachmentCLI(t, env.URL, "close", ref, "--done", "-m", attachmentCloseMessage, "--docbank", "42", "--docbank", "/reports/report.pdf", "--test", "document check", "--json")
	require.NoError(t, err, out)
	result := api.MutationResponse{}.Body
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.Equal(t, "closed", result.Issue.Status)
	var payload struct {
		Evidence []db.Evidence `json:"evidence"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.Event.Payload), &payload))
	require.Len(t, payload.Evidence, 2)
	require.Equal(t, "attachment", payload.Evidence[1].Type)
	require.Equal(t, "report.pdf", payload.Evidence[1].Attachment.Name)
}
func TestCloseAttachmentCLISuffixAndCanonicalReplay(t *testing.T) {
	env, f, ref := attachmentCLIEnv(t)
	seedCLILink(f)
	out, err := executeAttachmentCLI(t, env.URL, "link", ref, "42", "--json")
	require.NoError(t, err)
	var linked api.AddAttachmentResponseBody
	require.NoError(t, json.Unmarshal([]byte(out), &linked))
	out, err = executeCloseAttachmentCLI(t, env.URL, "close", ref, "--done", "-m", attachmentCloseMessage, "--evidence", "attachment:"+linked.ID[len(linked.ID)-8:], "--idempotency-key", "document-close", "--json")
	require.NoError(t, err, out)
	_, err = executeAttachmentCLI(t, env.URL, "rm", ref, linked.ID)
	require.NoError(t, err)
	out, err = executeCloseAttachmentCLI(t, env.URL, "close", ref, "--done", "-m", attachmentCloseMessage, "--evidence", "attachment:"+linked.ID, "--idempotency-key", "document-close", "--json")
	require.NoError(t, err, out)
	require.Contains(t, out, `"reused":true`)
	for _, mode := range []string{"--agent", "--quiet"} {
		out, err = executeCloseAttachmentCLI(t, env.URL, "show", ref, mode)
		require.NoError(t, err)
		require.Contains(t, out, "report.pdf")
		require.Contains(t, out, "application/pdf")
		require.Contains(t, out, "12 bytes")
		require.Contains(t, out, "removed")
	}
}
func TestCloseDocbankPreflight(t *testing.T) {
	for i, args := range [][]string{{"--done", "--dry"}, {"--done", "--if-match", "1"}, {"--done", "--idempotency-key", "key"}, {"--wontfix"}, {"--done", "--docbank", "bad"}, {"--done", "--commit", "invalid"}, {"--done", "-m", "short"}} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			env, f, ref := attachmentCLIEnv(t)
			seedCLILink(f)
			base := []string{"close", ref, "-m", attachmentCloseMessage, "--docbank", "42"}
			_, err := executeCloseAttachmentCLI(t, env.URL, append(base, args...)...)
			require.Error(t, err)
			require.Empty(t, f.Requests())
		})
	}
}
func TestCloseDocbankPartialFailureAndRetry(t *testing.T) {
	env, f, ref := attachmentCLIEnv(t)
	seedCLILink(f)
	_, err := executeCloseAttachmentCLI(t, env.URL, "close", ref, "--done", "-m", attachmentCloseMessage, "--docbank", "42", "--docbank", "99")
	require.ErrorContains(t, err, "99")
	require.ErrorContains(t, err, "already linked")
	p, e := env.DB.ProjectByName(t.Context(), "spoke-project")
	require.NoError(t, e)
	issue, e := env.DB.IssueByShortID(t.Context(), p.ID, ref, db.IncludeDeletedNo)
	require.NoError(t, e)
	require.Equal(t, "open", issue.Status)
	var refs map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(issue.Metadata), &refs))
	var linkedID string
	for key := range refs {
		if strings.HasPrefix(key, metadata.AttachmentPrefix) {
			require.Empty(t, linkedID)
			linkedID = strings.TrimPrefix(key, metadata.AttachmentPrefix)
		}
	}
	require.Len(t, linkedID, 26)
	require.ErrorContains(t, err, linkedID)
	cursor, e := env.DB.MaxEventID(t.Context())
	require.NoError(t, e)
	out, err := executeCloseAttachmentCLI(t, env.URL, "close", ref, "--done", "-m", attachmentCloseMessage, "--docbank", "42", "--json")
	require.NoError(t, err, out)
	result := api.MutationResponse{}.Body
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.Equal(t, "closed", result.Issue.Status)
	var after map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(result.Issue.Metadata), &after))
	require.Equal(t, refs, after, "retry must reuse the existing reference")
	var payload struct {
		Evidence []db.Evidence `json:"evidence"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.Event.Payload), &payload))
	require.Len(t, payload.Evidence, 1)
	require.Equal(t, linkedID, payload.Evidence[0].AttachmentID)
	next, e := env.DB.MaxEventID(t.Context())
	require.NoError(t, e)
	require.Equal(t, cursor+1, next, "retry emits only the close event")
}

func TestCloseAttachmentCLIOldDaemon(t *testing.T) {
	env, f, ref := attachmentCLIEnv(t)
	seedCLILink(f)
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.34.0"}`))
			return
		}
		if r.Method == "POST" && r.URL.Path != "/api/v1/projects/resolve" {
			writes++
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	for _, args := range [][]string{{"--docbank", "42"}, {"--evidence", "attachment:01K7E222222222222222222222"}} {
		_, err := executeCloseAttachmentCLI(t, server.URL, append([]string{"close", ref, "--done", "-m", attachmentCloseMessage}, args...)...)
		require.ErrorContains(t, err, "requires daemon API 0.35.0")
		require.Zero(t, writes)
		require.Empty(t, f.Requests())
	}
}

func TestCloseAttachmentAgentOutputKeepsEvidenceID(t *testing.T) {
	env, f, ref := attachmentCLIEnv(t)
	seedCLILink(f)
	out, err := executeCloseAttachmentCLI(t, env.URL, "close", ref, "--done", "-m", attachmentCloseMessage, "--docbank", "42", "--agent")
	require.NoError(t, err, out)
	require.Contains(t, out, "Evidence: attachment:")
}
