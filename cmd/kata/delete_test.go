package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestDelete_NoForceIsValidationError(t *testing.T) {
	f := newCLIFixture(t)
	short := createIssueViaHTTP(t, f.env, f.dir, "to be deleted")

	err := f.execute("delete", short)
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "--force")
}

func TestDelete_ForceWithConfirmSoftDeletes(t *testing.T) {
	f := newCLIFixture(t)
	short := createIssueViaHTTP(t, f.env, f.dir, "to be deleted")

	require.NoError(t, f.execute("delete", short, "--force", "--confirm", "DELETE kata#"+short))
	assert.Contains(t, f.buf.String(), "deleted")
}

func TestDelete_ReasonIsRecordedOnTheDeleteEvent(t *testing.T) {
	f := newCLIFixture(t)
	issue := createIssueViaHTTPFull(t, f.env, f.dir, "to be deleted")

	require.NoError(t, f.execute("delete", issue.ShortID, "--force", "--confirm", "DELETE kata#"+issue.ShortID,
		"--reason", "duplicate of the import task"))

	events, err := f.env.DB.EventsAfter(t.Context(), db.EventsAfterParams{
		IssueUID: issue.UID, Types: []string{"issue.soft_deleted"}, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	var payload struct {
		Reason string `json:"reason"`
	}
	require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
	assert.Equal(t, "duplicate of the import task", payload.Reason)
}

func TestDelete_ReasonRejectsOldDaemon(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	short := createIssueViaHTTP(t, env, dir, "to be deleted")
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.61.0"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/actions/delete") {
			deletes.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	old := *env
	old.URL = server.URL

	_, err = runCLICapture(t, &old, dir, "delete", short, "--force", "--confirm", "DELETE kata#"+short,
		"--reason", "duplicate of the import task")
	require.ErrorContains(t, err, "requires daemon API 0.63.0")
	assert.Zero(t, deletes.Load())
}

func TestDelete_AgentOutput(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	short := createIssueViaHTTP(t, env, dir, "to be deleted")

	out := runCLI(t, env, dir, "--agent", "delete", short, "--force", "--confirm", "DELETE kata#"+short)

	assert.Regexp(t, `(?m)^OK delete \S+`, out)
	assert.Contains(t, out, "Status: deleted")
	assert.NotContains(t, out, "Status: open")
	assert.Contains(t, out, "Undo: kata restore kata#"+short+" --agent")
}

func TestDelete_AgentUndoUsesQualifiedRef(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputAgent
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	body := []byte(`{"issue":{"short_id":"abc4","qualified_id":"other#abc4","title":"to be deleted","status":"open"},"changed":true}`)

	require.NoError(t, printDestructive(cmd, "other#abc4", "other", "delete", body))

	out := buf.String()
	assert.Contains(t, out, "Undo: kata restore other#abc4 --agent")
	assert.NotContains(t, out, "Undo: kata restore abc4 --agent")
}

func TestDelete_AgentUndoQuotesQualifiedRefForShell(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputAgent
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	body := []byte(`{"issue":{"short_id":"abc4","qualified_id":"Kata Tracker#abc4","title":"to be deleted","status":"open"},"changed":true}`)

	require.NoError(t, printDestructive(cmd, "Kata Tracker#abc4", "Kata Tracker", "delete", body))

	out := buf.String()
	assert.Contains(t, out, "Undo: kata restore 'Kata Tracker#abc4' --agent")
	assert.NotContains(t, out, "Undo: kata restore Kata Tracker#abc4 --agent")
}

func TestDelete_ConfirmMismatchIsExit6(t *testing.T) {
	f := newCLIFixture(t)
	short := createIssueViaHTTP(t, f.env, f.dir, "to be deleted")

	err := f.execute("delete", short, "--force", "--confirm", "DELETE wrong")
	ce := requireCLIError(t, err, ExitConfirm)
	assert.True(t, strings.Contains(ce.Code, "confirm_mismatch"))
}

// TestDelete_NoTTYNoConfirmIsConfirmRequired pins the resolveConfirm branch
// where stdin isn't a TTY and --confirm wasn't passed.
func TestDelete_NoTTYNoConfirmIsConfirmRequired(t *testing.T) {
	stubIsTTY(t, false)
	f := newCLIFixture(t)
	short := createIssueViaHTTP(t, f.env, f.dir, "to be deleted")

	err := f.execute("delete", short, "--force")
	ce := requireCLIError(t, err, ExitConfirm)
	assert.Equal(t, "confirm_required", ce.Code)
}
