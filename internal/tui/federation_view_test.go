package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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

	out, cmd := m.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEsc})

	require.Nil(t, cmd)
	assert.Equal(t, viewList, out.view)
}

func TestFederationView_EnterOpensSelectedStatusDetail(t *testing.T) {
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.federationCursor = 0

	out, cmd := enterThroughAdoptConfirm(t, m)

	require.Nil(t, cmd)
	assert.Equal(t, federationModeDetail, out.federationMode)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "hub project UID")
	assert.Contains(t, rendered, "pull cursor")
	assert.Contains(t, rendered, "credential")
	assert.Contains(t, rendered, "quarantine #7: push events 11-13")
	assert.Contains(t, rendered, "hub rejected deferred peer")
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
	assert.NotContains(t, rendered, "hub rejected deferred peer")
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

	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})

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

// TestFederationEnroll_NWithCurrentProjectStartsLocalSelectionCursored: an
// active project must never skip the local-project step (that hid the
// create-replica row and silently pre-armed adoption); it only positions the
// cursor, so the adopt flow costs exactly one Enter more.
func TestFederationEnroll_NWithCurrentProjectStartsLocalSelectionCursored(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "")
	t.Setenv("USER", "operator")
	m := setupFederationView()
	m.list.actor = resolveTUIActor()
	m.scope = homedScope(7, "spoke-project")
	injectProjects(&m,
		mockProject{ID: 7, Name: "spoke-project"},
		mockProject{ID: 9, Name: "other-project"},
	)
	m.daemonTargets = []daemonTarget{
		{Name: "spoke", URL: "https://spoke.example", Token: "spoke-auth"},
		{Name: "hub", URL: "https://hub.example", Token: "hub-auth"},
	}
	m.activeDaemon = m.daemonTargets[0]

	out, cmd := m.routeFederationViewKey(keyRune('n'))

	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectLocalProject, out.federationMode)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "create new local replica from hub project",
		"the replica path must stay reachable with an active project")
	rows := federationLocalProjectRows(out)
	require.Greater(t, len(rows), out.federationLocalProjectCursor)
	cursorRow := rows[out.federationLocalProjectCursor]
	require.False(t, cursorRow.createReplica, "cursor should pre-position on the active project")
	assert.Equal(t, "spoke-project", cursorRow.project.Name)

	// One Enter keeps the previous adopt ergonomics.
	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectHub, out.federationMode)
	assert.Equal(t, int64(7), out.federationDraft.SpokeProjectID)
	assert.Equal(t, "spoke-project", out.federationDraft.SpokeProjectName)
	assert.True(t, out.federationDraft.AdoptExisting)
	rendered = stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "Select hub daemon")
	assert.Contains(t, rendered, "hub https://hub.example auth token")
	assert.Equal(t, "operator", out.federationDraft.RequestedActor)
}

// TestFederationEnroll_ScopedProjectAdoptableWithEmptyProjectCache: the boot
// project-list fetch is asynchronous and can fail, so the scoped project must
// be adoptable from scope state alone — an empty projectsByID cache must not
// reduce the enroll flow to "create replica" only.
func TestFederationEnroll_ScopedProjectAdoptableWithEmptyProjectCache(t *testing.T) {
	m := setupFederationView()
	m.scope = homedScope(7, "spoke-project")
	// No injectProjects: simulate pressing `n` before projectsLoadedMsg (or
	// after a failed boot fetch).

	out, cmd := m.routeFederationViewKey(keyRune('n'))

	require.Nil(t, cmd)
	require.Equal(t, federationModeSelectLocalProject, out.federationMode)
	rows := federationLocalProjectRows(out)
	require.Greater(t, len(rows), out.federationLocalProjectCursor)
	cursorRow := rows[out.federationLocalProjectCursor]
	require.False(t, cursorRow.createReplica,
		"scoped project must be selectable (and pre-positioned) without the project cache")
	assert.Equal(t, "spoke-project", cursorRow.project.Name)
	assert.Equal(t, int64(7), cursorRow.project.ID)
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
	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	require.Equal(t, federationModeSelectHub, out.federationMode)

	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEsc})

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

	out, cmd := enterThroughAdoptConfirm(t, m)

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
	injectProjects(&m, mockProject{ID: 7, Name: "spoke-project"})
	m.daemonTargets = []daemonTarget{
		{Name: "spoke", URL: "https://spoke.example", Token: "spoke-auth"},
		{Name: "hub", URL: "https://hub.example", Token: "hub-auth"},
	}
	m.activeDaemon = m.daemonTargets[0]

	out, _ := m.routeFederationViewKey(keyRune('n'))
	out, _ = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // adopt the pre-cursored active project
	out.federationHubCursor = 1
	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	out = out.handleFederationHubProjectsLoaded(cmd().(federationHubProjectsLoadedMsg))

	rendered := stripANSI(renderFederation(out))
	assert.Equal(t, "hub-operator", out.federationDraft.HubInstance.Auth.Actor)
	assert.Contains(t, rendered, "hub auth: token actor hub-operator")

	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
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

	out, cmd := enterThroughAdoptConfirm(t, m)

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

	out, cmd := enterThroughAdoptConfirm(t, m)

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
	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectHub, out.federationMode)
	out.federationHubCursor = 1
	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	out = out.handleFederationHubProjectsLoaded(federationHubProjectsLoadedMsg{
		connGen:  out.connGen,
		gen:      out.federationEnrollGen,
		target:   out.federationDraft.HubTarget,
		projects: []ProjectSummary{{ID: 42, Name: "hub-project"}},
	})

	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})

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

	out, cmd := enterThroughAdoptConfirm(t, m)

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

	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	assert.Equal(t, federationModePreview, out.federationMode)
}

func TestFederationEnroll_MissingTokenEnvBlocksBeforeMutation(t *testing.T) {
	t.Setenv(missingHubAuthEnvName(), "")
	m := setupFederationView()
	m.scope = homedScope(7, "spoke-project")
	injectProjects(&m, mockProject{ID: 7, Name: "spoke-project"})
	m.daemonTargets = []daemonTarget{
		{Name: "spoke", URL: "https://spoke.example", Token: "spoke-auth"},
		{Name: "hub", URL: "https://hub.example", TokenEnv: missingHubAuthEnvName()},
	}
	m.activeDaemon = m.daemonTargets[0]

	out, _ := m.routeFederationViewKey(keyRune('n'))
	out, _ = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // adopt the pre-cursored active project
	out.federationHubCursor = 1
	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectHub, out.federationMode)
	assert.Contains(t, stripANSI(renderFederation(out)), "token_env")
}

func TestFederationEnroll_ActiveDaemonAsHubBlocked(t *testing.T) {
	m := setupFederationView()
	m.scope = homedScope(7, "spoke-project")
	injectProjects(&m, mockProject{ID: 7, Name: "spoke-project"})
	m.daemonTargets = []daemonTarget{{Name: "spoke", URL: "https://spoke.example", Token: "spoke-auth"}}
	m.activeDaemon = m.daemonTargets[0]

	out, _ := m.routeFederationViewKey(keyRune('n'))
	out, _ = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // adopt the pre-cursored active project
	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})

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

	out, cmd := enterThroughAdoptConfirm(t, m)

	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectHub, out.federationMode)
	assert.Empty(t, out.federationDraft.HubTarget.Name)
	require.Error(t, out.federationEnrollErr)
	assert.Contains(t, out.federationEnrollErr.Error(), "local hub")
}

func TestFederationEnroll_PlainHTTPHostnameRequiresCatalogAllowInsecure(t *testing.T) {
	m := setupFederationView()
	m.scope = homedScope(7, "spoke-project")
	injectProjects(&m, mockProject{ID: 7, Name: "spoke-project"})
	m.daemonTargets = []daemonTarget{
		{Name: "spoke", URL: "https://spoke.example", Token: "spoke-auth"},
		{Name: "hub", URL: "http://hub.internal:7777", Token: "hub-auth"},
	}
	m.activeDaemon = m.daemonTargets[0]

	out, _ := m.routeFederationViewKey(keyRune('n'))
	out, _ = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // adopt the pre-cursored active project
	out.federationHubCursor = 1
	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Contains(t, stripANSI(renderFederation(out)), "allow_insecure")
}

func TestFederationEnroll_EnterCreatesEnrollmentAndJoinsSpoke(t *testing.T) {
	m, joinBody := setupFederationExecutionPreview(t, federationExecutionServerOptions{})

	out, cmd := enterThroughAdoptConfirm(t, m)
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

	out, cmd := enterThroughAdoptConfirm(t, m)
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
	out, cmd := enterThroughAdoptConfirm(t, m)
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

	out, cmd := enterThroughAdoptConfirm(t, m)
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

	out, cmd := enterThroughAdoptConfirm(t, m)
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
	out, cmd := enterThroughAdoptConfirm(t, m)
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

	out, cmd := enterThroughAdoptConfirm(t, m)
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

	out, cmd := enterThroughAdoptConfirm(t, m)
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
	out, cmd := enterThroughAdoptConfirm(t, m)
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
	out, cmd := enterThroughAdoptConfirm(t, m)
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
	out, cmd := enterThroughAdoptConfirm(t, m)
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

func TestFederationLeaveKeyOpensPreviewOnSpokeRowOnly(t *testing.T) {
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.federationCursor = 0

	out, cmd := m.routeFederationViewKey(keyRune('x'))

	require.Nil(t, cmd)
	assert.Equal(t, federationModeLeavePreview, out.federationMode)
	assert.Equal(t, "spoke-proj", out.federationLeaveDraft.ProjectName)
	assert.Equal(t, "http://hub.internal:7777", out.federationLeaveDraft.HubURL)
	assert.Equal(t, int64(42), out.federationLeaveDraft.HubProjectID)
	assert.Equal(t, int64(7), out.federationLeaveDraft.ProjectID)
	assert.Equal(t, "detach", out.federationLeaveDraft.Disposition)
	assert.Empty(t, out.federationLeaveDraft.BlockedReason)
}

func TestFederationLeaveKeyDoesNotOpenPreviewOnNonSpokeRow(t *testing.T) {
	// The list only ever shows spoke rows, but the guard is the contract:
	// a non-spoke selection must never enter leave preview. Drive the guard
	// directly by exercising the detail router on a hub status.
	m := setupFederationViewWithStatuses(federationStatusFixture("hub-only", "hub"))
	m.federationCursor = 0
	m.federationMode = federationModeDetail

	out, cmd := m.routeFederationDetailKey(keyRune('x'))

	require.Nil(t, cmd)
	assert.NotEqual(t, federationModeLeavePreview, out.federationMode)
	assert.Equal(t, federationModeDetail, out.federationMode)
}

func TestFederationLeavePreviewRender(t *testing.T) {
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.federationCursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))

	rendered := stripANSI(renderFederation(out))

	assert.Contains(t, rendered, "Leave Federation")
	assert.Contains(t, rendered, "spoke-proj")
	assert.Contains(t, rendered, "hub.internal:7777")
	assert.Contains(t, rendered, "detach")
	assert.Contains(t, rendered, "revoke")
	assert.Contains(t, rendered, "[enter] confirm")
}

func TestFederationLeaveListFooterAdvertisesLeaveKey(t *testing.T) {
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))

	rendered := stripANSI(renderFederation(m))

	assert.Contains(t, rendered, "[x] leave")
}

func TestFederationLeavePreviewTogglesDispositionAndLocalOnly(t *testing.T) {
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.federationCursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))
	require.Equal(t, "detach", out.federationLeaveDraft.Disposition)

	out, cmd := out.routeFederationViewKey(keyRune('d'))
	require.Nil(t, cmd)
	assert.Equal(t, "archive", out.federationLeaveDraft.Disposition)

	out, cmd = out.routeFederationViewKey(keyRune('d'))
	require.Nil(t, cmd)
	assert.Equal(t, "detach", out.federationLeaveDraft.Disposition)

	out, cmd = out.routeFederationViewKey(keyRune('l'))
	require.Nil(t, cmd)
	assert.True(t, out.federationLeaveDraft.LocalOnly)
	assert.Contains(t, stripANSI(renderFederation(out)), "local-only")

	out, cmd = out.routeFederationViewKey(keyRune('l'))
	require.Nil(t, cmd)
	assert.False(t, out.federationLeaveDraft.LocalOnly)
}

func TestFederationLeavePreviewEscReturnsToList(t *testing.T) {
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.federationCursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))
	require.Equal(t, federationModeLeavePreview, out.federationMode)

	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEsc})

	require.Nil(t, cmd)
	assert.Equal(t, federationModeList, out.federationMode)
}

func TestFederationLeaveEnterRevokesHubEnrollmentThenTearsDownSpoke(t *testing.T) {
	hubProject := int64(42)
	hub := &recordingFederationHubAdmin{
		enrollments: []FederationEnrollment{
			{ID: 11, SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA", ProjectID: &hubProject},
		},
	}
	restoreFederationHubAdminClient(t, func(
		_ context.Context,
		target daemonTarget,
	) (federationHubAdminAPI, daemonTarget, error) {
		return hub, target, nil
	})
	var leaveBody LeaveFederationReplicaInput
	spoke := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/federation/replicas/7/actions/leave": func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&leaveBody))
			// Mirror the real daemon's attributedActor requirement: an empty
			// actor with no auth principal is rejected with 400. The TUI must
			// thread the model actor so detach + archive attribution succeed.
			require.NotEmpty(t, leaveBody.Actor, "leave request body must carry an actor")
			respondJSON(t, w, api.LeaveFederationReplicaResultBody{Detached: true, Disposition: "detach"})
		},
	})

	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.api = NewClient(spoke.URL, spoke.Client())
	m.list.actor = "operator"
	m.federationCursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))
	require.Equal(t, federationModeLeavePreview, out.federationMode)

	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd().(federationLeaveResultMsg)
	out, refresh := updateModel(out, msg)

	require.NotNil(t, refresh)
	require.NoError(t, msg.err)
	assert.Equal(t, federationModeResult, out.federationMode)
	assert.Equal(t, []int64{11}, hub.revokedEnrollmentIDs)
	assert.Equal(t, "detach", leaveBody.Disposition)
	assert.Equal(t, "operator", leaveBody.Actor)
	assert.Equal(t, 1, out.federationLeaveResult.RevokedCount)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "Leave Result")
	assert.Contains(t, rendered, "detached")
	assert.Contains(t, rendered, "revoked 1 enrollment")
}

// TestFederationLeaveAbortsWhenOnlyForeignEnrollmentsMatchProject: when no
// active enrollment matches this spoke's instance UID but the hub project
// still has active project-scoped enrollment(s), the leave must abort before
// local teardown instead of treating zero matches as success — the instance
// UID can drift from the enrollment's (clone/import refresh or an explicit
// --spoke-instance enroll), and proceeding would strand a live token.
func TestFederationLeaveAbortsWhenOnlyForeignEnrollmentsMatchProject(t *testing.T) {
	hubProject := int64(42)
	hub := &recordingFederationHubAdmin{
		enrollments: []FederationEnrollment{
			// Active, project-scoped, but for a different spoke instance.
			{ID: 21, SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4FF", ProjectID: &hubProject},
		},
	}
	restoreFederationHubAdminClient(t, func(
		_ context.Context,
		target daemonTarget,
	) (federationHubAdminAPI, daemonTarget, error) {
		return hub, target, nil
	})
	realLeaveRan := false
	spoke := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/federation/replicas/7/actions/leave": func(w http.ResponseWriter, r *http.Request) {
			var body LeaveFederationReplicaInput
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			// The non-mutating preflight legitimately precedes the revoke;
			// only the real teardown must be blocked by the abort.
			if !body.Preflight {
				realLeaveRan = true
			}
			respondJSON(t, w, api.LeaveFederationReplicaResultBody{Detached: !body.Preflight, Disposition: "detach"})
		},
	})

	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.api = NewClient(spoke.URL, spoke.Client())
	m.list.actor = "operator"
	m.federationCursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))
	require.Equal(t, federationModeLeavePreview, out.federationMode)

	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd().(federationLeaveResultMsg)

	require.Error(t, msg.err)
	assert.Contains(t, msg.err.Error(), "#21", "the surviving enrollment ID must be named")
	assert.Empty(t, hub.revokedEnrollmentIDs, "a foreign-instance enrollment must not be auto-revoked")
	assert.False(t, realLeaveRan, "local teardown must not run after the abort")
	_ = out
}

// TestFederationLeaveDetachPreflightRefusalSkipsRevoke: detach leaves run the
// same daemon preflight as archive leaves — the route can refuse a detach too
// (role drift, vanished project, actor validation), and a refusal discovered
// only after the hub revoke would strand the spoke locally bound with the hub
// side gone.
func TestFederationLeaveDetachPreflightRefusalSkipsRevoke(t *testing.T) {
	hubProject := int64(42)
	hub := &recordingFederationHubAdmin{
		enrollments: []FederationEnrollment{
			{ID: 11, SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA", ProjectID: &hubProject},
		},
	}
	restoreFederationHubAdminClient(t, func(
		_ context.Context,
		target daemonTarget,
	) (federationHubAdminAPI, daemonTarget, error) {
		return hub, target, nil
	})
	realLeaveRan := false
	spoke := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/federation/replicas/7/actions/leave": func(w http.ResponseWriter, r *http.Request) {
			var body LeaveFederationReplicaInput
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			if body.Preflight {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"status":409,"error":{"code":"not_a_spoke","message":"federation binding is not a spoke"}}`))
				return
			}
			realLeaveRan = true
			respondJSON(t, w, api.LeaveFederationReplicaResultBody{Detached: true, Disposition: "detach"})
		},
	})

	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.api = NewClient(spoke.URL, spoke.Client())
	m.list.actor = "operator"
	m.federationCursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))
	require.Equal(t, federationModeLeavePreview, out.federationMode)
	require.NotEqual(t, "archive", out.federationLeaveDraft.Disposition, "this test covers the default detach path")

	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd().(federationLeaveResultMsg)

	require.Error(t, msg.err, "the refused detach must surface before any teardown")
	assert.Empty(t, hub.revokedEnrollmentIDs,
		"the hub enrollment must not be revoked when the local detach would be refused")
	assert.False(t, realLeaveRan, "the real leave must not run after a refused preflight")
	_ = out
}

// TestFederationLeaveArchivePreflightRefusalSkipsRevoke: an archive-leave
// whose archive would be refused (open issues) must fail BEFORE the hub
// revoke — otherwise the spoke is left locally bound with a revoked hub
// token, breaking sync until manual recovery.
func TestFederationLeaveArchivePreflightRefusalSkipsRevoke(t *testing.T) {
	hubProject := int64(42)
	hub := &recordingFederationHubAdmin{
		enrollments: []FederationEnrollment{
			{ID: 11, SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA", ProjectID: &hubProject},
		},
	}
	restoreFederationHubAdminClient(t, func(
		_ context.Context,
		target daemonTarget,
	) (federationHubAdminAPI, daemonTarget, error) {
		return hub, target, nil
	})
	realLeaveRan := false
	spoke := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/federation/replicas/7/actions/leave": func(w http.ResponseWriter, r *http.Request) {
			var body LeaveFederationReplicaInput
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			if body.Preflight {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"status":409,"error":{"code":"project_has_open_issues","message":"project has open issues"}}`))
				return
			}
			realLeaveRan = true
			respondJSON(t, w, api.LeaveFederationReplicaResultBody{Detached: true, Disposition: "archive", Archived: true})
		},
	})

	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.api = NewClient(spoke.URL, spoke.Client())
	m.list.actor = "operator"
	m.federationCursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))
	require.Equal(t, federationModeLeavePreview, out.federationMode)
	out, _ = out.routeFederationViewKey(keyRune('d')) // toggle disposition to archive
	require.Equal(t, "archive", out.federationLeaveDraft.Disposition)

	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd().(federationLeaveResultMsg)

	require.Error(t, msg.err, "the refused archive must surface as the leave error")
	assert.Contains(t, msg.err.Error(), "open issues")
	assert.Empty(t, hub.revokedEnrollmentIDs,
		"the hub enrollment must not be revoked when the archive would be refused")
	assert.False(t, realLeaveRan, "the real leave must not run after a refused preflight")
	_ = out
}

func TestFederationLeaveLocalOnlySkipsHubRevoke(t *testing.T) {
	hub := &recordingFederationHubAdmin{}
	restoreFederationHubAdminClient(t, func(
		_ context.Context,
		target daemonTarget,
	) (federationHubAdminAPI, daemonTarget, error) {
		return hub, target, nil
	})
	var leaveBody LeaveFederationReplicaInput
	spoke := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/federation/replicas/7/actions/leave": func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&leaveBody))
			// The local teardown route still goes through attributedActor on
			// the real daemon, so the threaded actor must be present even in
			// the local-only path.
			require.NotEmpty(t, leaveBody.Actor, "leave request body must carry an actor")
			respondJSON(t, w, api.LeaveFederationReplicaResultBody{Detached: true, Disposition: "detach"})
		},
	})

	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.api = NewClient(spoke.URL, spoke.Client())
	m.list.actor = "operator"
	m.federationCursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))
	out, _ = out.routeFederationViewKey(keyRune('l')) // toggle local-only
	require.True(t, out.federationLeaveDraft.LocalOnly)

	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd().(federationLeaveResultMsg)
	out, _ = updateModel(out, msg)

	require.NoError(t, msg.err)
	assert.Equal(t, federationModeResult, out.federationMode)
	assert.Equal(t, 0, hub.listEnrollmentsCalls)
	assert.Empty(t, hub.revokedEnrollmentIDs)
	assert.True(t, out.federationLeaveResult.SkippedRevoke)
	assert.Contains(t, stripANSI(renderFederation(out)), "hub revoke skipped")
}

func TestFederationLeaveHubRevokeFailureReturnsToPreview(t *testing.T) {
	restoreFederationHubAdminClient(t, func(
		_ context.Context,
		_ daemonTarget,
	) (federationHubAdminAPI, daemonTarget, error) {
		return nil, daemonTarget{}, errors.New("hub unavailable")
	})
	var leaveCalled bool
	spoke := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/federation/replicas/7/actions/leave": func(w http.ResponseWriter, r *http.Request) {
			var body LeaveFederationReplicaInput
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			// The non-mutating preflight legitimately precedes the revoke;
			// only the real teardown call must be blocked by the hub failure.
			if !body.Preflight {
				leaveCalled = true
			}
			respondJSON(t, w, api.LeaveFederationReplicaResultBody{Detached: !body.Preflight})
		},
	})

	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.api = NewClient(spoke.URL, spoke.Client())
	m.federationCursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))

	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd().(federationLeaveResultMsg)
	out, next := updateModel(out, msg)

	require.Nil(t, next)
	assert.False(t, leaveCalled, "local teardown must not run when hub revoke fails")
	assert.Equal(t, federationModeLeavePreview, out.federationMode)
	require.Error(t, out.federationEnrollErr)
	assert.Contains(t, out.federationEnrollErr.Error(), "hub revoke failed")
}

func TestFederationLeaveMatchesActiveEnrollmentsForSpokeInstanceAndHubProject(t *testing.T) {
	hubProject := int64(42)
	otherProject := int64(99)
	revoked := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	enrollments := []FederationEnrollment{
		{ID: 1, SpokeInstanceUID: "spoke-uid", ProjectID: &hubProject},
		{ID: 2, SpokeInstanceUID: "spoke-uid", ProjectID: &hubProject, RevokedAt: &revoked},
		{ID: 3, SpokeInstanceUID: "other-uid", ProjectID: &hubProject},
		{ID: 4, SpokeInstanceUID: "spoke-uid", ProjectID: &otherProject},
		{ID: 5, SpokeInstanceUID: "spoke-uid", ProjectID: nil},
	}

	got, globals, foreign := matchFederationLeaveEnrollments(enrollments, "spoke-uid", hubProject)

	assert.Equal(t, []int64{1}, got)
	assert.Equal(t, []int64{5}, globals,
		"active global enrollments for this spoke must be surfaced (they still authorize the project) without being auto-revoked")
	assert.Equal(t, []int64{3}, foreign,
		"active project-scoped enrollments for other spoke instances must be surfaced so a zero-match leave can refuse instead of stranding them")
}

func TestFederationLeaveHubTargetUsesBindingURLAndToleratesTrailingSlash(t *testing.T) {
	m := newTestModel()
	m.daemonTargets = []daemonTarget{
		{Name: "local", Local: true},
		// Catalog entry's URL carries a trailing slash and a foreign-looking
		// path; only its token must be reused.
		{Name: "hub", URL: "https://bound.example/", Token: "catalog-token"},
	}

	got := m.federationLeaveHubTarget("https://bound.example", true)

	// Token comes from the matched catalog entry (slash-tolerant match)...
	assert.Equal(t, "catalog-token", got.Token)
	// ...but the target URL is pinned to the binding's hub URL, normalized,
	// and allow_insecure comes from the binding, not the catalog entry.
	assert.Equal(t, "https://bound.example", got.URL)
	assert.True(t, got.AllowInsecure, "binding allow_insecure must carry through")
	assert.False(t, got.Implicit, "a matched catalog entry is not an implicit target")
}

// TestFederationLeaveHubTargetUnionsCatalogAllowInsecure: a same-origin
// catalog entry's allow_insecure is the operator's own transport opt-in and
// must be able to RESTORE the flag when the binding-side value was lost with
// the credential; the union means the catalog can add but never remove the
// binding's opt-in.
func TestFederationLeaveHubTargetUnionsCatalogAllowInsecure(t *testing.T) {
	m := newTestModel()
	m.daemonTargets = []daemonTarget{
		{Name: "hub", URL: "http://hub.internal:7373", Token: "catalog-token", AllowInsecure: true},
	}

	got := m.federationLeaveHubTarget("http://hub.internal:7373", false)

	assert.Equal(t, "catalog-token", got.Token)
	assert.True(t, got.AllowInsecure,
		"same-origin catalog allow_insecure must union into the leave hub target")
}

func TestFederationLeaveHubTargetNoMatchFallsBackToBindingURL(t *testing.T) {
	m := newTestModel()
	m.daemonTargets = []daemonTarget{{Name: "local", Local: true}}

	got := m.federationLeaveHubTarget("https://bound.example/", true)

	// The fallback must be unauthenticated and non-implicit: implicit targets
	// pick up the global daemon token, which must never go to the hub origin.
	assert.False(t, got.Implicit, "fallback must not be implicit (global-auth pickup)")
	assert.Equal(t, "https://bound.example", got.URL)
	assert.Empty(t, got.Token)
	assert.True(t, got.AllowInsecure, "binding allow_insecure must carry through")
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
		ActiveQuarantines: []api.FederationQuarantineSummary{{
			ID:           7,
			Direction:    "push",
			FirstEventID: 11,
			LastEventID:  13,
			EventUIDs: []string{
				"01HZNQ7VFPK1XGD8R5MABCD4EA",
				"01HZNQ7VFPK1XGD8R5MABCD4EB",
				"01HZNQ7VFPK1XGD8R5MABCD4EC",
			},
			Error:     "hub rejected deferred peer",
			CreatedAt: last.Add(time.Minute),
		}},
		ResetBlocker:             "pending push",
		UnresolvedViolationCount: 2,
		RecentViolationCount:     2,
		LastSuccessfulSyncAt:     &last,
		LastPullSuccessAt:        &last,
		LastPushSuccessAt:        &last,
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
	enrollments           []FederationEnrollment
	getInstanceCalls      int
	listProjectsCalls     int
	ensureProjectCalls    int
	enableFederationCalls int
	createEnrollmentCalls int
	listEnrollmentsCalls  int
	revokedEnrollmentIDs  []int64
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

func (h *recordingFederationHubAdmin) ListFederationEnrollments(
	_ context.Context,
) ([]FederationEnrollment, error) {
	h.listEnrollmentsCalls++
	return h.enrollments, nil
}

func (h *recordingFederationHubAdmin) RevokeFederationEnrollment(
	_ context.Context,
	enrollmentID int64,
) error {
	h.revokedEnrollmentIDs = append(h.revokedEnrollmentIDs, enrollmentID)
	return nil
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

// TestFederationRecoveryShowsRealJoinError: the recovery screen must print the
// actual join error instead of hiding it behind the canned token guess. A
// non-auth failure (here the rejoin name-mismatch 409) must not be blamed on
// the enrollment token.
func TestFederationRecoveryShowsRealJoinError(t *testing.T) {
	m := setupFederationView()
	m.federationMode = federationModeRecovery
	m.federationRecovery = federationRecovery{
		Stage: "join",
		Err: fmt.Errorf("spoke: join failed: %w", &APIError{
			Status:  409,
			Code:    "federation_rejoin_name_mismatch",
			Message: `hub project UID is held by local project "spoke-project", which previously left this federation; rerun join with --project "spoke-project" to rejoin it`,
		}),
	}

	rendered := stripANSI(renderFederation(m))
	assert.Contains(t, rendered, "federation_rejoin_name_mismatch")
	assert.Contains(t, rendered, "previously left")
	assert.NotContains(t, rendered, "may be single-use",
		"a non-auth failure must not be blamed on the token")
}

// TestFederationRecoveryKeepsTokenHintForAuthFailures: 401/403 failures keep
// the token-oriented hint alongside the real error.
func TestFederationRecoveryKeepsTokenHintForAuthFailures(t *testing.T) {
	m := setupFederationView()
	m.federationMode = federationModeRecovery
	m.federationRecovery = federationRecovery{
		Stage: "join",
		Err:   fmt.Errorf("spoke: join failed: %w", &APIError{Status: 401, Code: "unauthorized", Message: "bearer token rejected"}),
	}

	rendered := stripANSI(renderFederation(m))
	assert.Contains(t, rendered, "bearer token rejected")
	assert.Contains(t, rendered, "may be single-use")
}

// TestFederationPreviewDetectsRejoinForUIDHolder: selecting a hub project
// whose UID is already held by an unbound local project must present the
// operation as a rejoin of that project (it previously left this federation),
// not as a new local replica that would dead-end on the daemon.
func TestFederationPreviewDetectsRejoinForUIDHolder(t *testing.T) {
	m := setupFederationView()
	m.projectsByID = map[int64]string{7: "spoke-project"}
	m.projectUIDByID = map[int64]string{7: "01HZNQ7VFPK1XGD8R5MABCD4EX"}
	m.federationDraft = federationDraft{CreateReplica: true}
	m.federationHubProjects = []ProjectSummary{{ID: 42, Name: "hub-project", UID: "01HZNQ7VFPK1XGD8R5MABCD4EX"}}
	m.federationHubProjectCursor = 0

	out, _ := m.previewFederationEnrollment()

	draft := out.federationDraft
	assert.Equal(t, federationOperationRejoin, draft.Operation)
	assert.Equal(t, "spoke-project", draft.SpokeProjectName, "rejoin must target the local UID-holder")
	assert.False(t, draft.AdoptExisting)
	assert.Empty(t, draft.BlockedReason)
	assert.Equal(t, "spoke-project", federationReplicaProjectName(draft, "hub-project"),
		"the join must name the holder, not the hub project")
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "rejoin")
	assert.Contains(t, rendered, "previously left")
}

// TestFederationPreviewBlocksRejoinWhenHolderStillBound: a UID-holder that
// still has a live binding is not silently rebound; the preview is blocked
// with a message naming it.
func TestFederationPreviewBlocksRejoinWhenHolderStillBound(t *testing.T) {
	m := setupFederationView()
	m.projectsByID = map[int64]string{7: "spoke-project"}
	m.projectUIDByID = map[int64]string{7: "01HZNQ7VFPK1XGD8R5MABCD4EX"}
	m.federationStatuses = []FederationProjectStatus{{ProjectID: 7, ProjectName: "spoke-project", Role: "spoke"}}
	m.federationDraft = federationDraft{CreateReplica: true}
	m.federationHubProjects = []ProjectSummary{{ID: 42, Name: "hub-project", UID: "01HZNQ7VFPK1XGD8R5MABCD4EX"}}
	m.federationHubProjectCursor = 0

	out, _ := m.previewFederationEnrollment()

	assert.NotEqual(t, federationOperationRejoin, out.federationDraft.Operation)
	assert.Contains(t, out.federationDraft.BlockedReason, "spoke-project")
}

// TestFederationPreviewCreateReplicaUnaffectedWithoutUIDMatch: no local
// UID-holder means the plain create-replica path is unchanged.
func TestFederationPreviewCreateReplicaUnaffectedWithoutUIDMatch(t *testing.T) {
	m := setupFederationView()
	m.projectsByID = map[int64]string{7: "other-project"}
	m.projectUIDByID = map[int64]string{7: "01HZNQ7VFPK1XGD8R5MABCD4ZZ"}
	m.federationDraft = federationDraft{CreateReplica: true}
	m.federationHubProjects = []ProjectSummary{{ID: 42, Name: "hub-project", UID: "01HZNQ7VFPK1XGD8R5MABCD4EX"}}
	m.federationHubProjectCursor = 0

	out, _ := m.previewFederationEnrollment()

	assert.Equal(t, federationOperationCreateReplica, out.federationDraft.Operation)
	assert.Empty(t, out.federationDraft.BlockedReason)
}

// typeFederationKeys feeds individual rune key presses into the federation
// view router.
func typeFederationKeys(m Model, text string) Model {
	out := m
	for _, r := range text {
		out, _ = out.routeFederationViewKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return out
}

// enterThroughAdoptConfirm presses Enter on the federation view; when that
// lands in the adoption typed-confirmation gate it types the local project
// name and confirms. Non-adopt presses pass through unchanged.
func enterThroughAdoptConfirm(t *testing.T, m Model) (Model, tea.Cmd) {
	t.Helper()
	out, cmd := m.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if out.federationMode != federationModeAdoptConfirm {
		return out, cmd
	}
	require.Nil(t, cmd, "entering the adopt confirmation must not execute")
	out = typeFederationKeys(out, out.federationDraft.SpokeProjectName)
	return out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
}

// TestFederationAdoptEnterOpensTypedConfirmation: adoption never executes on a
// bare Enter; the gate states the INTO relationship, a wrong name keeps it
// gated, and Esc returns to the preview.
func TestFederationAdoptEnterOpensTypedConfirmation(t *testing.T) {
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{})

	out, cmd := m.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd, "adoption must not execute on bare enter")
	assert.Equal(t, federationModeAdoptConfirm, out.federationMode)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "INTO hub project")
	assert.Contains(t, rendered, "spoke-project")

	out = typeFederationKeys(out, "wrong-name")
	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	assert.Equal(t, federationModeAdoptConfirm, out.federationMode)
	assert.False(t, out.federationEnrollRunning)
	require.Error(t, out.federationEnrollErr)

	out, _ = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	assert.Equal(t, federationModePreview, out.federationMode)
}

// TestFederationAdoptConfirmTypesSpaces: project names may contain spaces
// (ValidateProjectName rejects only non-printables), so the typed
// confirmation gate must accept the space key in every event shape: unix
// terminals deliver KeySpace WITH Runes{' '} (bubbletea v1.3.10 keeps the
// rune "for backwards compatibility"), Windows delivers KeyRunes, and a
// runeless KeySpace — a hand-built message or a future input backend — must
// not silently drop the character.
func TestFederationAdoptConfirmTypesSpaces(t *testing.T) {
	newConfirm := func() Model {
		m := setupFederationView()
		m.federationMode = federationModeAdoptConfirm
		m.federationDraft = newFederationDraft("operator")
		m.federationDraft.SpokeProjectName = "spoke project"
		return m
	}

	t.Run("runeless KeySpace appends a space", func(t *testing.T) {
		m := typeFederationKeys(newConfirm(), "spoke")
		m, _ = m.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
		m = typeFederationKeys(m, "project")
		assert.Equal(t, "spoke project", m.federationAdoptConfirmInput)
	})

	t.Run("unix-shape KeySpace with rune appends one space", func(t *testing.T) {
		m := typeFederationKeys(newConfirm(), "spoke")
		m, _ = m.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
		m = typeFederationKeys(m, "project")
		assert.Equal(t, "spoke project", m.federationAdoptConfirmInput)
	})
}

// TestFederationAdoptTypedConfirmationExecutes: typing the exact project name
// and confirming runs the enrollment.
func TestFederationAdoptTypedConfirmationExecutes(t *testing.T) {
	m, joinBody := setupFederationExecutionPreview(t, federationExecutionServerOptions{})

	out, cmd := enterThroughAdoptConfirm(t, m)
	require.NotNil(t, cmd)
	assert.True(t, out.federationEnrollRunning)
	msg := cmd().(federationEnrollResultMsg)
	out, _ = updateModel(out, msg)
	assert.Equal(t, federationModeResult, out.federationMode)
	assert.True(t, joinBody.AdoptExisting)
}

// TestFederationPreviewAdoptFlowBlocksWithoutLocalUID: when the selected
// local project's UID is not yet known (the async project-list fetch has not
// landed, or it failed), the adopt-first preview cannot distinguish genuine
// adoption from a post-leave rejoin. It must block rather than default to
// adoption, which would rewrite the project's event history.
func TestFederationPreviewAdoptFlowBlocksWithoutLocalUID(t *testing.T) {
	m := setupFederationView()
	m.projectsByID = map[int64]string{7: "spoke-project"}
	// projectUIDByID intentionally unseeded: the boot race window.
	m.federationDraft = newFederationDraft("operator")
	m.federationDraft.SpokeProjectID = 7
	m.federationDraft.SpokeProjectName = "spoke-project"
	m.federationHubProjects = []ProjectSummary{{ID: 42, Name: "spoke-project", UID: "01HZNQ7VFPK1XGD8R5MABCD4EX"}}
	m.federationHubProjectCursor = 0 // adopt-same-name row

	out, _ := m.previewFederationEnrollment()

	draft := out.federationDraft
	assert.NotEmpty(t, draft.BlockedReason,
		"unknown local project UID must block adoption, not silently proceed with it")
	assert.NotEqual(t, federationOperationRejoin, draft.Operation)
}

// TestFederationPreviewAdoptFlowBlocksWithoutHubUID: an unknown HUB project
// UID disables the rejoin comparison just like an unknown local UID — the
// preview must block rather than default to adoption, which could rewrite a
// post-leave project's event history.
func TestFederationPreviewAdoptFlowBlocksWithoutHubUID(t *testing.T) {
	m := setupFederationView()
	m.projectsByID = map[int64]string{7: "spoke-project"}
	m.projectUIDByID = map[int64]string{7: "01HZNQ7VFPK1XGD8R5MABCD4EX"}
	m.federationDraft = newFederationDraft("operator")
	m.federationDraft.SpokeProjectID = 7
	m.federationDraft.SpokeProjectName = "spoke-project"
	// Hub row carries no UID (e.g. an older hub daemon's project list).
	m.federationHubProjects = []ProjectSummary{{ID: 42, Name: "spoke-project"}}
	m.federationHubProjectCursor = 0 // adopt-same-name row

	out, _ := m.previewFederationEnrollment()

	draft := out.federationDraft
	assert.NotEmpty(t, draft.BlockedReason,
		"unknown hub project UID must block adoption, not silently proceed with it")
	assert.NotEqual(t, federationOperationRejoin, draft.Operation)
}

// TestFederationPreviewAdoptFlowDetectsRejoinForUIDHolder: selecting a local
// project that already shares the target hub project's identity (the
// post-leave state) must present rejoin, not adoption — adoption would
// rewrite the project's event history a second time. Covers the adopt-flow
// branch (local project picked first), which the create-replica-branch
// detection missed.
func TestFederationPreviewAdoptFlowDetectsRejoinForUIDHolder(t *testing.T) {
	m := setupFederationView()
	m.projectsByID = map[int64]string{7: "spoke-project"}
	m.projectUIDByID = map[int64]string{7: "01HZNQ7VFPK1XGD8R5MABCD4EX"}
	m.federationDraft = newFederationDraft("operator")
	m.federationDraft.SpokeProjectID = 7
	m.federationDraft.SpokeProjectName = "spoke-project"
	m.federationHubProjects = []ProjectSummary{{ID: 42, Name: "spoke-project", UID: "01HZNQ7VFPK1XGD8R5MABCD4EX"}}
	m.federationHubProjectCursor = 0 // adopt-same-name row

	out, _ := m.previewFederationEnrollment()

	draft := out.federationDraft
	assert.Equal(t, federationOperationRejoin, draft.Operation)
	assert.False(t, draft.AdoptExisting, "rejoin must not request adoption snapshots")
	assert.Equal(t, int64(42), draft.HubProjectID)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "previously left")
	assert.NotContains(t, rendered, "adoption warning")
}

// TestFederationPreviewAdoptFlowStaysAdoptWithoutUIDMatch: a local project
// with its own identity adopts normally.
func TestFederationPreviewAdoptFlowStaysAdoptWithoutUIDMatch(t *testing.T) {
	m := setupFederationView()
	m.projectsByID = map[int64]string{7: "spoke-project"}
	m.projectUIDByID = map[int64]string{7: "01HZNQ7VFPK1XGD8R5MABCD4ZZ"}
	m.federationDraft = newFederationDraft("operator")
	m.federationDraft.SpokeProjectID = 7
	m.federationDraft.SpokeProjectName = "spoke-project"
	m.federationHubProjects = []ProjectSummary{{ID: 42, Name: "spoke-project", UID: "01HZNQ7VFPK1XGD8R5MABCD4EX"}}
	m.federationHubProjectCursor = 0

	out, _ := m.previewFederationEnrollment()

	assert.Equal(t, federationOperationAdoptSameName, out.federationDraft.Operation)
	assert.True(t, out.federationDraft.AdoptExisting)
}

// TestFetchProjectsCarriesUIDs is the rejoin-detection prerequisite: the
// boot/refresh list fetch must populate projectUIDByID, otherwise
// previewFederationEnrollment cannot recognize a UID-holder and silently
// degrades a rejoin into history-rewriting adoption.
func TestFetchProjectsCarriesUIDs(t *testing.T) {
	srv := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/projects": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"projects":[{"id":7,"uid":"01HZNQ7VFPK1XGD8R5MABCD4EX","name":"spoke-project"}]}`))
		},
	})
	m := newTestModel()
	m.api = NewClient(srv.URL, srv.Client())

	msg, ok := m.fetchProjects()().(projectsLoadedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(t, "spoke-project", msg.projects[7])
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EX", msg.uids[7],
		"fetchProjects must carry project UIDs for rejoin detection")

	// And the loaded handler must apply them to projectUIDByID.
	out, _ := updateModel(m, msg)
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EX", out.projectUIDByID[7])
}
