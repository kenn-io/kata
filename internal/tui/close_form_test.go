package tui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/testenv"
	"uuid"
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
	s := newCloseForm(formTarget{projectID: 7, issueShortID: "abc4", origin: "detail"}, nil)
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
	s := newCloseForm(formTarget{projectID: 7, issueShortID: "abc4", origin: "detail"}, nil)
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

	_, err := c.GetInstance(t.Context())

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

	_, err := c.GetInstance(t.Context())
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

const (
	reportAttachmentID = "01KATTACH0000000000000REPT"
	logAttachmentID    = "01KATTACH0000000000000LOGS"
)

func closeFormAttachments() []closeAttachment {
	return []closeAttachment{
		{id: reportAttachmentID, ref: metadata.Attachment{Name: "report.pdf", Size: 2048}},
		{id: logAttachmentID, ref: metadata.Attachment{Name: "run.log", Size: 12}},
	}
}

func TestIssueDecodesAttachmentReferencesFromMetadata(t *testing.T) {
	raw := `{"short_id":"abc4","metadata":{
		"scheduled_on":"2026-11-20",
		"attachment.` + reportAttachmentID + `":{"name":"report.pdf","media_type":"application/pdf","size":2048,"docbank_version":"6f1c2e8a-2222-4222-8222-222222222222"},
		"attachment.01KBROKEN":{"name":"no-size.txt"},
		"other.key":{"name":"ignored"}}}`
	var issue Issue
	require.NoError(t, json.Unmarshal([]byte(raw), &issue))

	assert.Equal(t, "2026-11-20", issue.scheduledOn())
	assert.Equal(t, []closeAttachment{{id: reportAttachmentID, ref: metadata.Attachment{
		Name: "report.pdf", MediaType: "application/pdf", Size: 2048,
		DocbankVersion: uuid.MustParse("6f1c2e8a-2222-4222-8222-222222222222"),
	}}}, issue.attachments())
}

func TestCloseFormWithoutAttachmentsHasNoAttachmentField(t *testing.T) {
	s := newCloseForm(formTarget{projectID: 7, issueShortID: "abc4"}, nil)
	assert.Nil(t, s.field(fieldCloseAttachment))
}

func TestCloseFormReasonKeysToggleAttachmentField(t *testing.T) {
	s := newCloseForm(formTarget{projectID: 7, issueShortID: "abc4"}, closeFormAttachments())
	require.NotNil(t, s.field(fieldCloseAttachment), "done is the default reason")
	assert.Equal(t, []string{"none", "report.pdf · 2048 bytes", "run.log · 12 bytes"}, s.field(fieldCloseAttachment).radio.choices)
	s.active = 0 // reason radio

	for _, reason := range []string{"wontfix", "duplicate", "superseded", "audit-no-change"} {
		var handled bool
		s, handled = s.handleRadioKey(tea.KeyPressMsg{Code: tea.KeyRight})
		require.True(t, handled)
		assert.Equal(t, reason, s.fieldValue(fieldCloseReason))
		assert.Nil(t, s.field(fieldCloseAttachment), reason)
	}
	for range 4 {
		s, _ = s.handleRadioKey(tea.KeyPressMsg{Code: tea.KeyLeft})
	}
	assert.Equal(t, "done", s.fieldValue(fieldCloseReason))
	assert.NotNil(t, s.field(fieldCloseAttachment))
}

func TestCloseInputFromFormCitesChosenAttachment(t *testing.T) {
	s := newCloseForm(formTarget{projectID: 7, issueShortID: "abc4"}, closeFormAttachments())
	s.field(fieldCloseMessage).area.SetValue("Delivered the report and checked its final contents.")
	s.field(fieldCloseAttachment).radio.set("run.log · 12 bytes")

	got, err := closeInputFromForm(s, "worker-a")

	require.NoError(t, err)
	assert.Equal(t, "done", got.Reason)
	assert.Equal(t, []api.Evidence{{Type: api.EvidenceAttachment, AttachmentID: logAttachmentID}}, got.Evidence)
}

func TestCloseInputFromFormCombinesAttachmentWithTypedEvidence(t *testing.T) {
	s := newCloseForm(formTarget{projectID: 7, issueShortID: "abc4"}, closeFormAttachments())
	s.field(fieldCloseMessage).area.SetValue("Delivered the report and checked its final contents.")
	s.field(fieldEvidenceType).radio.set("commit")
	s.field(fieldEvidenceValue).input.SetValue("abc1234")
	s.field(fieldCloseAttachment).radio.set("report.pdf · 2048 bytes")

	got, err := closeInputFromForm(s, "worker-a")

	require.NoError(t, err)
	assert.Equal(t, []api.Evidence{
		{Type: api.EvidenceCommit, SHA: "abc1234"},
		{Type: api.EvidenceAttachment, AttachmentID: reportAttachmentID},
	}, got.Evidence)
}

func TestCloseInputFromFormIgnoresAttachmentChoiceForOtherReasons(t *testing.T) {
	s := newCloseForm(formTarget{projectID: 7, issueShortID: "abc4"}, closeFormAttachments())
	s.field(fieldCloseAttachment).radio.set("run.log · 12 bytes")
	s.field(fieldCloseReason).radio.set("wontfix") // no sync: the stale field must not leak
	s.field(fieldCloseMessage).area.SetValue("This behavior is intentionally unsupported because it violates the authority boundary.")

	got, err := closeInputFromForm(s, "worker-a")

	require.NoError(t, err)
	assert.Empty(t, got.Evidence)
}

func TestCloseInputFromFormWithoutAttachmentStillNeedsEvidence(t *testing.T) {
	s := newCloseForm(formTarget{projectID: 7, issueShortID: "abc4"}, closeFormAttachments())
	s.field(fieldCloseMessage).area.SetValue("Delivered the report and checked its final contents.")

	_, err := closeInputFromForm(s, "worker-a")

	require.EqualError(t, err, "evidence value is required")
}

func TestRouteCloseKeyOffersTheIssuesAttachments(t *testing.T) {
	m := initialModel(Options{})
	m.closeRequiresEvidence = true
	m.view = viewList
	m.scope = scope{projectID: 7}
	var issue Issue
	require.NoError(t, json.Unmarshal([]byte(`{"project_id":7,"short_id":"abc4","status":"open","metadata":{"attachment.`+reportAttachmentID+`":{"name":"report.pdf","media_type":"application/pdf","size":2048,"docbank_version":"6f1c2e8a-2222-4222-8222-222222222222"}}}`), &issue))
	m.list.issues = []Issue{issue}

	got, _, handled := m.routeCloseKey(runeKey('x'))

	require.True(t, handled)
	require.NotNil(t, got.input.field(fieldCloseAttachment))
	assert.Equal(t, []string{"none", "report.pdf · 2048 bytes"}, got.input.field(fieldCloseAttachment).radio.choices)
}

func attachmentIssue(t *testing.T, name string) Issue {
	t.Helper()
	var issue Issue
	raw := `{"project_id":7,"short_id":"abc4","title":"Ship it","status":"open","metadata":{"attachment.` + reportAttachmentID +
		`":{"name":"` + name + `","media_type":"application/pdf","size":2048,"docbank_version":"6f1c2e8a-2222-4222-8222-222222222222"}}}`
	require.NoError(t, json.Unmarshal([]byte(raw), &issue))
	return issue
}

// manyAttachmentIssue holds n attachments that share one long name.
func manyAttachmentIssue(t *testing.T, name string, n int) Issue {
	t.Helper()
	var issue Issue
	raw := `{"project_id":7,"short_id":"abc4","title":"Ship it","status":"open","metadata":{`
	for i := range n {
		if i > 0 {
			raw += ","
		}
		raw += fmt.Sprintf(`"attachment.01KATTACH%016d":{"name":"%s","media_type":"application/pdf","size":2048,"docbank_version":"6f1c2e8a-2222-4222-8222-222222222222"}`, i, name)
	}
	require.NoError(t, json.Unmarshal([]byte(raw+`}}`), &issue))
	return issue
}

// focusCloseAttachment moves focus to the attachment selector.
func focusCloseAttachment(t *testing.T, m Model) Model {
	t.Helper()
	m.input.active = slices.IndexFunc(m.input.fields, func(f inputField) bool { return f.id == fieldCloseAttachment })
	require.GreaterOrEqual(t, m.input.active, 0)
	return m
}

func TestEvidenceKeyOpensTheCloseFormWithoutRequiredEvidence(t *testing.T) {
	m := initialModel(Options{})
	m.view = viewList
	m.scope = scope{projectID: 7}
	m.list.issues = []Issue{attachmentIssue(t, "report.pdf")}

	got, _, handled := m.routeCloseKey(runeKey('V'))

	require.True(t, handled)
	assert.Equal(t, inputCloseForm, got.input.kind)
	require.NotNil(t, got.input.field(fieldCloseAttachment))

	got, _, handled = m.routeCloseKey(runeKey('x'))
	require.True(t, handled)
	assert.Equal(t, inputNone, got.input.kind, "x keeps closing at once")
}

func TestOpeningTheCloseFormClearsThePriorityPrefix(t *testing.T) {
	for _, key := range []rune{'V', 'x'} {
		t.Run(string(key), func(t *testing.T) {
			m := initialModel(Options{})
			m.view = viewList
			m.scope = scope{projectID: 7}
			m.closeRequiresEvidence = true
			m.list.issues = []Issue{attachmentIssue(t, "report.pdf")}
			m, _ = stepModel(m, runeKey('!'))
			require.True(t, m.list.pendingPriority)
			require.NotEmpty(t, m.list.status)

			m, _ = stepModel(m, runeKey(key))

			require.Equal(t, inputCloseForm, m.input.kind)
			assert.False(t, m.list.pendingPriority, "the form cancels the priority prompt")
			assert.Empty(t, m.list.status, "and its status line")
		})
	}
}

func TestUndoCloseFormOffersTheIssuesAttachments(t *testing.T) {
	m := initialModel(Options{})
	m.undoHistory.entries = []undoEntry{{id: 1, uid: "01TEST-abc4", after: attachmentIssue(t, "report.pdf")}}

	got := m.openUndoCloseForm()

	require.NotNil(t, got.input.field(fieldCloseAttachment))
	assert.Equal(t, []string{"none", "report.pdf · 2048 bytes"}, got.input.field(fieldCloseAttachment).radio.choices)
}

func TestCloseFormWithAttachmentsFitsEverySize(t *testing.T) {
	defer snapshotInit(t)()
	longName := strings.Repeat("a", 251) + ".pdf" // 255 bytes, the longest stored name
	longProject := strings.Repeat("p", 120)
	for _, tc := range []struct {
		name          string
		width, height int
		help          bool
		fullForm      bool
	}{
		{"80x24", 80, 24, false, true},
		{"60x24", 60, 24, false, true},
		{"80x12", 80, 12, false, false},
		{"100x8", 100, 8, false, false},
		{"split layout", 160, 40, false, true},
		{"over help", 100, 24, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := initialModel(Options{})
			m.view = viewList
			m.scope = scope{projectID: 7, projectName: longProject}
			m.list.issues = []Issue{manyAttachmentIssue(t, longName, 10)}
			m, _ = stepModel(m, tea.WindowSizeMsg{Width: tc.width, Height: tc.height})
			m, _ = stepModel(m, runeKey('V'))
			require.Equal(t, inputCloseForm, m.input.kind)
			if tc.help {
				m.view = viewHelp
			}

			m = focusCloseAttachment(t, m)
			m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyRight})
			rendered := stripANSI(m.View().Content)

			for line := range strings.SplitSeq(rendered, "\n") {
				assert.LessOrEqual(t, ansi.StringWidth(line), tc.width, "each line fits the terminal")
			}
			if tc.fullForm {
				assert.Contains(t, rendered, "Attachment evidence", rendered)
				assert.Contains(t, rendered, "ctrl+o save", "the form keeps its footer")
				assert.Contains(t, rendered, "… · 2048 bytes (1 of 10)", "the long name is shortened, the size and position stay")
			}
			if !tc.help {
				assert.LessOrEqual(t, strings.Count(rendered, "\n")+1, tc.height, "the frame fits:\n%s", rendered)
			}
		})
	}
}

func TestCloseFormAttachmentSelectorCyclesThroughEveryAttachment(t *testing.T) {
	defer snapshotInit(t)()
	m := initialModel(Options{})
	m.view = viewList
	m.scope = scope{projectID: 7}
	m.list.issues = []Issue{manyAttachmentIssue(t, "report.pdf", 3)}
	m, _ = stepModel(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = stepModel(m, runeKey('V'))
	m = focusCloseAttachment(t, m)

	view := func() string { return stripANSI(m.View().Content) }
	assert.Contains(t, view(), "none")
	assert.NotContains(t, view(), "report.pdf")
	for _, want := range []string{"(1 of 3)", "(2 of 3)", "(3 of 3)"} {
		m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyRight})
		assert.Contains(t, view(), "report.pdf · 2048 bytes "+want)
	}
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Equal(t, 0, m.input.field(fieldCloseAttachment).radio.index, "right from the last attachment wraps to none")
	assert.NotContains(t, view(), "report.pdf")
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyLeft})
	assert.Contains(t, view(), "report.pdf · 2048 bytes (3 of 3)", "left from none wraps to the last attachment")
}
