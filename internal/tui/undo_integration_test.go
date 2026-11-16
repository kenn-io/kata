package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCommentFormPreventsUndoingEarlierEdit(t *testing.T) {
	for _, kind := range []string{"plain", "reply"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			env := testenv.New(t)
			base := NewClient(env.URL, env.HTTP)
			project, err := base.EnsureProject(ctx, "example:project", "alice")
			require.NoError(t, err)
			issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: project.ID, Title: "Example", Body: "before", Author: "alice",
			})
			require.NoError(t, err)
			_, err = base.AddComment(ctx, project.ID, issue.UID, "Original finding", "alice")
			require.NoError(t, err)
			comments, err := base.ListComments(ctx, project.ID, issue.UID)
			require.NoError(t, err)
			require.Len(t, comments, 1)

			client := newConnectedUndoClient(t, base)
			m := initialModel(Options{})
			m.api = client
			m.view = viewDetail
			m.list.actor = "alice"
			m.detail = detailModel{scopePID: project.ID, activeTab: tabComments,
				issue: &Issue{UID: issue.UID, ShortID: issue.ShortID, Status: "open"}, comments: comments}
			edited, err := client.EditBody(ctx, project.ID, issue.UID, "after", "alice", nil)
			require.NoError(t, err)
			updated, _ := m.Update(mutationDoneMsg{resp: edited})
			m = updated.(Model)
			require.Len(t, m.undoHistory.entries, 1)
			if kind == "plain" {
				m = m.openCommentForm()
			} else {
				m = m.openTypedReplyForm(kind)
			}
			m.input.fields[0].area.SetValue("Follow-up finding")
			m, cmd := m.commitFormInput(inputCommentForm)
			require.NotNil(t, cmd)
			result := cmd().(mutationDoneMsg)
			require.NoError(t, result.err)
			updated, _ = m.Update(result)
			m = updated.(Model)
			require.Equal(t, inputNone, m.input.kind)
			comments, err = base.ListComments(ctx, project.ID, issue.UID)
			require.NoError(t, err)
			require.Len(t, comments, 2)
			require.Equal(t, "Follow-up finding", comments[1].Body)
			if kind == "reply" {
				require.NotNil(t, comments[1].Reply)
				require.Equal(t, comments[0].UID, comments[1].Reply.UID)
			}

			updated, cmd = m.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
			m = updated.(Model)
			require.NotNil(t, cmd)
			if m.undoInFlight {
				updated, _ = m.Update(cmd())
				m = updated.(Model)
			}
			current, err := env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, "after", current.Body, "Undo must stop at the newly added comment")
			require.Contains(t, m.toast.text, "nothing to undo: comment addition cannot be undone")
		})
	}
}

func TestTypedReplyPreflightFailurePreservesUndo(t *testing.T) {
	for _, failure := range []string{"old daemon", "health transport", "health server", "comment transport"} {
		t.Run(failure, func(t *testing.T) {
			ctx := t.Context()
			env := testenv.New(t)
			project, err := env.DB.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: project.ID, Title: "Example", Body: "before", Author: "alice",
			})
			require.NoError(t, err)
			transport := env.HTTP.Transport
			if transport == nil {
				transport = http.DefaultTransport
			}
			var commentPosts atomic.Int32
			// Only the reply's preflight sees the broken health probe; the
			// later undo is a guarded save and needs a readable version.
			var healthBroken atomic.Bool
			transportErr := errors.New("request transport failed")
			hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/v1/health" && (failure == "old daemon" || healthBroken.Load()) {
					switch failure {
					case "old daemon":
						return &http.Response{StatusCode: http.StatusOK, Request: r,
							Header: http.Header{"Content-Type": {"application/json"}},
							Body:   io.NopCloser(strings.NewReader(`{"ok":true,"api_schema_version":"0.27.0"}`))}, nil
					case "health transport":
						return nil, transportErr
					case "health server":
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Request: r,
							Header: http.Header{"Content-Type": {"application/json"}},
							Body:   io.NopCloser(strings.NewReader(`{"error":{"code":"unavailable","message":"health unavailable"}}`))}, nil
					}
				}
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
					commentPosts.Add(1)
					return nil, transportErr
				}
				return transport.RoundTrip(r)
			})}
			client := newConnectedUndoClient(t, NewClient(env.URL, hc))
			m := initialModel(Options{})
			m.api = client
			m.view = viewList
			edited, err := client.EditBody(ctx, project.ID, issue.UID, "after", "alice", nil)
			require.NoError(t, err)
			updated, _ := m.Update(mutationDoneMsg{resp: edited})
			m = updated.(Model)
			healthBroken.Store(true)
			response, err := client.AddCommentReply(ctx, project.ID, issue.UID, "Reply", "alice", "c:abcdef", "reply", false, "reply-key")
			healthBroken.Store(false)
			require.Error(t, err)
			if failure == "health transport" || failure == "comment transport" {
				require.ErrorIs(t, err, transportErr)
			}
			updated, _ = m.Update(mutationDoneMsg{resp: response, err: err})
			m = updated.(Model)
			updated, cmd := m.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
			m = updated.(Model)
			require.NotNil(t, cmd)
			if m.undoInFlight {
				updated, _ = m.Update(cmd())
				m = updated.(Model)
			}
			current, err := env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			if failure == "comment transport" {
				require.Equal(t, int32(1), commentPosts.Load())
				require.Equal(t, "after", current.Body)
				require.Contains(t, m.toast.text, "previous action's outcome is unknown")
			} else {
				require.Zero(t, commentPosts.Load(), "the reply failed before its write request")
				require.Equal(t, "before", current.Body, "Undo must still restore the earlier edit")
				require.Contains(t, m.toast.text, "undid body.edit")
			}
		})
	}
}

func TestUndoClientRealDaemonCloseAndParentLink(t *testing.T) {
	ctx := t.Context()
	env := testenv.New(t)
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	parent, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Parent", Author: "alice"})
	require.NoError(t, err)
	child, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Child", Author: "alice"})
	require.NoError(t, err)
	client := newConnectedUndoClient(t, NewClient(env.URL, env.HTTP))

	closed, err := client.Close(ctx, project.ID, child.UID, "alice")
	require.NoError(t, err)
	require.NotNil(t, closed.undo.entry)
	closed.undo.complete()
	closeOutcome := client.undo(ctx, *closed.undo.entry, undoOptions{})
	require.NoError(t, closeOutcome.err)
	require.True(t, closeOutcome.changed)
	closeOutcome.attempt.complete()
	reopened, err := env.DB.IssueByUID(ctx, child.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "open", reopened.Status)

	linked, err := client.AddLink(ctx, project.ID, child.UID, LinkBody{Type: "parent", ToRef: parent.UID}, "alice")
	require.NoError(t, err)
	require.NotNil(t, linked.undo.entry)
	require.NotNil(t, linked.undo.entry.link)
	require.Positive(t, linked.undo.entry.link.ID)
	linked.undo.complete()
	linkOutcome := client.undo(ctx, *linked.undo.entry, undoOptions{})
	require.NoError(t, linkOutcome.err)
	require.True(t, linkOutcome.changed)
	linkOutcome.attempt.complete()
	links, err := client.ListLinks(ctx, project.ID, child.UID)
	require.NoError(t, err)
	require.Empty(t, links)
}

func TestUndoClientRealDaemonScopedReopenRequiresEvidence(t *testing.T) {
	ctx := t.Context()
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Assigned work", Author: "alice"})
	require.NoError(t, err)
	_, _, _, err = env.DB.CloseIssue(ctx, issue.ID, "done", "alice", "", nil)
	require.NoError(t, err)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(ctx, db.CreateAPITokenParams{ //nolint:gosec // Deterministic test credential.
		PlaintextToken: "scoped-undo-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope:     &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: issue.UID},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	transport := env.HTTP.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.Header.Set("Authorization", "Bearer scoped-undo-token")
		return transport.RoundTrip(clone)
	})}
	client := newConnectedUndoClient(t, NewClient(env.URL, hc))
	reopened, err := client.Reopen(ctx, project.ID, issue.UID, "worker-a")
	require.NoError(t, err)
	require.NotNil(t, reopened.undo.entry)
	reopened.undo.complete()
	entry := *reopened.undo.entry
	preflight := client.undo(ctx, entry, undoOptions{evidenceRequired: true})
	require.True(t, preflight.needsEvidence)
	require.False(t, preflight.changed)
	preflight.attempt.complete()
	outcome := client.undo(ctx, entry, undoOptions{evidenceRequired: true, evidence: &CloseInput{Actor: "worker-a", Reason: "done",
		Message:  "Completed the assigned work and verified it.",
		Evidence: []api.Evidence{{Type: api.EvidenceTest, Command: "go test ./internal/tui"}},
	}})
	require.NoError(t, outcome.err)
	require.True(t, outcome.changed)
	outcome.attempt.complete()
	closed, err := env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "closed", closed.Status)
}

func TestUndoClientRealDaemonBodyConflictWithoutRevisionChange(t *testing.T) {
	ctx := t.Context()
	env := testenv.New(t)
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Example", Body: "before", Author: "alice"})
	require.NoError(t, err)
	base := NewClient(env.URL, env.HTTP)
	client := newConnectedUndoClient(t, base)
	edited, err := client.EditBody(ctx, project.ID, issue.UID, "after", "alice", nil)
	require.NoError(t, err)
	require.NotNil(t, edited.undo.entry)
	entry := *edited.undo.entry
	edited.undo.complete()
	external, err := base.EditBody(ctx, project.ID, issue.UID, "someone else's text", "bob", nil)
	require.NoError(t, err)
	require.Equal(t, entry.revision, external.Issue.Revision)
	outcome := client.undo(ctx, entry, undoOptions{})
	require.Contains(t, outcome.conflict, "body")
	require.False(t, outcome.changed)
	outcome.attempt.complete()
	current, err := env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "someone else's text", current.Body)
}

func TestUndoClientRealDaemonDetectsStatusChangedAwayAndBack(t *testing.T) {
	ctx := t.Context()
	env := testenv.New(t)
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Example", Author: "alice",
	})
	require.NoError(t, err)
	base := NewClient(env.URL, env.HTTP)
	client := newConnectedUndoClient(t, base)
	closed, err := client.Close(ctx, project.ID, issue.UID, "alice")
	require.NoError(t, err)
	entry := *closed.undo.entry
	closed.undo.complete()
	_, err = base.Reopen(ctx, project.ID, issue.UID, "bob")
	require.NoError(t, err)
	reclosed, err := base.Close(ctx, project.ID, issue.UID, "bob")
	require.NoError(t, err)
	require.Equal(t, entry.after.Status, reclosed.Issue.Status)
	require.Equal(t, entry.after.ClosedReason, reclosed.Issue.ClosedReason)
	outcome := client.undo(ctx, entry, undoOptions{})
	defer outcome.attempt.complete()
	require.NotEmpty(t, outcome.conflict)
	require.False(t, outcome.changed)
	current, err := env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "closed", current.Status)
}

// A body save made against the content revision the editor opened at is
// refused once another client has edited the body, writes nothing, and a
// save that names no revision still goes through.
func TestClientEditBodyRefusesStaleContentRevision(t *testing.T) {
	ctx := t.Context()
	env := testenv.New(t)
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Example", Body: "before", Author: "alice"})
	require.NoError(t, err)
	client := NewClient(env.URL, env.HTTP)

	opened, err := client.GetIssueDetail(ctx, project.ID, issue.UID)
	require.NoError(t, err)
	revision := opened.Issue.ContentRevision

	_, err = client.EditBody(ctx, project.ID, issue.UID, "other client's text", "bob", &revision)
	require.NoError(t, err)

	_, err = client.EditBody(ctx, project.ID, issue.UID, "stale editor's text", "alice", &revision)
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusPreconditionFailed, apiErr.Status)
	require.Equal(t, "revision_conflict", apiErr.Code)
	current, err := env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "other client's text", current.Body)

	resp, err := client.EditBody(ctx, project.ID, issue.UID, "unguarded text", "alice", nil)
	require.NoError(t, err)
	require.True(t, resp.Changed)
	current, err = env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "unguarded text", current.Body)
}

// Against a daemon older than 0.52.0 the body save carries no
// expected_content_revision (the daemon would reject the unknown field) and
// succeeds as it did before the field existed.
func TestClientEditBodyOmitsContentRevisionForOlderDaemon(t *testing.T) {
	revision := int64(3)
	for _, tc := range []struct {
		version string
		sent    bool
	}{{"0.51.0", false}, {"0.52.0", true}} {
		t.Run(tc.version, func(t *testing.T) {
			var raw map[string]any
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					respondJSON(t, w, map[string]any{"ok": true, "api_schema_version": tc.version})
					return
				}
				require.Equal(t, http.MethodPatch, r.Method)
				require.NoError(t, json.UnmarshalRead(r.Body, &raw))
				respondJSON(t, w, map[string]any{"issue": map[string]any{"uid": "u"}, "changed": true})
			})
			resp, err := c.EditBody(t.Context(), 7, "abc4", "new body", "alice", &revision)
			require.NoError(t, err)
			require.True(t, resp.Changed)
			require.Equal(t, "new body", raw["body"])
			_, sent := raw["expected_content_revision"]
			require.Equal(t, tc.sent, sent)
		})
	}
}

// healthDownClient returns a client whose daemon answers the health probe
// with status and fails the test when a PATCH reaches it.
func healthDownClient(t *testing.T, healthBody map[string]any, status int) *Client {
	t.Helper()
	return newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			w.WriteHeader(status)
			if healthBody != nil {
				respondJSON(t, w, healthBody)
			}
			return
		}
		t.Errorf("unexpected %s %s: no write may follow a failed version check", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
}

// When the daemon's version cannot be read, a guarded body save fails with
// that error instead of going out without the guard.
func TestClientEditBodyFailsWhenVersionUnknown(t *testing.T) {
	revision := int64(3)
	for name, c := range map[string]*Client{
		"health fails":      healthDownClient(t, nil, http.StatusInternalServerError),
		"no schema version": healthDownClient(t, map[string]any{"ok": true}, http.StatusOK),
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := c.EditBody(t.Context(), 7, "abc4", "new body", "alice", &revision)
			require.Error(t, err)
			require.Nil(t, resp)
			require.Contains(t, err.Error(), "daemon API "+contentRevisionAPIVersion)
		})
	}
}

// The body editor's save surfaces the failed version check and sends no write.
func TestFormBodySaveFailsWhenVersionUnknown(t *testing.T) {
	c := healthDownClient(t, nil, http.StatusInternalServerError)
	msg := dispatchFormEditBody(c, formTarget{projectID: 7, issueShortID: "abc4", contentRevision: 3}, "new body", "alice", 1)().(mutationDoneMsg)
	require.Error(t, msg.err)
	require.Nil(t, msg.resp)
}

// healthDownUndoAPI routes the body write through a real client so the undo
// meets the version check.
type healthDownUndoAPI struct {
	*undoTestAPI
	client *Client
}

func (h *healthDownUndoAPI) EditBody(ctx context.Context, projectID int64, ref, body, actor string, expected *int64) (*MutationResp, error) {
	return h.client.EditBody(ctx, projectID, ref, body, actor, expected)
}

// A body undo whose version check fails reports the error and writes nothing.
func TestUndoClientBodyFailsWhenVersionUnknown(t *testing.T) {
	uid := "01JZ0000000000000000000001"
	current := Issue{UID: uid, ProjectID: 7, Body: "after", Revision: 4}
	entry := undoEntry{kind: "body.edit", uid: uid, projectID: 7, revision: 4, instanceUID: "instance-1", actor: "bob",
		before: Issue{Body: "before"}, after: Issue{Body: "after"}}
	api := &healthDownUndoAPI{undoTestAPI: &undoTestAPI{issue: current}, client: healthDownClient(t, nil, http.StatusInternalServerError)}
	outcome := newConnectedUndoClient(t, api).undo(t.Context(), entry, undoOptions{})
	require.Error(t, outcome.err)
	require.False(t, outcome.changed)
	require.Empty(t, outcome.conflict)
}
