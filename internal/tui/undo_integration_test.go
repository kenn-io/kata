package tui

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestUndoClientRealDaemonCloseAndParentLink(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; applyColorMode rewrites package style vars
	ctx := context.Background()
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
	closeOutcome := client.undo(ctx, *closed.undo.entry, false, nil)
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
	linkOutcome := client.undo(ctx, *linked.undo.entry, false, nil)
	require.NoError(t, linkOutcome.err)
	require.True(t, linkOutcome.changed)
	linkOutcome.attempt.complete()
	links, err := client.ListLinks(ctx, project.ID, child.UID)
	require.NoError(t, err)
	require.Empty(t, links)
}

func TestUndoClientRealDaemonScopedReopenRequiresEvidence(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; applyColorMode rewrites package style vars
	ctx := context.Background()
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
	preflight := client.undo(ctx, entry, true, nil)
	require.True(t, preflight.needsEvidence)
	require.False(t, preflight.changed)
	preflight.attempt.complete()
	outcome := client.undo(ctx, entry, true, &CloseInput{Actor: "worker-a", Reason: "done",
		Message:  "Completed the assigned work and verified it.",
		Evidence: []api.Evidence{{Type: api.EvidenceTest, Command: "go test ./internal/tui"}},
	})
	require.NoError(t, outcome.err)
	require.True(t, outcome.changed)
	outcome.attempt.complete()
	closed, err := env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "closed", closed.Status)
}

func TestUndoClientRealDaemonBodyConflictWithoutRevisionChange(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; applyColorMode rewrites package style vars
	ctx := context.Background()
	env := testenv.New(t)
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Example", Body: "before", Author: "alice"})
	require.NoError(t, err)
	base := NewClient(env.URL, env.HTTP)
	client := newConnectedUndoClient(t, base)
	edited, err := client.EditBody(ctx, project.ID, issue.UID, "after", "alice")
	require.NoError(t, err)
	require.NotNil(t, edited.undo.entry)
	entry := *edited.undo.entry
	edited.undo.complete()
	external, err := base.EditBody(ctx, project.ID, issue.UID, "someone else's text", "bob")
	require.NoError(t, err)
	require.Equal(t, entry.revision, external.Issue.Revision)
	outcome := client.undo(ctx, entry, false, nil)
	require.Contains(t, outcome.conflict, "body")
	require.False(t, outcome.changed)
	outcome.attempt.complete()
	current, err := env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "someone else's text", current.Body)
}

func TestUndoClientRealDaemonDetectsStatusChangedAwayAndBack(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; applyColorMode rewrites package style vars
	ctx := context.Background()
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
	outcome := client.undo(ctx, entry, false, nil)
	defer outcome.attempt.complete()
	require.NotEmpty(t, outcome.conflict)
	require.False(t, outcome.changed)
	current, err := env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "closed", current.Status)
}
