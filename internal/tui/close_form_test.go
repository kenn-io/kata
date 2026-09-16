package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestClientCloseWithEvidenceSendsAuthenticatedCompletion(t *testing.T) {
	var got api.CloseActionRequestBody
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/projects/7/issues/abc4/actions/close", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		respondJSON(t, w, MutationResp{Changed: true})
	})

	_, err := c.CloseWithEvidence(t.Context(), 7, "abc4", CloseInput{
		Actor:   "worker-a",
		Reason:  "superseded",
		Message: "Implemented the scoped worker flow and verified its behavior.",
		Evidence: []api.Evidence{{
			Type: api.EvidenceSupersededBy, IssueRef: "next1",
		}},
	})

	require.NoError(t, err)
	assert.Equal(t, "worker-a", got.Actor)
	assert.Equal(t, "superseded", got.Reason)
	assert.Equal(t, "tui", got.Source)
	assert.Equal(t, "Implemented the scoped worker flow and verified its behavior.", got.Message)
	require.Len(t, got.Evidence, 1)
	assert.Equal(t, api.EvidenceSupersededBy, got.Evidence[0].Type)
	assert.Equal(t, "next1", got.Evidence[0].IssueRef)
}

func TestCloseInputFromFormBuildsTypedEvidence(t *testing.T) {
	s := newCloseForm(formTarget{projectID: 7, issueShortID: "abc4", origin: "detail"})
	s.field(fieldCloseReason).radio.set("audit-no-change")
	s.field(fieldCloseMessage).area.SetValue("Implemented the scoped worker flow and verified its behavior.")
	s.field(fieldEvidenceType).radio.set("no-change-audit")
	s.field(fieldEvidenceValue).input.SetValue("The existing implementation already enforces the required invariant.")

	got, err := closeInputFromForm(s, "worker-a")

	require.NoError(t, err)
	assert.Equal(t, "worker-a", got.Actor)
	assert.Equal(t, "audit-no-change", got.Reason)
	require.Len(t, got.Evidence, 1)
	assert.Equal(t, api.EvidenceNoChangeAudit, got.Evidence[0].Type)
	assert.Equal(t, "The existing implementation already enforces the required invariant.", got.Evidence[0].Rationale)
}

func TestCloseInputFromFormBuildsWontfixWithoutEvidence(t *testing.T) {
	s := newCloseForm(formTarget{projectID: 7, issueShortID: "abc4", origin: "detail"})
	s.field(fieldCloseReason).radio.set("wontfix")
	s.field(fieldCloseMessage).area.SetValue("This behavior is intentionally unsupported because it violates the authority boundary.")

	got, err := closeInputFromForm(s, "worker-a")

	require.NoError(t, err)
	assert.Equal(t, "wontfix", got.Reason)
	assert.Empty(t, got.Evidence)
}

func TestIssueScopedCloseCapabilityOpensEvidenceForm(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/instance", r.URL.Path)
		future := time.Now().UTC().Add(time.Hour)
		respondJSON(t, w, InstanceInfo{Auth: AuthInfo{
			Kind:                  "db_token",
			Scope:                 &api.TokenScopeOut{Kind: "issue_subtree", ProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4EX", RootIssueUID: "01HZNQ7VFPK1XGD8R5MABCD5YZ"},
			ExpiresAt:             &future,
			AllowedActions:        []string{"issue.read", "issue.edit", "issue.close"},
			CloseRequiresEvidence: true,
		}})
	})
	m := initialModel(Options{})
	m.api = c
	m.view = viewList
	m.scope = scope{projectID: 7}
	m.list.issues = []Issue{{ProjectID: 7, ShortID: "abc4", Status: "open"}}

	model, _ := m.Update(m.fetchAuthCapabilities()())
	m = model.(Model)
	got, cmd, handled := m.routeCloseKey(runeKey('x'))
	require.True(t, handled)
	require.Nil(t, cmd)
	assert.Empty(t, got.input.err)

	assert.Equal(t, inputCloseForm, got.input.kind)
	assert.Equal(t, "abc4", got.input.target.issueShortID)
	assert.Equal(t, "list", got.input.target.origin)
}

func TestIssueScopedCapabilitiesSuppressParentEditing(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/instance", r.URL.Path)
		future := time.Now().UTC().Add(time.Hour)
		respondJSON(t, w, InstanceInfo{Auth: AuthInfo{
			Kind: "db_token",
			Scope: &api.TokenScopeOut{
				Kind: "issue_subtree", ProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4EX", RootIssueUID: "01HZNQ7VFPK1XGD8R5MABCD5YZ",
			},
			ExpiresAt: &future, AllowedActions: []string{"issue.read", "issue.edit"},
			CloseRequiresEvidence: true,
		}})
	})
	m := initialModel(Options{})
	m.api = c
	m.view = viewDetail
	m.detail.issue = &Issue{ProjectID: 7, ShortID: "abc4", Status: "open"}

	msg := m.fetchAuthCapabilities()()
	model, _ := m.Update(msg)
	m = model.(Model)
	model, cmd := m.Update(runeKey('p'))
	m = model.(Model)

	require.Nil(t, cmd)
	assert.Equal(t, inputNone, m.input.kind)
}

func TestReadOnlyScopedCapabilitiesPreventClose(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		future := time.Now().UTC().Add(time.Hour)
		respondJSON(t, w, InstanceInfo{Auth: AuthInfo{
			Kind: "db_token",
			Scope: &api.TokenScopeOut{
				Kind: "issue_subtree", ProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4EX",
				RootIssueUID: "01HZNQ7VFPK1XGD8R5MABCD5YZ",
			},
			ExpiresAt: &future, AllowedActions: []string{"issue.read"}, CloseRequiresEvidence: true,
		}})
	})
	m := initialModel(Options{})
	m.api = c
	m.view = viewList
	m.scope = scope{projectID: 7}
	m.list.issues = []Issue{{ProjectID: 7, ShortID: "abc4", Status: "open"}}

	model, _ := m.Update(m.fetchAuthCapabilities()())
	m = model.(Model)
	model, cmd := m.Update(runeKey('x'))
	m = model.(Model)

	require.Nil(t, cmd)
	assert.Equal(t, inputNone, m.input.kind)
}

func TestUnscopedCapabilityRefreshPreservesParentPrompt(t *testing.T) {
	m := initialModel(Options{})
	m.input = inputState{kind: inputParentPrompt}

	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Kind: "local"}})

	assert.Equal(t, inputParentPrompt, m.input.kind)
}

func TestFailedCapabilityDiscoveryRetriesBeforeMutationKeys(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		respondJSON(t, w, InstanceInfo{Auth: AuthInfo{Kind: "local", AllowedActions: []string{"issue.edit"}}})
	})
	m := initialModel(Options{})
	m.api = c
	m.view = viewDetail
	m.detail.issue = &Issue{ProjectID: 7, ShortID: "abc4", Status: "open"}

	model, _ := m.Update(m.fetchAuthCapabilities()())
	m = model.(Model)
	require.NotNil(t, m.toast)
	assert.Contains(t, m.toast.text, "permissions unavailable")
	model, cmd := m.Update(runeKey('e'))
	m = model.(Model)

	require.NotNil(t, cmd)
	assert.Equal(t, inputNone, m.input.kind)
	require.NotNil(t, m.toast)
	assert.Contains(t, m.toast.text, "retry your action")
	batch, ok := cmd().(tea.BatchMsg)
	require.True(t, ok)
	model, _ = m.Update(batch[0]())
	m = model.(Model)
	model, cmd = m.Update(runeKey('e'))
	m = model.(Model)

	require.Nil(t, cmd)
	assert.Equal(t, inputBodyEditForm, m.input.kind)
	assert.EqualValues(t, 2, calls.Load())
}

func TestGetInstanceRejectsPartialScopedCapabilities(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(t, w, InstanceInfo{Auth: AuthInfo{
			Kind:  "db_token",
			Scope: &api.TokenScopeOut{Kind: "issue_subtree", ProjectUID: "01PROJECT", RootIssueUID: "01ROOT"},
		}})
	})

	_, err := c.GetInstance(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "incomplete issue-scoped capabilities")
}

func TestGetInstanceRejectsExpiredScopedCapabilities(t *testing.T) {
	expired := time.Now().UTC().Add(-time.Minute)
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(t, w, InstanceInfo{Auth: AuthInfo{
			Kind: "db_token",
			Scope: &api.TokenScopeOut{
				Kind: "issue_subtree", ProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4EX",
				RootIssueUID: "01HZNQ7VFPK1XGD8R5MABCD5YZ",
			},
			ExpiresAt: &expired, AllowedActions: []string{"issue.read"}, CloseRequiresEvidence: true,
		}})
	})

	_, err := c.GetInstance(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "incomplete issue-scoped capabilities")
}

func TestScopedTUIClientCompletesIssueWithEvidence(t *testing.T) {
	ctx := t.Context()
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	root, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Delegated work", Author: "coordinator",
	})
	require.NoError(t, err)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(ctx, db.CreateAPITokenParams{ //nolint:gosec // Deterministic test credential.
		PlaintextToken: "scoped-tui-token",
		Actor:          "worker-a",
		AdminActor:     db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)

	transport := env.HTTP.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.Header.Set("Authorization", "Bearer scoped-tui-token")
		return transport.RoundTrip(clone)
	})}
	c := NewClient(env.URL, hc)

	instance, err := c.GetInstance(ctx)
	require.NoError(t, err)
	require.True(t, instance.Auth.CloseRequiresEvidence)
	_, err = c.CloseWithEvidence(ctx, project.ID, root.ShortID, CloseInput{
		Actor:   "worker-a",
		Message: "Implemented the delegated change and verified the complete behavior.",
		Evidence: []api.Evidence{{
			Type: api.EvidenceTest, Command: "go test ./internal/tui",
		}},
	})
	require.NoError(t, err)

	closed, err := env.DB.IssueByShortID(ctx, project.ID, root.ShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, "closed", closed.Status)
}
