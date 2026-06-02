package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFederationView_FKeyTransitionsFromList(t *testing.T) {
	m := setupFederationSourceModel()

	out, cmd := updateModel(m, keyRune('F'))

	assert.Equal(t, viewFederation, out.view)
	assert.Equal(t, viewList, out.prevView)
	require.NotNil(t, cmd)
}

func TestFederationView_EscReturnsToPreviousView(t *testing.T) {
	m := setupFederationView()

	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEsc})

	require.Nil(t, cmd)
	assert.Equal(t, viewList, out.view)
}

func TestFederationView_EnterOpensSelectedStatusDetail(t *testing.T) {
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.federationCursor = 0

	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Equal(t, federationModeDetail, out.federationMode)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "hub project UID")
	assert.Contains(t, rendered, "pull cursor")
	assert.Contains(t, rendered, "credential")
}

func TestFederationView_RenderIncludesActiveSpokeStatus(t *testing.T) {
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	rendered := stripANSI(renderFederation(m))

	assert.Contains(t, rendered, "kata / federation")
	assert.Contains(t, rendered, "Federation for active daemon:")
	assert.Contains(t, rendered, "spoke-daemon")
	assert.Contains(t, rendered, "http://spoke.internal:7777")
	assert.Contains(t, rendered, "instance 01HZNQ7VFPK1XGD8R5MABCD4EA")
	assert.Contains(t, rendered, "auth token")
	assert.Contains(t, rendered, "spoke-proj")
	assert.Contains(t, rendered, "hub.internal:7777")
	assert.Contains(t, rendered, "operator")
	assert.Contains(t, rendered, "push")
	assert.Contains(t, rendered, "3")
	assert.Contains(t, rendered, "insecure")
	assert.Contains(t, rendered, "quarantine")
	assert.Contains(t, rendered, "reset")
	assert.Contains(t, rendered, "violations")
}

func TestFederationView_ListShowsOnlySpokeBindings(t *testing.T) {
	m := setupFederationViewWithStatuses(
		federationStatusFixture("spoke-proj", "spoke"),
		federationStatusFixture("hub-only", "hub"),
	)

	rendered := stripANSI(renderFederation(m))

	assert.Contains(t, rendered, "spoke-proj")
	assert.NotContains(t, rendered, "hub-only")
}

func TestFederationView_HelpAndFooterIncludeFederationBinding(t *testing.T) {
	help := stripANSI(renderHelp(newKeymap(), 100, ListFilter{}))
	assert.Contains(t, help, "F")
	assert.Contains(t, help, "federation")

	m := Model{list: listModel{issues: hierarchyIssues()}}
	assertHelpItemPresent(t, flattenHelpRows(m.queueHelpRows()), helpItem{key: "F", desc: "federation"})
}

func TestFederationEnroll_NWithCurrentProjectStartsHubSelection(t *testing.T) {
	m := setupFederationView()
	m.scope = homedScope(7, "spoke-project")
	m.daemonTargets = []daemonTarget{
		{Name: "spoke", URL: "https://spoke.example", Token: "spoke-auth"},
		{Name: "hub", URL: "https://hub.example", Token: "hub-auth"},
	}
	m.activeDaemon = m.daemonTargets[0]

	out, cmd := m.routeFederationViewKey(keyRune('n'))

	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectHub, out.federationMode)
	assert.Equal(t, int64(7), out.federationDraft.SpokeProjectID)
	assert.Equal(t, "spoke-project", out.federationDraft.SpokeProjectName)
	assert.True(t, out.federationDraft.AdoptExisting)
	assert.Contains(t, stripANSI(renderFederation(out)), "Select hub daemon")
}

func TestFederationEnroll_NWithoutProjectStartsLocalProjectSelection(t *testing.T) {
	m := setupFederationView()
	m.scope = scope{allProjects: true}
	injectProjects(&m,
		mockProject{ID: 7, Name: "spoke-project"},
		mockProject{ID: 9, Name: "other-project"},
	)

	out, cmd := m.routeFederationViewKey(keyRune('n'))

	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectLocalProject, out.federationMode)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "Select local spoke project")
	assert.Contains(t, rendered, "create new local replica from hub project")
	assert.Contains(t, rendered, "spoke-project")
	assert.Contains(t, rendered, "other-project")
}

func TestFederationEnroll_SelectHubThenSelectSameNameHubProjectPreview(t *testing.T) {
	m := setupFederationHubProjectSelection()
	m.federationHubProjects = []ProjectSummary{{ID: 42, Name: "spoke-project"}}

	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Equal(t, federationModePreview, out.federationMode)
	assert.Equal(t, federationOperationAdoptSameName, out.federationDraft.Operation)
	assert.Equal(t, "claim,pull,push", out.federationDraft.APICapabilities)
	assert.Equal(t, "pull,push,lease", out.federationDraft.DisplayCapabilities)
	assert.True(t, out.federationDraft.AdoptExisting)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "Operation: adopt existing local project")
	assert.Contains(t, rendered, `hub project "spoke-project" will be created if missing or enabled if present`)
	assert.Contains(t, rendered, "allow_insecure: true")
	assert.Contains(t, rendered, "pre-adoption event history is replaced by snapshot events for federation")
}

func TestFederationEnroll_SelectDifferentExistingHubProjectStillAdoptsLocalProject(t *testing.T) {
	m := setupFederationHubProjectSelection()
	m.federationHubProjects = []ProjectSummary{{ID: 42, Name: "hub-project"}}
	m.federationHubProjectCursor = 1

	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Equal(t, federationModePreview, out.federationMode)
	assert.Equal(t, federationOperationAdoptSelectedHub, out.federationDraft.Operation)
	assert.True(t, out.federationDraft.AdoptExisting)
	assert.Equal(t, int64(42), out.federationDraft.HubProjectID)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "Operation: adopt existing local project into selected hub project")
	assert.Contains(t, rendered, "hub-project")
}

func TestFederationEnroll_CreateReplicaBranchDefaultsLocalNameFromHubProject(t *testing.T) {
	m := setupFederationView()
	m.scope = scope{allProjects: true}
	m.daemonTargets = []daemonTarget{
		{Name: "spoke", URL: "https://spoke.example", Token: "spoke-auth"},
		{Name: "hub", URL: "https://hub.example", Token: "hub-auth"},
	}
	m.activeDaemon = m.daemonTargets[0]

	out, cmd := m.routeFederationViewKey(keyRune('n'))
	require.Nil(t, cmd)
	out, cmd = out.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectHub, out.federationMode)
	out.federationHubCursor = 1
	out, cmd = out.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)
	out = out.handleFederationHubProjectsLoaded(federationHubProjectsLoadedMsg{
		connGen:  out.connGen,
		gen:      out.federationEnrollGen,
		target:   out.federationDraft.HubTarget,
		projects: []ProjectSummary{{ID: 42, Name: "hub-project"}},
	})

	out, cmd = out.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Equal(t, federationModePreview, out.federationMode)
	assert.Equal(t, federationOperationCreateReplica, out.federationDraft.Operation)
	assert.Equal(t, "hub-project", out.federationDraft.SpokeProjectName)
	assert.False(t, out.federationDraft.AdoptExisting)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "Operation: create new local replica from hub project")
	assert.NotContains(t, rendered, "pre-adoption event history")
}

func TestFederationEnroll_CreateReplicaBranchPreflightsLocalNameConflict(t *testing.T) {
	m := setupFederationView()
	m.scope = scope{allProjects: true}
	injectProjects(&m, mockProject{ID: 7, Name: "spoke-project"})
	m.federationMode = federationModeSelectHubProject
	m.federationDraft = newFederationDraft("anonymous")
	m.federationDraft.CreateReplica = true
	m.federationDraft.HubTarget = daemonTarget{Name: "hub", URL: "https://hub.example"}
	m.federationHubProjects = []ProjectSummary{{ID: 42, Name: "spoke-project"}}

	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Equal(t, federationModePreview, out.federationMode)
	assert.False(t, out.federationDraft.AdoptExisting)
	assert.Contains(t, stripANSI(renderFederation(out)), `Blocked: local project "spoke-project" already exists`)
}

func TestFederationEnroll_MissingTokenEnvBlocksBeforeMutation(t *testing.T) {
	t.Setenv(missingHubAuthEnvName(), "")
	m := setupFederationView()
	m.scope = homedScope(7, "spoke-project")
	m.daemonTargets = []daemonTarget{
		{Name: "spoke", URL: "https://spoke.example", Token: "spoke-auth"},
		{Name: "hub", URL: "https://hub.example", TokenEnv: missingHubAuthEnvName()},
	}
	m.activeDaemon = m.daemonTargets[0]

	out, _ := m.routeFederationViewKey(keyRune('n'))
	out.federationHubCursor = 1
	out, cmd := out.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectHub, out.federationMode)
	assert.Contains(t, stripANSI(renderFederation(out)), "token_env")
}

func TestFederationEnroll_ActiveDaemonAsHubBlocked(t *testing.T) {
	m := setupFederationView()
	m.scope = homedScope(7, "spoke-project")
	m.daemonTargets = []daemonTarget{{Name: "spoke", URL: "https://spoke.example", Token: "spoke-auth"}}
	m.activeDaemon = m.daemonTargets[0]

	out, _ := m.routeFederationViewKey(keyRune('n'))
	out, cmd := out.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Contains(t, stripANSI(renderFederation(out)), "active daemon cannot be selected as hub")
}

func TestFederationEnroll_PlainHTTPHostnameRequiresCatalogAllowInsecure(t *testing.T) {
	m := setupFederationView()
	m.scope = homedScope(7, "spoke-project")
	m.daemonTargets = []daemonTarget{
		{Name: "spoke", URL: "https://spoke.example", Token: "spoke-auth"},
		{Name: "hub", URL: "http://hub.internal:7777", Token: "hub-auth"},
	}
	m.activeDaemon = m.daemonTargets[0]

	out, _ := m.routeFederationViewKey(keyRune('n'))
	out.federationHubCursor = 1
	out, cmd := out.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Contains(t, stripANSI(renderFederation(out)), "allow_insecure")
}

func setupFederationSourceModel() Model {
	m := newTestModel()
	m.view = viewList
	m.width, m.height = 120, 24
	m.activeDaemon = daemonTarget{
		Name:     "spoke-daemon",
		URL:      "http://spoke.internal:7777",
		TokenEnv: testAuthEnvName(),
	}
	return m
}

func testAuthEnvName() string {
	return strings.Join([]string{"KATA", "SPOKE", "AUTH"}, "_")
}

func missingHubAuthEnvName() string {
	return strings.Join([]string{"MISSING", "HUB", "AUTH"}, "_")
}

func setupFederationView() Model {
	m := setupFederationSourceModel()
	m.view = viewFederation
	m.prevView = viewList
	m.federationMode = federationModeList
	return m
}

func setupFederationViewWithStatuses(statuses ...FederationProjectStatus) Model {
	m := setupFederationView()
	m.federationInstance = InstanceInfo{
		InstanceUID:   "01HZNQ7VFPK1XGD8R5MABCD4EA",
		Version:       "dev",
		SchemaVersion: 1,
	}
	m.federationStatuses = statuses
	return m
}

func federationStatusFixture(projectName, role string) FederationProjectStatus {
	last := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	return FederationProjectStatus{
		ProjectID:                   7,
		ProjectUID:                  "01HZNQ7VFPK1XGD8R5MABCD4EP",
		ProjectName:                 projectName,
		Role:                        role,
		Enabled:                     true,
		PushEnabled:                 true,
		BoundActor:                  "operator",
		HubURL:                      "http://hub.internal:7777",
		HubProjectID:                42,
		HubProjectUID:               "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Capabilities:                "claim,pull,push",
		AllowInsecure:               true,
		CredentialStatus:            "present",
		PullCursorEventID:           12,
		PushCursorEventID:           10,
		PendingPushCount:            3,
		PendingPushHighWaterEventID: 15,
		PendingClaimCount:           1,
		ActiveQuarantineCount:       1,
		ResetBlocker:                "pending push",
		UnresolvedViolationCount:    2,
		RecentViolationCount:        2,
		LastSuccessfulSyncAt:        &last,
		LastPullSuccessAt:           &last,
		LastPushSuccessAt:           &last,
	}
}

func setupFederationHubProjectSelection() Model {
	m := setupFederationView()
	m.scope = homedScope(7, "spoke-project")
	m.list.actor = "operator"
	m.federationMode = federationModeSelectHubProject
	m.federationDraft = newFederationDraft("operator")
	m.federationDraft.SpokeProjectID = 7
	m.federationDraft.SpokeProjectName = "spoke-project"
	m.federationDraft.HubTarget = daemonTarget{
		Name:          "hub",
		URL:           "https://hub.example",
		Token:         "hub-auth",
		AllowInsecure: true,
	}
	m.federationDraft.AllowInsecure = true
	m.federationDraft.AdoptExisting = true
	return m
}
