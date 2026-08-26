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
	clientpkg "go.kenn.io/kata/internal/client"
)

func TestFederationView_FKeyTransitionsFromList(t *testing.T) {
	m := setupFederationSourceModel()

	out, cmd := updateModel(m, keyRune('F'))

	assert.Equal(t, viewFederation, out.view)
	assert.Equal(t, viewList, out.prevView)
	require.NotNil(t, cmd)
}

// TestFederationView_FKeyInvalidatesPriorOperationResults exercises the
// global shortcut rather than calling transitionToFederation directly. A
// fresh federation list must own neither an enrollment nor leave operation
// retained from the previous view, so either late reply is discarded instead
// of replacing the list with an unrelated result screen.
func TestFederationView_FKeyInvalidatesPriorOperationResults(t *testing.T) {
	tests := []struct {
		name string
		kind federationOpKind
		late tea.Msg
	}{
		{
			name: "enroll",
			kind: federationOpEnroll,
			late: federationEnrollResultMsg{
				attempt: 7,
				result:  federationEnrollResult{HubURL: "https://hub.example"},
			},
		},
		{
			name: "leave",
			kind: federationOpLeave,
			late: federationLeaveResultMsg{
				attempt: 7,
				result: federationLeaveResult{
					Draft: federationLeaveDraft{ProjectName: "spoke-project"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := setupFederationSourceModel()
			m.view = viewProjects
			m.federation.op = federationOp{kind: tt.kind, attempt: 7, running: true}

			out, fetchCmd := updateModel(m, keyRune('F'))
			require.NotNil(t, fetchCmd)
			require.Equal(t, viewFederation, out.view)
			require.Equal(t, federationModeList, out.federation.mode)
			assert.Greater(t, out.federation.op.attempt, uint64(7))
			assert.Equal(t, federationOpNone, out.federation.op.kind)
			assert.False(t, out.federation.op.running)

			out, cmd := updateModel(out, tt.late)
			require.Nil(t, cmd, "a late result must not trigger a status refetch")
			assert.Equal(t, viewFederation, out.view)
			assert.Equal(t, federationModeList, out.federation.mode)
			assert.Equal(t, federationOpNone, out.federation.op.kind)
		})
	}
}

func TestFederationView_EscReturnsToPreviousView(t *testing.T) {
	m := setupFederationView()

	out, cmd := m.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEsc})

	require.Nil(t, cmd)
	assert.Equal(t, viewList, out.view)
}

func TestFederationView_EnterOpensSelectedStatusDetail(t *testing.T) {
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.federation.cursor = 0

	out, cmd := enterThroughAdoptConfirm(t, m)

	require.Nil(t, cmd)
	assert.Equal(t, federationModeDetail, out.federation.mode)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "hub project UID")
	assert.Contains(t, rendered, "pull cursor")
	assert.Contains(t, rendered, "credential")
	assert.Contains(t, rendered, "quarantine #7: push events 11-13")
	assert.Contains(t, rendered, "hub rejected deferred peer")
}

func TestFederationView_RenderIncludesActiveSpokeStatus(t *testing.T) {
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.federation.instance.Auth = AuthInfo{Kind: "db_token", Actor: "operator"}
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
	m.federation.instance.Auth = AuthInfo{Kind: "db_token", Actor: "operator"}

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
	m.federation.mode = federationModeDetail

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
	assert.Equal(t, 0, out.federation.cursor)
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
	spokeTarget := daemonTargetWithResolvedAuth("spoke", "https://spoke.example", "spoke-auth", false)
	hubTarget := daemonTargetWithResolvedAuth("catalog-hub", "https://hub.example", "hub-auth", false)
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
	m.federation.hubCursor = 1
	initialScope := m.scope
	initialSSE := m.sseCh
	initialConnGen := m.connGen
	initialSwitchAttempt := m.daemonSwitchAttempt

	out, cmd := m.routeFederationViewKey(keyRune('b'))
	require.NotNil(t, cmd)
	assert.Equal(t, federationModeBrowseHubs, out.federation.mode)

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
	hubTarget := daemonTargetWithResolvedAuth("catalog-hub", "https://hub.example", "hub-auth", false)
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
	m.activeDaemon = daemonTargetWithResolvedAuth("spoke", "https://spoke.example", "spoke-auth", false)
	m.daemonTargets = []daemonTarget{m.activeDaemon, hubTarget}
	m.federation.hubCursor = 1

	out, cmd := m.routeFederationViewKey(keyRune('b'))
	require.NotNil(t, cmd)
	out, _ = updateModel(out, cmd().(federationHubProjectsLoadedMsg))

	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Equal(t, federationModeBrowseHubs, out.federation.mode)
	assert.Equal(t, 0, hub.ensureProjectCalls)
	assert.Equal(t, 0, hub.enableFederationCalls)
	assert.Equal(t, 0, hub.createEnrollmentCalls)
	assert.NotEqual(t, federationModePreview, out.federation.mode)
	assert.Empty(t, out.federation.recovery.Token)
	assert.Empty(t, out.federation.recovery.Command.Token)
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
		daemonTargetWithResolvedAuth("spoke", "https://spoke.example", "spoke-auth", false),
		daemonTargetWithResolvedAuth("hub", "https://hub.example", "hub-auth", false),
	}
	m.activeDaemon = m.daemonTargets[0]

	out, cmd := m.routeFederationViewKey(keyRune('n'))

	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectLocalProject, out.federation.mode)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "create new local replica from hub project",
		"the replica path must stay reachable with an active project")
	rows := federationLocalProjectRows(out)
	require.Greater(t, len(rows), out.federation.localProjectCursor)
	cursorRow := rows[out.federation.localProjectCursor]
	require.False(t, cursorRow.createReplica, "cursor should pre-position on the active project")
	assert.Equal(t, "spoke-project", cursorRow.project.Name)

	// One Enter keeps the previous adopt ergonomics.
	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectHub, out.federation.mode)
	assert.Equal(t, int64(7), out.federation.draft.SpokeProjectID)
	assert.Equal(t, "spoke-project", out.federation.draft.SpokeProjectName)
	assert.True(t, out.federation.draft.AdoptExisting)
	rendered = stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "Select hub daemon")
	assert.Contains(t, rendered, "hub https://hub.example auth token")
	assert.Equal(t, "operator", out.federation.draft.RequestedActor)
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
	require.Equal(t, federationModeSelectLocalProject, out.federation.mode)
	rows := federationLocalProjectRows(out)
	require.Greater(t, len(rows), out.federation.localProjectCursor)
	cursorRow := rows[out.federation.localProjectCursor]
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
	assert.Equal(t, federationModeSelectLocalProject, out.federation.mode)
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
	out.federation.localProjectCursor = 1
	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	require.Equal(t, federationModeSelectHub, out.federation.mode)

	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEsc})

	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectLocalProject, out.federation.mode)
}

func TestFederationEnroll_SelectHubThenSelectSameNameHubProjectPreview(t *testing.T) {
	m := setupFederationHubProjectSelection()
	m.federation.draft.HubInstance = InstanceInfo{Auth: AuthInfo{Kind: "db_token", Actor: "hub-operator"}}
	m.federation.hubProjects = []ProjectSummary{{ID: 42, Name: "spoke-project"}}

	renderedSelection := stripANSI(renderFederation(m))
	assert.Contains(t, renderedSelection, "hub auth: token actor hub-operator")
	assert.Contains(t, renderedSelection, `use existing hub project "spoke-project"; enable federation if needed`)
	assert.NotContains(t, renderedSelection, "will be created if missing")
	assert.NotContains(t, renderedSelection, "\n  spoke-project\n")

	out, cmd := enterThroughAdoptConfirm(t, m)

	require.Nil(t, cmd)
	assert.Equal(t, federationModePreview, out.federation.mode)
	assert.Equal(t, federationOperationAdoptSameName, out.federation.draft.Operation)
	assert.Equal(t, int64(42), out.federation.draft.HubProjectID)
	assert.Equal(t, "claim,pull,push", out.federation.draft.APICapabilities)
	assert.Equal(t, "pull,push,lease", out.federation.draft.DisplayCapabilities)
	assert.True(t, out.federation.draft.AdoptExisting)
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
		daemonTargetWithResolvedAuth("spoke", "https://spoke.example", "spoke-auth", false),
		daemonTargetWithResolvedAuth("hub", "https://hub.example", "hub-auth", false),
	}
	m.activeDaemon = m.daemonTargets[0]

	out, _ := m.routeFederationViewKey(keyRune('n'))
	out, _ = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // adopt the pre-cursored active project
	out.federation.hubCursor = 1
	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	out = out.handleFederationHubProjectsLoaded(cmd().(federationHubProjectsLoadedMsg))

	rendered := stripANSI(renderFederation(out))
	assert.Equal(t, "hub-operator", out.federation.draft.HubInstance.Auth.Actor)
	assert.Contains(t, rendered, "hub auth: token actor hub-operator")

	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	rendered = stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "requested actor: hub-operator")
	assert.NotContains(t, rendered, "requested actor: anonymous")
}

func TestFederationEnroll_SelectDifferentHubProjectSkipsSameNameDuplicate(t *testing.T) {
	m := setupFederationHubProjectSelection()
	m.federation.hubProjects = []ProjectSummary{
		{ID: 42, Name: "spoke-project"},
		{ID: 77, Name: "team-hub-project"},
	}
	m.federation.hubProjectCursor = 1

	out, cmd := enterThroughAdoptConfirm(t, m)

	require.Nil(t, cmd)
	assert.Equal(t, federationModePreview, out.federation.mode)
	assert.Equal(t, federationOperationAdoptSelectedHub, out.federation.draft.Operation)
	assert.Equal(t, int64(77), out.federation.draft.HubProjectID)
	assert.Equal(t, "team-hub-project", out.federation.draft.HubProjectName)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "team-hub-project")
}

func TestFederationEnroll_SelectDifferentExistingHubProjectStillAdoptsLocalProject(t *testing.T) {
	m := setupFederationHubProjectSelection()
	m.federation.hubProjects = []ProjectSummary{{ID: 42, Name: "hub-project"}}
	m.federation.hubProjectCursor = 1

	out, cmd := enterThroughAdoptConfirm(t, m)

	require.Nil(t, cmd)
	assert.Equal(t, federationModePreview, out.federation.mode)
	assert.Equal(t, federationOperationAdoptSelectedHub, out.federation.draft.Operation)
	assert.True(t, out.federation.draft.AdoptExisting)
	assert.Equal(t, int64(42), out.federation.draft.HubProjectID)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "Operation: adopt existing local project into selected hub project")
	assert.Contains(t, rendered, "hub-project")
}

func TestFederationEnroll_CreateReplicaBranchDefaultsLocalNameFromHubProject(t *testing.T) {
	m := setupFederationView()
	m.scope = scope{allProjects: true}
	m.daemonTargets = []daemonTarget{
		daemonTargetWithResolvedAuth("spoke", "https://spoke.example", "spoke-auth", false),
		daemonTargetWithResolvedAuth("hub", "https://hub.example", "hub-auth", false),
	}
	m.activeDaemon = m.daemonTargets[0]

	out, cmd := m.routeFederationViewKey(keyRune('n'))
	require.Nil(t, cmd)
	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectHub, out.federation.mode)
	out.federation.hubCursor = 1
	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	out = out.handleFederationHubProjectsLoaded(federationHubProjectsLoadedMsg{
		connGen:  out.connGen,
		gen:      out.federation.enrollGen,
		target:   out.federation.draft.HubTarget,
		projects: []ProjectSummary{{ID: 42, Name: "hub-project"}},
	})

	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Equal(t, federationModePreview, out.federation.mode)
	assert.Equal(t, federationOperationCreateReplica, out.federation.draft.Operation)
	assert.Equal(t, "hub-project", out.federation.draft.SpokeProjectName)
	assert.False(t, out.federation.draft.AdoptExisting)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "Operation: create new local replica from hub project")
	assert.NotContains(t, rendered, "pre-adoption event history")
}

func TestFederationEnroll_CreateReplicaBranchPreflightsLocalNameConflict(t *testing.T) {
	m := setupFederationView()
	m.scope = scope{allProjects: true}
	injectProjects(&m, mockProject{ID: 7, Name: "spoke-project"})
	m.federation.mode = federationModeSelectHubProject
	m.federation.draft = newFederationDraft("anonymous")
	m.federation.draft.CreateReplica = true
	m.federation.draft.HubTarget = daemonTarget{Name: "hub", URL: "https://hub.example"}
	m.federation.hubProjects = []ProjectSummary{{ID: 42, Name: "spoke-project"}}

	out, cmd := enterThroughAdoptConfirm(t, m)

	require.Nil(t, cmd)
	assert.Equal(t, federationModePreview, out.federation.mode)
	assert.False(t, out.federation.draft.AdoptExisting)
	assert.Contains(t, stripANSI(renderFederation(out)), `Blocked: local project "spoke-project" already exists`)
}

func TestFederationEnroll_SameNamePreviewClearsStaleSelectedHubProjectID(t *testing.T) {
	m := setupFederationHubProjectSelection()
	m.federation.draft.HubProjectID = 42
	m.federation.draft.HubProjectName = "old-hub-project"
	m.federation.hubProjectCursor = 0
	m.federation.hubProjects = []ProjectSummary{{ID: 77, Name: "different-hub-project"}}

	out, cmd := m.previewFederationEnrollment()

	require.Nil(t, cmd)
	assert.Equal(t, federationOperationAdoptSameName, out.federation.draft.Operation)
	assert.Equal(t, int64(0), out.federation.draft.HubProjectID)
	assert.Equal(t, "spoke-project", out.federation.draft.HubProjectName)
}

func TestFederationEnroll_ExistingLocalFederationBindingBlocksBeforeMutation(t *testing.T) {
	m := setupFederationHubProjectSelection()
	m.federation.statuses = []FederationProjectStatus{federationStatusFixture("spoke-project", "spoke")}
	m.federation.hubProjects = []ProjectSummary{{ID: 42, Name: "spoke-project"}}

	out, cmd := m.previewFederationEnrollment()
	require.Nil(t, cmd)

	assert.Equal(t, federationModePreview, out.federation.mode)
	assert.Contains(t, stripANSI(renderFederation(out)), "already has federation binding")

	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	assert.Equal(t, federationModePreview, out.federation.mode)
}

func TestFederationEnroll_MissingTokenEnvBlocksBeforeMutation(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv(missingHubAuthEnvName(), "")
	m := setupFederationView()
	m.scope = homedScope(7, "spoke-project")
	injectProjects(&m, mockProject{ID: 7, Name: "spoke-project"})
	m.daemonTargets = []daemonTarget{
		daemonTargetWithResolvedAuth("spoke", "https://spoke.example", "spoke-auth", false),
		{Name: "hub", URL: "https://hub.example", TokenEnv: missingHubAuthEnvName()},
	}
	m.activeDaemon = m.daemonTargets[0]

	out, _ := m.routeFederationViewKey(keyRune('n'))
	out, _ = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // adopt the pre-cursored active project
	out.federation.hubCursor = 1
	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectHub, out.federation.mode)
	require.EqualError(t, out.federation.op.err,
		`daemon "hub": token_env "`+missingHubAuthEnvName()+`" is unset or empty`)
}

func TestFederationEnroll_GlobalAuthTokenDoesNotOverrideMissingTargetTokenEnv(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "local-daemon-token")
	t.Setenv(missingHubAuthEnvName(), "")
	m := setupFederationView()
	m.scope = homedScope(7, "spoke-project")
	injectProjects(&m, mockProject{ID: 7, Name: "spoke-project"})
	m.daemonTargets = []daemonTarget{
		daemonTargetWithResolvedAuth("spoke", "https://spoke.example", "spoke-auth", false),
		{Name: "hub", URL: "https://hub.example", TokenEnv: missingHubAuthEnvName()},
	}
	m.activeDaemon = m.daemonTargets[0]

	out, _ := m.routeFederationViewKey(keyRune('n'))
	out, _ = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	out.federation.hubCursor = 1
	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectHub, out.federation.mode)
	require.EqualError(t, out.federation.op.err,
		`daemon "hub": token_env "`+missingHubAuthEnvName()+`" is unset or empty`)
}

func TestFederationEnroll_ActiveDaemonAsHubBlocked(t *testing.T) {
	m := setupFederationView()
	m.scope = homedScope(7, "spoke-project")
	injectProjects(&m, mockProject{ID: 7, Name: "spoke-project"})
	m.daemonTargets = []daemonTarget{daemonTargetWithResolvedAuth("spoke", "https://spoke.example", "spoke-auth", false)}
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
	m.federation.mode = federationModeSelectHub
	m.federation.draft = newFederationDraft("operator")
	m.federation.draft.SpokeProjectID = 7
	m.federation.draft.SpokeProjectName = "spoke-project"
	m.activeDaemon = daemonTarget{Name: "spoke", URL: "https://spoke.example"}
	m.daemonTargets = []daemonTarget{
		m.activeDaemon,
		{Name: "local-hub", Local: true},
	}
	m.federation.hubCursor = 1

	out, cmd := enterThroughAdoptConfirm(t, m)

	require.Nil(t, cmd)
	assert.Equal(t, federationModeSelectHub, out.federation.mode)
	assert.Empty(t, out.federation.draft.HubTarget.Name)
	require.Error(t, out.federation.op.err)
	assert.Contains(t, out.federation.op.err.Error(), "local hub")
}

func TestFederationEnroll_PlainHTTPHostnameRequiresCatalogAllowInsecure(t *testing.T) {
	m := setupFederationView()
	m.scope = homedScope(7, "spoke-project")
	injectProjects(&m, mockProject{ID: 7, Name: "spoke-project"})
	m.daemonTargets = []daemonTarget{
		daemonTargetWithResolvedAuth("spoke", "https://spoke.example", "spoke-auth", false),
		daemonTargetWithResolvedAuth("hub", "http://hub.internal:7777", "hub-auth", false),
	}
	m.activeDaemon = m.daemonTargets[0]

	out, _ := m.routeFederationViewKey(keyRune('n'))
	out, _ = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter}) // adopt the pre-cursored active project
	out.federation.hubCursor = 1
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
	assert.Equal(t, federationModeResult, out.federation.mode)
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
	m.federation.draft.Operation = federationOperationAdoptSelectedHub
	m.federation.draft.SpokeProjectName = "local-spoke-project"
	m.federation.draft.HubProjectName = "hub-project"

	out, cmd := enterThroughAdoptConfirm(t, m)
	require.NotNil(t, cmd)
	msg := cmd().(federationEnrollResultMsg)
	out, _ = updateModel(out, msg)

	assert.Equal(t, federationModeResult, out.federation.mode)
	assert.Equal(t, "local-spoke-project", joinBody.ProjectName)
	assert.Equal(t, "hub-project", out.federation.op.enroll.Metadata.ProjectName)
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EX", joinBody.HubProjectUID)
}

func TestFederationEnroll_AdoptSelectedHubRecoveryUsesSelectedLocalProjectName(t *testing.T) {
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{
		hubProjectName: "hub-project",
		joinStatus:     http.StatusInternalServerError,
	})
	m.federation.draft.Operation = federationOperationAdoptSelectedHub
	m.federation.draft.SpokeProjectName = "local-spoke-project"
	m.federation.draft.HubProjectName = "hub-project"
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
	assert.Equal(t, federationModeRecovery, out.federation.mode)
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
	assert.Equal(t, federationModeRecovery, out.federation.mode)
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
	assert.Equal(t, federationModePreview, out.federation.mode)
	require.Error(t, out.federation.op.err)
	assert.Contains(t, out.federation.op.err.Error(), "hub unavailable")
	assert.Empty(t, out.federation.recovery.Token)
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
		m.federation.draft,
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
	m.activeDaemon.resolved.AllowInsecure = true
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
	m.activeDaemon.resolved.Token = spokeToken
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
	m.federation.cursor = 0

	out, cmd := m.routeFederationViewKey(keyRune('x'))

	require.Nil(t, cmd)
	assert.Equal(t, federationModeLeavePreview, out.federation.mode)
	assert.Equal(t, "spoke-proj", out.federation.leaveDraft.ProjectName)
	assert.Equal(t, "http://hub.internal:7777", out.federation.leaveDraft.HubURL)
	assert.Equal(t, int64(42), out.federation.leaveDraft.HubProjectID)
	assert.Equal(t, int64(7), out.federation.leaveDraft.ProjectID)
	assert.Equal(t, "detach", out.federation.leaveDraft.Disposition)
	assert.Empty(t, out.federation.leaveDraft.BlockedReason)
}

func TestFederationLeaveKeyDoesNotOpenPreviewOnNonSpokeRow(t *testing.T) {
	// The list only ever shows spoke rows, but the guard is the contract:
	// a non-spoke selection must never enter leave preview. Drive the guard
	// directly by exercising the detail router on a hub status.
	m := setupFederationViewWithStatuses(federationStatusFixture("hub-only", "hub"))
	m.federation.cursor = 0
	m.federation.mode = federationModeDetail

	out, cmd := m.routeFederationDetailKey(keyRune('x'))

	require.Nil(t, cmd)
	assert.NotEqual(t, federationModeLeavePreview, out.federation.mode)
	assert.Equal(t, federationModeDetail, out.federation.mode)
}

func TestFederationLeavePreviewRender(t *testing.T) {
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.federation.cursor = 0
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
	m.federation.cursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))
	require.Equal(t, "detach", out.federation.leaveDraft.Disposition)

	out, cmd := out.routeFederationViewKey(keyRune('d'))
	require.Nil(t, cmd)
	assert.Equal(t, "archive", out.federation.leaveDraft.Disposition)

	out, cmd = out.routeFederationViewKey(keyRune('d'))
	require.Nil(t, cmd)
	assert.Equal(t, "detach", out.federation.leaveDraft.Disposition)

	out, cmd = out.routeFederationViewKey(keyRune('l'))
	require.Nil(t, cmd)
	assert.True(t, out.federation.leaveDraft.LocalOnly)
	assert.Contains(t, stripANSI(renderFederation(out)), "local-only")

	out, cmd = out.routeFederationViewKey(keyRune('l'))
	require.Nil(t, cmd)
	assert.False(t, out.federation.leaveDraft.LocalOnly)
}

func TestFederationLeavePreviewEscReturnsToList(t *testing.T) {
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.federation.cursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))
	require.Equal(t, federationModeLeavePreview, out.federation.mode)

	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEsc})

	require.Nil(t, cmd)
	assert.Equal(t, federationModeList, out.federation.mode)
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
	m.federation.cursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))
	require.Equal(t, federationModeLeavePreview, out.federation.mode)

	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd().(federationLeaveResultMsg)
	out, refresh := updateModel(out, msg)

	require.NotNil(t, refresh)
	require.NoError(t, msg.err)
	assert.Equal(t, federationModeResult, out.federation.mode)
	assert.Equal(t, []int64{11}, hub.revokedEnrollmentIDs)
	assert.Equal(t, "detach", leaveBody.Disposition)
	assert.Equal(t, "operator", leaveBody.Actor)
	assert.Equal(t, 1, out.federation.op.leave.RevokedCount)
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
	m.federation.cursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))
	require.Equal(t, federationModeLeavePreview, out.federation.mode)

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
	m.federation.cursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))
	require.Equal(t, federationModeLeavePreview, out.federation.mode)
	require.NotEqual(t, "archive", out.federation.leaveDraft.Disposition, "this test covers the default detach path")

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
	m.federation.cursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))
	require.Equal(t, federationModeLeavePreview, out.federation.mode)
	out, _ = out.routeFederationViewKey(keyRune('d')) // toggle disposition to archive
	require.Equal(t, "archive", out.federation.leaveDraft.Disposition)

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
	m.federation.cursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))
	out, _ = out.routeFederationViewKey(keyRune('l')) // toggle local-only
	require.True(t, out.federation.leaveDraft.LocalOnly)

	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd().(federationLeaveResultMsg)
	out, _ = updateModel(out, msg)

	require.NoError(t, msg.err)
	assert.Equal(t, federationModeResult, out.federation.mode)
	assert.Equal(t, 0, hub.listEnrollmentsCalls)
	assert.Empty(t, hub.revokedEnrollmentIDs)
	assert.True(t, out.federation.op.leave.SkippedRevoke)
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
	m.federation.cursor = 0
	out, _ := m.routeFederationViewKey(keyRune('x'))

	out, cmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	msg := cmd().(federationLeaveResultMsg)
	out, next := updateModel(out, msg)

	require.Nil(t, next)
	assert.False(t, leaveCalled, "local teardown must not run when hub revoke fails")
	assert.Equal(t, federationModeLeavePreview, out.federation.mode)
	require.Error(t, out.federation.op.err)
	assert.Contains(t, out.federation.op.err.Error(), "hub revoke failed")
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
		daemonTargetWithResolvedAuth("hub", "https://bound.example/", "catalog-token", false),
	}

	got := m.federationLeaveHubTarget("https://bound.example", true)

	// Token comes from the matched catalog entry (slash-tolerant match)...
	assert.Equal(t, "catalog-token", got.resolved.Token)
	// ...but the target URL is pinned to the binding's hub URL, normalized,
	// and allow_insecure comes from the binding, not the catalog entry.
	assert.Equal(t, "https://bound.example", got.URL)
	assert.True(t, got.resolved.AllowInsecure, "binding allow_insecure must carry through")
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
		daemonTargetWithResolvedAuth("hub", "http://hub.internal:7373", "catalog-token", true),
	}

	got := m.federationLeaveHubTarget("http://hub.internal:7373", false)

	assert.Equal(t, "catalog-token", got.resolved.Token)
	assert.True(t, got.resolved.AllowInsecure,
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
	assert.Empty(t, got.resolved.Token)
	assert.True(t, got.resolved.AllowInsecure, "binding allow_insecure must carry through")
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
	m.federation.mode = federationModeList
	return m
}

func setupFederationViewWithStatuses(statuses ...FederationProjectStatus) Model {
	m := setupFederationView()
	m.federation.instance = InstanceInfo{
		InstanceUID:   "01HZNQ7VFPK1XGD8R5MABCD4EA",
		Version:       "dev",
		SchemaVersion: 1,
	}
	m.federation.statuses = statuses
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
	m.federation.mode = federationModeSelectHubProject
	m.federation.draft = newFederationDraft("operator")
	m.federation.draft.SpokeProjectID = 7
	m.federation.draft.SpokeProjectName = "spoke-project"
	m.federation.draft.HubTarget = daemonTargetWithResolvedAuth(
		"hub", "https://hub.example", "hub-auth", true,
	)
	m.federation.draft.AllowInsecure = true
	m.federation.draft.AdoptExisting = true
	return m
}

// TestFederationHubProjectRows is the audit's requested table over
// {adopt, create-replica, browse} x {empty, only-same-name, mixed}. It is
// the single place the sentinel-row convention is now stated: count,
// labels and selection all come off one row list, so they cannot drift
// apart the way the four independent derivations used to.
func TestFederationHubProjectRows(t *testing.T) {
	sameName := ProjectSummary{ID: 42, Name: "spoke-project"}
	other := ProjectSummary{ID: 77, Name: "hub-project"}

	setup := func(createReplica bool, projects []ProjectSummary) Model {
		m := setupFederationHubProjectSelection()
		m.federation.draft.CreateReplica = createReplica
		m.federation.hubProjects = projects
		return m
	}

	t.Run("adopt/empty", func(t *testing.T) {
		rows := federationHubProjectRows(setup(false, nil))
		require.Len(t, rows, 1)
		assert.True(t, rows[0].sentinel)
		assert.Equal(t, `create hub project "spoke-project" and enable federation`, rows[0].label)
	})

	t.Run("adopt/only-same-name", func(t *testing.T) {
		rows := federationHubProjectRows(setup(false, []ProjectSummary{sameName}))
		require.Len(t, rows, 1, "the same-name project is folded into the sentinel row")
		assert.True(t, rows[0].sentinel)
		assert.Equal(t,
			`use existing hub project "spoke-project"; enable federation if needed`,
			rows[0].label)
	})

	t.Run("adopt/mixed", func(t *testing.T) {
		m := setup(false, []ProjectSummary{sameName, other})
		rows := federationHubProjectRows(m)
		require.Len(t, rows, 2)
		assert.True(t, rows[0].sentinel)
		assert.False(t, rows[1].sentinel)
		assert.Equal(t, other, rows[1].project)
		assert.Equal(t, "hub-project", rows[1].label)

		m.federation.hubProjectCursor = 0
		row, ok := m.selectedFederationHubProjectRow()
		require.True(t, ok)
		assert.True(t, row.sentinel, "row 0 stays the adopt-same-name sentinel")

		m.federation.hubProjectCursor = 1
		row, ok = m.selectedFederationHubProjectRow()
		require.True(t, ok)
		assert.False(t, row.sentinel)
		assert.Equal(t, int64(77), row.project.ID)
	})

	t.Run("create-replica/empty", func(t *testing.T) {
		assert.Empty(t, federationHubProjectRows(setup(true, nil)))
	})

	t.Run("create-replica/only-same-name", func(t *testing.T) {
		m := setup(true, []ProjectSummary{sameName})
		rows := federationHubProjectRows(m)
		require.Len(t, rows, 1, "create-replica does not filter the same-name project")
		assert.False(t, rows[0].sentinel)
		assert.Equal(t, "spoke-project", rows[0].label)

		m.federation.hubProjectCursor = 0
		row, ok := m.selectedFederationHubProjectRow()
		require.True(t, ok)
		assert.Equal(t, int64(42), row.project.ID, "row 0 is the first hub project in create-replica mode")
	})

	t.Run("create-replica/mixed", func(t *testing.T) {
		rows := federationHubProjectRows(setup(true, []ProjectSummary{sameName, other}))
		require.Len(t, rows, 2)
		assert.Equal(t, int64(42), rows[0].project.ID)
		assert.Equal(t, int64(77), rows[1].project.ID)
	})

	t.Run("browse/empty", func(t *testing.T) {
		assert.Empty(t, federationBrowseHubProjectRows(setup(false, nil)))
	})

	t.Run("browse/only-same-name", func(t *testing.T) {
		rows := federationBrowseHubProjectRows(setup(false, []ProjectSummary{sameName}))
		require.Len(t, rows, 1, "browse never filters and never prepends a sentinel")
		assert.False(t, rows[0].sentinel)
		assert.Equal(t, "42 spoke-project", rows[0].label)
	})

	t.Run("browse/mixed", func(t *testing.T) {
		rows := federationBrowseHubProjectRows(setup(false, []ProjectSummary{sameName, other}))
		require.Len(t, rows, 2)
		assert.Equal(t, "42 spoke-project", rows[0].label)
		assert.Equal(t, "77 hub-project", rows[1].label)
	})
}

// TestFederationHubProjectRowsForMode: browse mode is a third row
// convention over the same cursor and the same []ProjectSummary. Naming it
// in a builder is what removes the count patch-up that
// handleFederationHubProjectsLoaded used to need.
func TestFederationHubProjectRowsForMode(t *testing.T) {
	m := setupFederationHubProjectSelection()
	m.federation.hubProjects = []ProjectSummary{{ID: 42, Name: "spoke-project"}}

	m.federation.mode = federationModeSelectHubProject
	assert.Len(t, federationHubProjectRowsForMode(m), 1)
	assert.True(t, federationHubProjectRowsForMode(m)[0].sentinel)

	m.federation.mode = federationModeBrowseHubs
	assert.Len(t, federationHubProjectRowsForMode(m), 1)
	assert.False(t, federationHubProjectRowsForMode(m)[0].sentinel)
}

// TestFederationSelectHubProject_EmptyCreateReplicaListIsNotSelectable pins
// the resolution of the count-vs-label divergence: with create-replica and
// no hub projects, the screen explains itself but offers nothing to move to,
// and the key router agrees.
func TestFederationSelectHubProject_EmptyCreateReplicaListIsNotSelectable(t *testing.T) {
	m := setupFederationHubProjectSelection()
	m.federation.draft.CreateReplica = true
	m.federation.hubProjects = nil

	rendered := stripANSI(renderFederation(m))

	assert.Contains(t, rendered, "no hub projects")
	assert.NotContains(t, rendered, "▶ no hub projects",
		"the empty-list line must not render as a highlighted, selectable row")

	assert.Empty(t, federationHubProjectRows(m),
		"rendered selectable rows and the router's movement bound must agree")
	out, cmd := m.routeFederationViewKey(keyRune('j'))
	require.Nil(t, cmd)
	assert.Equal(t, 0, out.federation.hubProjectCursor,
		"j must not move a cursor over an empty row list")
}

// TestFederationSelectHubProject_OutOfRangeCursorSelectsTheHighlightedRow:
// the renderer has always clamped its highlight, but the selection used to
// return "nothing selected" past the end, so Enter and the highlight could
// disagree. Selection now clamps the same way.
func TestFederationSelectHubProject_OutOfRangeCursorSelectsTheHighlightedRow(t *testing.T) {
	m := setupFederationHubProjectSelection()
	m.federation.hubProjects = []ProjectSummary{{ID: 77, Name: "hub-project"}}
	m.federation.hubProjectCursor = 99 // rows are [sentinel, hub-project]

	out, cmd := m.previewFederationEnrollment()

	require.Nil(t, cmd)
	assert.Equal(t, federationOperationAdoptSelectedHub, out.federation.draft.Operation)
	assert.Equal(t, int64(77), out.federation.draft.HubProjectID)
}

// TestFederationSelectHubProject_RowLabelsDriveTheRenderedList: the render
// loop reads row.label instead of calling a parallel label builder, so a
// label change cannot land on one of the two lists only.
func TestFederationSelectHubProject_RowLabelsDriveTheRenderedList(t *testing.T) {
	m := setupFederationHubProjectSelection()
	m.federation.hubProjects = []ProjectSummary{
		{ID: 42, Name: "spoke-project"},
		{ID: 77, Name: "hub-project"},
	}

	rendered := stripANSI(renderFederation(m))

	for _, row := range federationHubProjectRows(m) {
		assert.Contains(t, rendered, row.label)
	}
	assert.Contains(t, rendered, `▶ use existing hub project "spoke-project"`,
		"cursor 0 highlights the sentinel row")
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
	m.federation.instance = InstanceInfo{InstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA"}
	m.federation.mode = federationModePreview
	m.federation.draft.Operation = federationOperationAdoptSameName
	m.federation.draft.HubProjectID = 42
	m.federation.draft.HubProjectName = hubProjectName
	m.federation.draft.HubTarget = daemonTargetWithResolvedAuth("hub", hub.URL, "", true)
	m.federation.draft.AllowInsecure = true
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
	name, _ string,
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
	m.federation.mode = federationModeRecovery
	m.federation.recovery = federationRecovery{
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
	m.federation.mode = federationModeRecovery
	m.federation.recovery = federationRecovery{
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
	m.federation.draft = federationDraft{CreateReplica: true}
	m.federation.hubProjects = []ProjectSummary{{ID: 42, Name: "hub-project", UID: "01HZNQ7VFPK1XGD8R5MABCD4EX"}}
	m.federation.hubProjectCursor = 0

	out, _ := m.previewFederationEnrollment()

	draft := out.federation.draft
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
	m.federation.statuses = []FederationProjectStatus{{ProjectID: 7, ProjectName: "spoke-project", Role: "spoke"}}
	m.federation.draft = federationDraft{CreateReplica: true}
	m.federation.hubProjects = []ProjectSummary{{ID: 42, Name: "hub-project", UID: "01HZNQ7VFPK1XGD8R5MABCD4EX"}}
	m.federation.hubProjectCursor = 0

	out, _ := m.previewFederationEnrollment()

	assert.NotEqual(t, federationOperationRejoin, out.federation.draft.Operation)
	assert.Contains(t, out.federation.draft.BlockedReason, "spoke-project")
}

// TestFederationPreviewCreateReplicaUnaffectedWithoutUIDMatch: no local
// UID-holder means the plain create-replica path is unchanged.
func TestFederationPreviewCreateReplicaUnaffectedWithoutUIDMatch(t *testing.T) {
	m := setupFederationView()
	m.projectsByID = map[int64]string{7: "other-project"}
	m.projectUIDByID = map[int64]string{7: "01HZNQ7VFPK1XGD8R5MABCD4ZZ"}
	m.federation.draft = federationDraft{CreateReplica: true}
	m.federation.hubProjects = []ProjectSummary{{ID: 42, Name: "hub-project", UID: "01HZNQ7VFPK1XGD8R5MABCD4EX"}}
	m.federation.hubProjectCursor = 0

	out, _ := m.previewFederationEnrollment()

	assert.Equal(t, federationOperationCreateReplica, out.federation.draft.Operation)
	assert.Empty(t, out.federation.draft.BlockedReason)
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
	if out.federation.mode != federationModeAdoptConfirm {
		return out, cmd
	}
	require.Nil(t, cmd, "entering the adopt confirmation must not execute")
	out = typeFederationKeys(out, out.federation.draft.SpokeProjectName)
	return out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
}

// TestFederationAdoptEnterOpensTypedConfirmation: adoption never executes on a
// bare Enter; the gate states the INTO relationship, a wrong name keeps it
// gated, and Esc returns to the preview.
func TestFederationAdoptEnterOpensTypedConfirmation(t *testing.T) {
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{})

	out, cmd := m.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd, "adoption must not execute on bare enter")
	assert.Equal(t, federationModeAdoptConfirm, out.federation.mode)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "INTO hub project")
	assert.Contains(t, rendered, "spoke-project")

	out = typeFederationKeys(out, "wrong-name")
	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	assert.Equal(t, federationModeAdoptConfirm, out.federation.mode)
	assert.False(t, out.federation.op.running)
	require.Error(t, out.federation.op.err)

	out, _ = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	assert.Equal(t, federationModePreview, out.federation.mode)
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
		m.federation.mode = federationModeAdoptConfirm
		m.federation.draft = newFederationDraft("operator")
		m.federation.draft.SpokeProjectName = "spoke project"
		return m
	}

	t.Run("runeless KeySpace appends a space", func(t *testing.T) {
		m := typeFederationKeys(newConfirm(), "spoke")
		m, _ = m.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
		m = typeFederationKeys(m, "project")
		assert.Equal(t, "spoke project", m.federation.adoptConfirmInput)
	})

	t.Run("unix-shape KeySpace with rune appends one space", func(t *testing.T) {
		m := typeFederationKeys(newConfirm(), "spoke")
		m, _ = m.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
		m = typeFederationKeys(m, "project")
		assert.Equal(t, "spoke project", m.federation.adoptConfirmInput)
	})
}

// TestFederationAdoptTypedConfirmationExecutes: typing the exact project name
// and confirming runs the enrollment.
func TestFederationAdoptTypedConfirmationExecutes(t *testing.T) {
	m, joinBody := setupFederationExecutionPreview(t, federationExecutionServerOptions{})

	out, cmd := enterThroughAdoptConfirm(t, m)
	require.NotNil(t, cmd)
	assert.True(t, out.federation.op.running)
	msg := cmd().(federationEnrollResultMsg)
	out, _ = updateModel(out, msg)
	assert.Equal(t, federationModeResult, out.federation.mode)
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
	m.federation.draft = newFederationDraft("operator")
	m.federation.draft.SpokeProjectID = 7
	m.federation.draft.SpokeProjectName = "spoke-project"
	m.federation.hubProjects = []ProjectSummary{{ID: 42, Name: "spoke-project", UID: "01HZNQ7VFPK1XGD8R5MABCD4EX"}}
	m.federation.hubProjectCursor = 0 // adopt-same-name row

	out, _ := m.previewFederationEnrollment()

	draft := out.federation.draft
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
	m.federation.draft = newFederationDraft("operator")
	m.federation.draft.SpokeProjectID = 7
	m.federation.draft.SpokeProjectName = "spoke-project"
	// Hub row carries no UID (e.g. an older hub daemon's project list).
	m.federation.hubProjects = []ProjectSummary{{ID: 42, Name: "spoke-project"}}
	m.federation.hubProjectCursor = 0 // adopt-same-name row

	out, _ := m.previewFederationEnrollment()

	draft := out.federation.draft
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
	m.federation.draft = newFederationDraft("operator")
	m.federation.draft.SpokeProjectID = 7
	m.federation.draft.SpokeProjectName = "spoke-project"
	m.federation.hubProjects = []ProjectSummary{{ID: 42, Name: "spoke-project", UID: "01HZNQ7VFPK1XGD8R5MABCD4EX"}}
	m.federation.hubProjectCursor = 0 // adopt-same-name row

	out, _ := m.previewFederationEnrollment()

	draft := out.federation.draft
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
	m.federation.draft = newFederationDraft("operator")
	m.federation.draft.SpokeProjectID = 7
	m.federation.draft.SpokeProjectName = "spoke-project"
	m.federation.hubProjects = []ProjectSummary{{ID: 42, Name: "spoke-project", UID: "01HZNQ7VFPK1XGD8R5MABCD4EX"}}
	m.federation.hubProjectCursor = 0

	out, _ := m.previewFederationEnrollment()

	assert.Equal(t, federationOperationAdoptSameName, out.federation.draft.Operation)
	assert.True(t, out.federation.draft.AdoptExisting)
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
func daemonTargetWithResolvedAuth(name, baseURL, token string, allowInsecure bool) daemonTarget {
	return daemonTarget{
		Name: name,
		URL:  baseURL,
		resolved: clientpkg.ResolvedDaemon{
			Source: clientpkg.DaemonSourceInjected, Name: name, BaseURL: baseURL,
			Token: token, AllowInsecure: allowInsecure,
		},
	}
}

// TestFederationResultScreenFollowsOperationKind: the result screen used to
// pick its body from a separate bool discriminator any writer could leave
// disagreeing with the populated result struct. It now switches on the kind
// of the operation that produced the result.
func TestFederationResultScreenFollowsOperationKind(t *testing.T) {
	base := setupFederationView()
	base.federation.mode = federationModeResult

	enroll := base
	enroll.federation.op = federationOp{
		kind:   federationOpEnroll,
		enroll: federationEnrollResult{HubURL: "https://hub.example"},
	}
	rendered := stripANSI(renderFederation(enroll))
	assert.Contains(t, rendered, "Enrollment Result")
	assert.NotContains(t, rendered, "Leave Result")

	leave := base
	leave.federation.op = federationOp{
		kind: federationOpLeave,
		leave: federationLeaveResult{
			Draft:        federationLeaveDraft{ProjectName: "spoke-project"},
			RevokedCount: 1,
		},
	}
	rendered = stripANSI(renderFederation(leave))
	assert.Contains(t, rendered, "Leave Result")
	assert.NotContains(t, rendered, "Enrollment Result")
}

// TestFederationLateResultFromOtherFlowIsDiscarded: the two flows share one
// attempt counter now, so starting a leave invalidates an in-flight enroll's
// result. That is the intended reading of "the modes are mutually exclusive",
// but it is a behavior change and has to be asserted rather than assumed.
func TestFederationLateResultFromOtherFlowIsDiscarded(t *testing.T) {
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-project", "spoke"))
	m.federation.op = federationOp{kind: federationOpEnroll, attempt: 1, running: true}

	// A leave starts while the enroll is still in flight.
	m.federation.op = federationOp{kind: federationOpLeave, attempt: 2, running: true}
	m.federation.mode = federationModeLeavePreview

	out, cmd := m.handleFederationEnrollResult(federationEnrollResultMsg{
		connGen: m.connGen,
		attempt: 1,
		result:  federationEnrollResult{HubURL: "https://hub.example"},
	})

	assert.Nil(t, cmd, "a stale result must not trigger a status refetch")
	assert.Equal(t, federationModeLeavePreview, out.federation.mode)
	assert.Equal(t, federationOpLeave, out.federation.op.kind)
	assert.True(t, out.federation.op.running, "the live leave must stay in flight")
	assert.Equal(t, federationEnrollResult{}, out.federation.op.enroll)
}

// TestFederationMergedHandlerRoutesErrorsToTheFlowsOwnPreview pins that the
// merged handler still routes each flow's failure back to its own preview.
func TestFederationMergedHandlerRoutesErrorsToTheFlowsOwnPreview(t *testing.T) {
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-project", "spoke"))

	enrolling := m
	enrolling.federation.op = federationOp{kind: federationOpEnroll, attempt: 1, running: true}
	out, cmd := enrolling.handleFederationEnrollResult(federationEnrollResultMsg{
		connGen: m.connGen, attempt: 1, err: errors.New("hub unavailable"),
	})
	require.Nil(t, cmd)
	assert.Equal(t, federationModePreview, out.federation.mode)
	assert.False(t, out.federation.op.running)
	require.Error(t, out.federation.op.err)

	leaving := m
	leaving.federation.op = federationOp{kind: federationOpLeave, attempt: 1, running: true}
	out, cmd = leaving.handleFederationLeaveResult(federationLeaveResultMsg{
		connGen: m.connGen, attempt: 1, err: errors.New("hub revoke failed"),
	})
	require.Nil(t, cmd)
	assert.Equal(t, federationModeLeavePreview, out.federation.mode)
	assert.False(t, out.federation.op.running)
	require.Error(t, out.federation.op.err)
}

// TestFederationKeyRouteSwitchFromEnrollToLeaveInvalidatesLateEnrollResult
// exercises the reachable path that exposed the stale-operation bug: dispatch
// an enrollment, back out through each enrollment screen, then open the leave
// preview from the list. The enrollment reply must no longer own the current
// operation and therefore cannot replace the leave preview.
func TestFederationKeyRouteSwitchFromEnrollToLeaveInvalidatesLateEnrollResult(t *testing.T) {
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{})
	m.federation.statuses = []FederationProjectStatus{federationStatusFixture("spoke-project", "spoke")}

	out, enrollCmd := enterThroughAdoptConfirm(t, m)
	require.NotNil(t, enrollCmd)
	require.Equal(t, federationOpEnroll, out.federation.op.kind)
	enrollAttempt := out.federation.op.attempt

	for _, wantMode := range []federationMode{
		federationModePreview,
		federationModeSelectHubProject,
		federationModeSelectHub,
		federationModeList,
	} {
		var cmd tea.Cmd
		out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEsc})
		require.Nil(t, cmd)
		require.Equal(t, wantMode, out.federation.mode)
	}

	out, cmd := out.routeFederationViewKey(keyRune('x'))
	require.Nil(t, cmd)
	require.Equal(t, federationModeLeavePreview, out.federation.mode)
	assert.Greater(t, out.federation.op.attempt, enrollAttempt,
		"switching flows must invalidate the dispatched enrollment attempt")
	assert.Equal(t, federationOpNone, out.federation.op.kind)
	assert.False(t, out.federation.op.running)

	out, cmd = out.handleFederationEnrollResult(federationEnrollResultMsg{
		connGen: out.connGen,
		attempt: enrollAttempt,
		result:  federationEnrollResult{HubURL: "https://hub.example"},
	})
	require.Nil(t, cmd)
	assert.Equal(t, federationModeLeavePreview, out.federation.mode)
	assert.Equal(t, federationOpNone, out.federation.op.kind)
}

// TestFederationKeyRouteSwitchFromLeaveToEnrollCanDispatchEnrollment covers
// the reverse path: an abandoned in-flight leave must not leave running=true
// behind to block Enter on the enrollment preview. Every transition below is
// driven through the federation key router, including loading the selected
// hub's projects through the command returned by Enter.
func TestFederationKeyRouteSwitchFromLeaveToEnrollCanDispatchEnrollment(t *testing.T) {
	hubAdmin := &recordingFederationHubAdmin{
		instance: InstanceInfo{Auth: AuthInfo{Kind: "db_token", Actor: "hub-operator"}},
		projects: []ProjectSummary{{ID: 42, Name: "spoke-project"}},
	}
	restoreFederationHubAdminClient(t, func(
		_ context.Context,
		target daemonTarget,
	) (federationHubAdminAPI, daemonTarget, error) {
		return hubAdmin, target, nil
	})
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{})
	hubTarget := m.federation.draft.HubTarget
	m.federation.statuses = []FederationProjectStatus{federationStatusFixture("spoke-project", "spoke")}
	m.federation.mode = federationModeList
	m.scope = homedScope(9, "other-project")
	injectProjects(&m, mockProject{ID: 9, Name: "other-project"})
	m.daemonTargets = []daemonTarget{m.activeDaemon, hubTarget}

	out, cmd := m.routeFederationViewKey(keyRune('x'))
	require.Nil(t, cmd)
	require.Equal(t, federationModeLeavePreview, out.federation.mode)
	out, leaveCmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, leaveCmd)
	require.Equal(t, federationOpLeave, out.federation.op.kind)
	leaveAttempt := out.federation.op.attempt

	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Nil(t, cmd)
	require.Equal(t, federationModeList, out.federation.mode)
	out, cmd = out.routeFederationViewKey(keyRune('n'))
	require.Nil(t, cmd)
	require.Equal(t, federationModeSelectLocalProject, out.federation.mode)
	assert.Greater(t, out.federation.op.attempt, leaveAttempt,
		"starting enrollment must invalidate the dispatched leave attempt")
	assert.False(t, out.federation.op.running)

	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	require.Equal(t, federationModeSelectHub, out.federation.mode)
	rows := federationHubRows(out)
	require.NotEmpty(t, rows)
	hubRow := -1
	for i, row := range rows {
		if daemonTargetsMatch(row.target, hubTarget) {
			hubRow = i
			break
		}
	}
	require.NotEqual(t, -1, hubRow)
	for out.federation.hubCursor < hubRow {
		out, _ = out.routeFederationViewKey(keyRune('j'))
	}
	for out.federation.hubCursor > hubRow {
		out, _ = out.routeFederationViewKey(keyRune('k'))
	}

	out, loadCmd := out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, loadCmd)
	loaded, ok := loadCmd().(federationHubProjectsLoadedMsg)
	require.True(t, ok)
	require.NoError(t, loaded.err)
	out = out.handleFederationHubProjectsLoaded(loaded)
	require.Equal(t, federationModeSelectHubProject, out.federation.mode)

	out, cmd = out.routeFederationViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	require.Equal(t, federationModePreview, out.federation.mode)
	require.Empty(t, out.federation.draft.BlockedReason)
	out, enrollCmd := enterThroughAdoptConfirm(t, out)
	require.NotNil(t, enrollCmd, "the abandoned leave must not block enrollment dispatch")
	assert.Equal(t, federationOpEnroll, out.federation.op.kind)
	assert.True(t, out.federation.op.running)
	assert.Greater(t, out.federation.op.attempt, leaveAttempt)
}
