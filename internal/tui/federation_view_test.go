package tui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
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
	m.federationInstance.Auth = AuthInfo{Kind: "db_token", Actor: "operator"}
	rendered := stripANSI(renderFederation(m))

	assert.Contains(t, rendered, "kata / federation")
	assert.Contains(t, rendered, "Federation for active daemon:")
	assert.Contains(t, rendered, "spoke-daemon")
	assert.Contains(t, rendered, "http://spoke.internal:7777")
	assert.Contains(t, rendered, "instance 01HZNQ7VFPK1XGD8R5MABCD4EA")
	assert.Contains(t, rendered, "auth token actor operator")
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

func TestFederationView_ActiveLocalGlobalAuthDisplaysTokenActor(t *testing.T) {
	m := setupFederationViewWithStatuses()
	m.activeDaemon = daemonTarget{Name: "local", Local: true}
	m.federationInstance.Auth = AuthInfo{Kind: "db_token", Actor: "operator"}

	rendered := stripANSI(renderFederation(m))

	assert.Contains(t, rendered, "auth token actor operator")
	assert.NotContains(t, rendered, "auth no token actor operator")
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

func TestFederationView_ListFitsTerminalHeight(t *testing.T) {
	statuses := make([]FederationProjectStatus, 0, 10)
	for i := range 10 {
		status := federationStatusFixture("spoke-proj-"+strconv.Itoa(i), "spoke")
		status.ProjectID = int64(i + 1)
		statuses = append(statuses, status)
	}
	m := setupFederationViewWithStatuses(statuses...)
	m.height = 12

	rendered := stripANSI(renderFederation(m))

	assert.LessOrEqual(t, len(strings.Split(rendered, "\n")), m.height)
}

func TestFederationView_DetailFitsTerminalHeight(t *testing.T) {
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.height = 12
	m.federationMode = federationModeDetail

	rendered := stripANSI(renderFederation(m))

	assert.LessOrEqual(t, len(strings.Split(rendered, "\n")), m.height)
	assert.Contains(t, rendered, "spoke-proj")
	assert.Contains(t, rendered, "[esc] back")
}

func TestFederationView_MouseClickUsesFederationRowOffset(t *testing.T) {
	m := setupFederationViewWithStatuses(
		federationStatusFixture("spoke-proj-0", "spoke"),
		federationStatusFixture("spoke-proj-1", "spoke"),
		federationStatusFixture("spoke-proj-2", "spoke"),
	)

	out, cmd := m.mouseFederationClick(7)

	require.Nil(t, cmd)
	assert.Equal(t, 0, out.federationCursor)
}

func TestFederationView_HelpAndFooterIncludeFederationBinding(t *testing.T) {
	help := stripANSI(renderHelp(newKeymap(), 100, ListFilter{}))
	assert.Contains(t, help, "F")
	assert.Contains(t, help, "federation")

	m := Model{list: listModel{issues: hierarchyIssues()}}
	assertHelpItemPresent(t, flattenHelpRows(m.queueHelpRows()), helpItem{key: "F", desc: "federation"})
}

func TestFederationBrowse_BKeyListsCatalogHubProjectsWithoutSwitchingActiveDaemon(t *testing.T) {
	spokeAPI := &Client{}
	spokeTarget := daemonTarget{Name: "spoke", URL: "https://spoke.example", Token: "spoke-auth"}
	hubTarget := daemonTarget{Name: "catalog-hub", URL: "https://hub.example", Token: "hub-auth"}
	hub := &recordingFederationHubAdmin{
		projects: []ProjectSummary{
			{ID: 42, Name: "hub-project"},
			{ID: 43, Name: "other-hub-project"},
		},
	}
	var requestedTarget daemonTarget
	restoreFederationHubAdminClient(t, func(
		_ context.Context,
		target daemonTarget,
	) (federationHubAdminAPI, daemonTarget, error) {
		requestedTarget = target
		return hub, target, nil
	})
	m := setupFederationView()
	m.api = spokeAPI
	m.activeDaemon = spokeTarget
	m.daemonTargets = []daemonTarget{spokeTarget, hubTarget}
	m.federationHubCursor = 1
	initialScope := m.scope
	initialSSE := m.sseCh
	initialConnGen := m.connGen
	initialSwitchAttempt := m.daemonSwitchAttempt

	out, cmd := m.routeFederationViewKey(keyRune('b'))
	require.NotNil(t, cmd)
	assert.Equal(t, federationModeBrowseHubs, out.federationMode)

	msg := cmd().(federationHubProjectsLoadedMsg)
	out, nextCmd := updateModel(out, msg)

	require.Nil(t, nextCmd)
	assert.Equal(t, hubTarget, requestedTarget)
	assert.Equal(t, 1, hub.listProjectsCalls)
	assert.Equal(t, spokeTarget, out.activeDaemon)
	assert.Same(t, spokeAPI, out.api)
	assert.Equal(t, initialScope, out.scope)
	assert.True(t, out.sseCh == initialSSE)
	assert.Equal(t, initialConnGen, out.connGen)
	assert.Equal(t, initialSwitchAttempt, out.daemonSwitchAttempt)

	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "catalog-hub")
	assert.Contains(t, rendered, "https://hub.example")
	assert.Contains(t, rendered, "hub-project")
	assert.Contains(t, rendered, "other-hub-project")
}

func TestFederationBrowse_ReadOnlyDoesNotCreateEnrollment(t *testing.T) {
	hubTarget := daemonTarget{Name: "catalog-hub", URL: "https://hub.example", Token: "hub-auth"}
	hub := &recordingFederationHubAdmin{
		projects: []ProjectSummary{{ID: 42, Name: "hub-project"}},
	}
	restoreFederationHubAdminClient(t, func(
		_ context.Context,
		target daemonTarget,
	) (federationHubAdminAPI, daemonTarget, error) {
		return hub, target, nil
	})
	m := setupFederationView()
	m.api = &Client{}
	m.activeDaemon = daemonTarget{Name: "spoke", URL: "https://spoke.example", Token: "spoke-auth"}
	m.daemonTargets = []daemonTarget{m.activeDaemon, hubTarget}
	m.federationHubCursor = 1

	out, cmd := m.routeFederationViewKey(keyRune('b'))
	require.NotNil(t, cmd)
	out, _ = updateModel(out, cmd().(federationHubProjectsLoadedMsg))

	out, cmd = out.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Equal(t, federationModeBrowseHubs, out.federationMode)
	assert.Equal(t, 0, hub.ensureProjectCalls)
	assert.Equal(t, 0, hub.enableFederationCalls)
	assert.Equal(t, 0, hub.createEnrollmentCalls)
	assert.NotEqual(t, federationModePreview, out.federationMode)
	assert.Empty(t, out.federationRecovery.Token)
	assert.Empty(t, out.federationRecovery.Command.Token)
	rendered := stripANSI(renderFederation(out))
	assert.NotContains(t, rendered, "single-use/secret-bearing")
	assert.NotContains(t, rendered, enrollmentSecret())
}

func TestFederationEnroll_NWithCurrentProjectStartsHubSelection(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "")
	t.Setenv("USER", "operator")
	m := setupFederationView()
	m.list.actor = resolveTUIActor()
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
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "Select hub daemon")
	assert.Contains(t, rendered, "hub https://hub.example auth token")
	assert.Equal(t, "operator", out.federationDraft.RequestedActor)
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

func TestFederationEnroll_EscFromHubSelectionReturnsToLocalProjectSelection(t *testing.T) {
	m := setupFederationView()
	m.scope = scope{allProjects: true}
	injectProjects(&m, mockProject{ID: 7, Name: "spoke-project"})

	out, cmd := m.routeFederationViewKey(keyRune('n'))
	require.Nil(t, cmd)
	out.federationLocalProjectCursor = 1
	out, cmd = out.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	require.Nil(t, cmd)
	require.Equal(t, federationModeSelectHub, out.federationMode)

	out, cmd = out.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEsc})

	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectLocalProject, out.federationMode)
}

func TestFederationEnroll_SelectHubThenSelectSameNameHubProjectPreview(t *testing.T) {
	m := setupFederationHubProjectSelection()
	m.federationDraft.HubInstance = InstanceInfo{Auth: AuthInfo{Kind: "db_token", Actor: "hub-operator"}}
	m.federationHubProjects = []ProjectSummary{{ID: 42, Name: "spoke-project"}}

	renderedSelection := stripANSI(renderFederation(m))
	assert.Contains(t, renderedSelection, "hub auth: token actor hub-operator")
	assert.Contains(t, renderedSelection, `use existing hub project "spoke-project"; enable federation if needed`)
	assert.NotContains(t, renderedSelection, "will be created if missing")
	assert.NotContains(t, renderedSelection, "\n  spoke-project\n")

	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Equal(t, federationModePreview, out.federationMode)
	assert.Equal(t, federationOperationAdoptSameName, out.federationDraft.Operation)
	assert.Equal(t, int64(42), out.federationDraft.HubProjectID)
	assert.Equal(t, "claim,pull,push", out.federationDraft.APICapabilities)
	assert.Equal(t, "pull,push,lease", out.federationDraft.DisplayCapabilities)
	assert.True(t, out.federationDraft.AdoptExisting)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "Operation: adopt existing local project")
	assert.Contains(t, rendered, "hub auth: token actor hub-operator")
	assert.Contains(t, rendered, `use existing hub project "spoke-project"; enable federation if needed`)
	assert.Contains(t, rendered, "allow_insecure: true")
	assert.Contains(t, rendered, "pre-adoption event history is replaced by snapshot events for federation")
}

func TestFederationEnroll_SelectHubLoadsHubAuthPrincipal(t *testing.T) {
	hub := &recordingFederationHubAdmin{
		instance: InstanceInfo{Auth: AuthInfo{Kind: "db_token", Actor: "hub-operator"}},
		projects: []ProjectSummary{
			{ID: 42, Name: "spoke-project"},
		},
	}
	restoreFederationHubAdminClient(t, func(
		_ context.Context,
		target daemonTarget,
	) (federationHubAdminAPI, daemonTarget, error) {
		return hub, target, nil
	})
	m := setupFederationView()
	m.scope = homedScope(7, "spoke-project")
	m.daemonTargets = []daemonTarget{
		{Name: "spoke", URL: "https://spoke.example", Token: "spoke-auth"},
		{Name: "hub", URL: "https://hub.example", Token: "hub-auth"},
	}
	m.activeDaemon = m.daemonTargets[0]

	out, _ := m.routeFederationViewKey(keyRune('n'))
	out.federationHubCursor = 1
	out, cmd := out.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)
	out = out.handleFederationHubProjectsLoaded(cmd().(federationHubProjectsLoadedMsg))

	rendered := stripANSI(renderFederation(out))
	assert.Equal(t, "hub-operator", out.federationDraft.HubInstance.Auth.Actor)
	assert.Contains(t, rendered, "hub auth: token actor hub-operator")

	out, cmd = out.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	require.Nil(t, cmd)
	rendered = stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "requested actor: hub-operator")
	assert.NotContains(t, rendered, "requested actor: anonymous")
}

func TestFederationEnroll_SelectDifferentHubProjectSkipsSameNameDuplicate(t *testing.T) {
	m := setupFederationHubProjectSelection()
	m.federationHubProjects = []ProjectSummary{
		{ID: 42, Name: "spoke-project"},
		{ID: 77, Name: "team-hub-project"},
	}
	m.federationHubProjectCursor = 1

	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Equal(t, federationModePreview, out.federationMode)
	assert.Equal(t, federationOperationAdoptSelectedHub, out.federationDraft.Operation)
	assert.Equal(t, int64(77), out.federationDraft.HubProjectID)
	assert.Equal(t, "team-hub-project", out.federationDraft.HubProjectName)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "team-hub-project")
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

func TestFederationEnroll_SameNamePreviewClearsStaleSelectedHubProjectID(t *testing.T) {
	m := setupFederationHubProjectSelection()
	m.federationDraft.HubProjectID = 42
	m.federationDraft.HubProjectName = "old-hub-project"
	m.federationHubProjectCursor = 0
	m.federationHubProjects = []ProjectSummary{{ID: 77, Name: "different-hub-project"}}

	out, cmd := m.previewFederationEnrollment()

	require.Nil(t, cmd)
	assert.Equal(t, federationOperationAdoptSameName, out.federationDraft.Operation)
	assert.Equal(t, int64(0), out.federationDraft.HubProjectID)
	assert.Equal(t, "spoke-project", out.federationDraft.HubProjectName)
}

func TestFederationEnroll_ExistingLocalFederationBindingBlocksBeforeMutation(t *testing.T) {
	m := setupFederationHubProjectSelection()
	m.federationStatuses = []FederationProjectStatus{federationStatusFixture("spoke-project", "spoke")}
	m.federationHubProjects = []ProjectSummary{{ID: 42, Name: "spoke-project"}}

	out, cmd := m.previewFederationEnrollment()
	require.Nil(t, cmd)

	assert.Equal(t, federationModePreview, out.federationMode)
	assert.Contains(t, stripANSI(renderFederation(out)), "already has federation binding")

	out, cmd = out.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	require.Nil(t, cmd)
	assert.Equal(t, federationModePreview, out.federationMode)
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

func TestFederationEnroll_LocalHubTargetBlocksBeforeMutation(t *testing.T) {
	m := setupFederationView()
	m.scope = homedScope(7, "spoke-project")
	m.federationMode = federationModeSelectHub
	m.federationDraft = newFederationDraft("operator")
	m.federationDraft.SpokeProjectID = 7
	m.federationDraft.SpokeProjectName = "spoke-project"
	m.activeDaemon = daemonTarget{Name: "spoke", URL: "https://spoke.example"}
	m.daemonTargets = []daemonTarget{
		m.activeDaemon,
		{Name: "local-hub", Local: true},
	}
	m.federationHubCursor = 1

	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectHub, out.federationMode)
	assert.Empty(t, out.federationDraft.HubTarget.Name)
	require.Error(t, out.federationEnrollErr)
	assert.Contains(t, out.federationEnrollErr.Error(), "local hub")
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

func TestFederationEnroll_EnterCreatesEnrollmentAndJoinsSpoke(t *testing.T) {
	m, joinBody := setupFederationExecutionPreview(t, federationExecutionServerOptions{})

	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd().(federationEnrollResultMsg)
	out, refresh := updateModel(out, msg)

	require.NotNil(t, refresh)
	assert.Equal(t, federationModeResult, out.federationMode)
	assert.Equal(t, "hub-actor", joinBody.Actor)
	assert.Equal(t, "claim,pull,push", joinBody.Capabilities)
	assert.True(t, joinBody.PushEnabled)
	assert.True(t, joinBody.AdoptExisting)
	assert.True(t, joinBody.AllowInsecure)
	assert.Equal(t, enrollmentSecret(), joinBody.Token)
	assert.NotContains(t, stripANSI(renderFederation(out)), enrollmentSecret())
}

func TestFederationEnroll_AdoptSelectedHubJoinsSelectedLocalProjectName(t *testing.T) {
	m, joinBody := setupFederationExecutionPreview(t, federationExecutionServerOptions{hubProjectName: "hub-project"})
	m.federationDraft.Operation = federationOperationAdoptSelectedHub
	m.federationDraft.SpokeProjectName = "local-spoke-project"
	m.federationDraft.HubProjectName = "hub-project"

	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd().(federationEnrollResultMsg)
	out, _ = updateModel(out, msg)

	assert.Equal(t, federationModeResult, out.federationMode)
	assert.Equal(t, "local-spoke-project", joinBody.ProjectName)
	assert.Equal(t, "hub-project", out.federationResult.Metadata.ProjectName)
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EX", joinBody.HubProjectUID)
}

func TestFederationEnroll_AdoptSelectedHubRecoveryUsesSelectedLocalProjectName(t *testing.T) {
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{
		hubProjectName: "hub-project",
		joinStatus:     http.StatusInternalServerError,
	})
	m.federationDraft.Operation = federationOperationAdoptSelectedHub
	m.federationDraft.SpokeProjectName = "local-spoke-project"
	m.federationDraft.HubProjectName = "hub-project"
	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd().(federationEnrollResultMsg)
	out, _ = updateModel(out, msg)

	out, revealCmd := out.routeFederationViewKey(keyRune('R'))
	require.Nil(t, revealCmd)

	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "--project local-spoke-project")
	assert.NotContains(t, rendered, "--project hub-project")
}

func TestFederationEnroll_ResultShowsBoundActorAndHubMetadata(t *testing.T) {
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{})

	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd().(federationEnrollResultMsg)
	out, _ = updateModel(out, msg)

	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "actor: hub-actor")
	assert.Contains(t, rendered, "adopted")
	assert.Contains(t, rendered, "snapshot count: 5")
	assert.Contains(t, rendered, "hub URL: ")
	assert.Contains(t, rendered, "hub project ID: 42")
	assert.Contains(t, rendered, "hub project UID: 01HZNQ7VFPK1XGD8R5MABCD4EX")
	assert.NotContains(t, rendered, enrollmentSecret())
}

func TestFederationEnroll_MetadataFailureShowsHubLabeledRecoveryAndHidesToken(t *testing.T) {
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{metadataStatus: 500})

	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd().(federationEnrollResultMsg)
	out, _ = updateModel(out, msg)

	rendered := stripANSI(renderFederation(out))
	assert.Equal(t, federationModeRecovery, out.federationMode)
	assert.Contains(t, rendered, "hub hub: enrollment metadata fetch failed")
	assert.NotContains(t, rendered, enrollmentSecret())
}

func TestFederationEnroll_MetadataFailureRecoveryRevealUsesOnlyAvailableFields(t *testing.T) {
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{metadataStatus: 500})
	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	msg := cmd().(federationEnrollResultMsg)
	out, _ = updateModel(out, msg)

	out, revealCmd := out.routeFederationViewKey(keyRune('R'))
	require.Nil(t, revealCmd)

	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "single-use/secret-bearing")
	assert.Contains(t, rendered, "spoke target")
	assert.Contains(t, rendered, "KATA_SERVER=")
	assert.Contains(t, rendered, "--hub-url")
	assert.Contains(t, rendered, "--hub-project-id 42")
	assert.Contains(t, rendered, "--project spoke-project")
	assert.Contains(t, rendered, enrollmentSecret())
	assert.NotContains(t, rendered, "--hub-project-uid")
	assert.NotContains(t, rendered, "--project-name")
	assert.NotContains(t, rendered, "--replay-horizon-event-id")
	assert.NotContains(t, rendered, "--baseline-through-event-id")
	assert.NotContains(t, rendered, "--replay-horizon")
	assert.NotContains(t, rendered, "--baseline-through")
	assert.NotContains(t, rendered, "--server")
}

func TestFederationEnroll_JoinFailureShowsSpokeLabeledRecoveryAndHidesToken(t *testing.T) {
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{joinStatus: 500})

	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd().(federationEnrollResultMsg)
	out, _ = updateModel(out, msg)

	rendered := stripANSI(renderFederation(out))
	assert.Equal(t, federationModeRecovery, out.federationMode)
	assert.Contains(t, rendered, "hub: enrollment created")
	assert.Contains(t, rendered, "spoke: join failed")
	assert.NotContains(t, rendered, enrollmentSecret())
}

func TestFederationEnroll_PreEnrollmentFailureReturnsToPreview(t *testing.T) {
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{})
	restoreFederationHubAdminClient(t, func(
		_ context.Context,
		_ daemonTarget,
	) (federationHubAdminAPI, daemonTarget, error) {
		return nil, daemonTarget{}, errors.New("hub unavailable")
	})

	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd().(federationEnrollResultMsg)
	out, nextCmd := updateModel(out, msg)

	require.Nil(t, nextCmd)
	assert.Equal(t, federationModePreview, out.federationMode)
	require.Error(t, out.federationEnrollErr)
	assert.Contains(t, out.federationEnrollErr.Error(), "hub unavailable")
	assert.Empty(t, out.federationRecovery.Token)
}

func TestFederationEnroll_MissingSpokeInstanceBlocksBeforeHubMutation(t *testing.T) {
	m := setupFederationHubProjectSelection()
	var hubCalled bool
	restoreFederationHubAdminClient(t, func(
		_ context.Context,
		_ daemonTarget,
	) (federationHubAdminAPI, daemonTarget, error) {
		hubCalled = true
		return &recordingFederationHubAdmin{}, daemonTarget{}, nil
	})

	result, err := runFederationEnrollment(
		context.Background(),
		m.federationDraft,
		"",
		m.activeDaemon,
		m.api,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "spoke instance UID")
	assert.False(t, hubCalled)
	assert.Empty(t, result.Enrollment.Token)
}

func TestFederationEnroll_JoinFailureRecoveryRevealIsExplicitAndSecretBearing(t *testing.T) {
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{joinStatus: 500})
	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	msg := cmd().(federationEnrollResultMsg)
	out, _ = updateModel(out, msg)

	out, revealCmd := out.routeFederationViewKey(keyRune('R'))
	require.Nil(t, revealCmd)

	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "single-use/secret-bearing")
	assert.Contains(t, rendered, "valid and not revoked")
	assert.Contains(t, rendered, "spoke target")
	assert.Contains(t, rendered, "KATA_SERVER=")
	assert.Contains(t, rendered, "--hub-url")
	assert.Contains(t, rendered, "--hub-project-id 42")
	assert.Contains(t, rendered, "--hub-project-uid 01HZNQ7VFPK1XGD8R5MABCD4EX")
	assert.Contains(t, rendered, "--project spoke-project")
	assert.Contains(t, rendered, "--replay-horizon 9")
	assert.Contains(t, rendered, "--baseline-through 11")
	assert.Contains(t, rendered, enrollmentSecret())
	assert.NotContains(t, rendered, "--project-name")
	assert.NotContains(t, rendered, "--replay-horizon-event-id")
	assert.NotContains(t, rendered, "--baseline-through-event-id")
	assert.NotContains(t, rendered, "--server")
}

func TestFederationEnroll_RecoveryCommandPreservesSpokeAllowInsecure(t *testing.T) {
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{joinStatus: 500})
	m.activeDaemon.AllowInsecure = true
	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	msg := cmd().(federationEnrollResultMsg)
	out, _ = updateModel(out, msg)

	out, revealCmd := out.routeFederationViewKey(keyRune('R'))
	require.Nil(t, revealCmd)

	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "KATA_SERVER=")
	assert.Contains(t, rendered, "KATA_ALLOW_INSECURE=1")
}

func TestFederationEnroll_RecoveryCommandPreservesSpokeAuthOnlyAfterReveal(t *testing.T) {
	spokeToken := spokeAuthSecret()
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{joinStatus: 500})
	m.activeDaemon.Token = spokeToken
	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	msg := cmd().(federationEnrollResultMsg)
	out, _ = updateModel(out, msg)

	rendered := stripANSI(renderFederation(out))
	assert.NotContains(t, rendered, spokeToken)

	out, revealCmd := out.routeFederationViewKey(keyRune('R'))
	require.Nil(t, revealCmd)

	rendered = stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "KATA_AUTH_TOKEN="+spokeToken)
}

func TestFederationAuthDisplayShowsTrustedProxyMissingActor(t *testing.T) {
	got := federationAuthDisplay(daemonTarget{Name: "hub"}, AuthInfo{Kind: "trusted_proxy_absent"})

	assert.Equal(t, "trusted-proxy missing actor", got)
}

func TestFederationEnroll_RecoveryCommandQuotesShellMetacharacters(t *testing.T) {
	cmd := federationRecoveryCommand{
		HubURL:        "http://hub.internal:7777",
		HubProjectID:  42,
		ProjectName:   "spoke;project",
		Token:         "token$(secret)",
		Actor:         "hub$(actor)",
		Capabilities:  "claim,pull,push",
		SpokeEndpoint: "http://spoke.internal:7777",
	}

	rendered := federationRecoveryCommandString(cmd)

	assert.Contains(t, rendered, "--project 'spoke;project'")
	assert.Contains(t, rendered, "--token 'token$(secret)'")
	assert.Contains(t, rendered, "--actor 'hub$(actor)'")
	assert.NotContains(t, rendered, "--project spoke;project")
	assert.NotContains(t, rendered, "--token token$(secret)")
	assert.NotContains(t, rendered, "--actor hub$(actor)")
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

type federationExecutionServerOptions struct {
	metadataStatus int
	joinStatus     int
	hubProjectName string
}

func setupFederationExecutionPreview(
	t *testing.T,
	opts federationExecutionServerOptions,
) (Model, *CreateFederationReplicaInput) {
	t.Helper()
	hubProjectName := opts.hubProjectName
	if hubProjectName == "" {
		hubProjectName = "spoke-project"
	}
	var joinBody CreateFederationReplicaInput
	spoke := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/federation/replicas": func(w http.ResponseWriter, r *http.Request) {
			if opts.joinStatus != 0 {
				w.WriteHeader(opts.joinStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "join failed"}})
				return
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&joinBody))
			respondJSON(t, w, api.CreateFederationReplicaBody{
				Adopted:               true,
				AdoptionSnapshotCount: 5,
			})
		},
	})
	hub := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/projects": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				respondJSON(t, w, map[string]any{"project": map[string]any{"id": 42, "name": hubProjectName}})
				return
			}
			respondJSON(t, w, map[string]any{"projects": []map[string]any{{"id": 42, "name": hubProjectName}}})
		},
		"/api/v1/projects/42/federation/enable": func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(t, w, federationMetadataBody(hubProjectName))
		},
		"/api/v1/federation/enrollments": func(w http.ResponseWriter, _ *http.Request) {
			projectID := int64(42)
			respondJSON(t, w, api.FederationEnrollmentOut{
				ID:               7,
				SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
				ProjectID:        &projectID,
				Capabilities:     "claim,pull,push",
				Actor:            "hub-actor",
				Token:            enrollmentSecret(),
			})
		},
		"/api/v1/projects/42/federation/metadata": func(w http.ResponseWriter, _ *http.Request) {
			if opts.metadataStatus != 0 {
				w.WriteHeader(opts.metadataStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "metadata failed"}})
				return
			}
			respondJSON(t, w, federationMetadataBody(hubProjectName))
		},
	})
	m := setupFederationHubProjectSelection()
	m.api = NewClient(spoke.URL, spoke.Client())
	m.activeDaemon = daemonTarget{Name: "spoke", URL: spoke.URL}
	m.federationInstance = InstanceInfo{InstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA"}
	m.federationMode = federationModePreview
	m.federationDraft.Operation = federationOperationAdoptSameName
	m.federationDraft.HubProjectID = 42
	m.federationDraft.HubProjectName = hubProjectName
	m.federationDraft.HubTarget = daemonTarget{Name: "hub", URL: hub.URL, AllowInsecure: true}
	m.federationDraft.AllowInsecure = true
	return m, &joinBody
}

func federationMetadataBody(projectName string) api.ProjectFederationBody {
	return api.ProjectFederationBody{
		ProjectID:              42,
		ProjectUID:             "01HZNQ7VFPK1XGD8R5MABCD4EX",
		ProjectName:            projectName,
		ReplayHorizonEventID:   9,
		BaselineThroughEventID: 11,
	}
}

func enrollmentSecret() string {
	return strings.Join([]string{"enrollment", "secret"}, "-")
}

func spokeAuthSecret() string {
	return strings.Join([]string{"spoke", "auth"}, "-")
}

type recordingFederationHubAdmin struct {
	instance              InstanceInfo
	projects              []ProjectSummary
	getInstanceCalls      int
	listProjectsCalls     int
	ensureProjectCalls    int
	enableFederationCalls int
	createEnrollmentCalls int
}

func (h *recordingFederationHubAdmin) GetInstance(_ context.Context) (InstanceInfo, error) {
	h.getInstanceCalls++
	return h.instance, nil
}

func (h *recordingFederationHubAdmin) ListProjects(_ context.Context) ([]ProjectSummary, error) {
	h.listProjectsCalls++
	return h.projects, nil
}

func (h *recordingFederationHubAdmin) EnsureProject(
	_ context.Context,
	name string,
) (ProjectSummary, error) {
	h.ensureProjectCalls++
	return ProjectSummary{ID: 99, Name: name}, nil
}

func (h *recordingFederationHubAdmin) EnableFederation(
	_ context.Context,
	projectID int64,
	_ string,
) (ProjectFederationMetadata, error) {
	h.enableFederationCalls++
	return ProjectFederationMetadata{ProjectID: projectID}, nil
}

func (h *recordingFederationHubAdmin) CreateFederationEnrollment(
	_ context.Context,
	_ CreateFederationEnrollmentInput,
) (FederationEnrollment, error) {
	h.createEnrollmentCalls++
	return FederationEnrollment{Token: enrollmentSecret()}, nil
}

func restoreFederationHubAdminClient(
	t *testing.T,
	replacement func(context.Context, daemonTarget) (federationHubAdminAPI, daemonTarget, error),
) {
	t.Helper()
	orig := newFederationHubAdminClient
	newFederationHubAdminClient = replacement
	t.Cleanup(func() {
		newFederationHubAdminClient = orig
	})
}
