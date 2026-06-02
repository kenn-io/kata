package tui

import (
	"encoding/json"
	"net/http"
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
	assert.Contains(t, rendered, "--hub-url")
	assert.Contains(t, rendered, "--hub-project-id 42")
	assert.Contains(t, rendered, "--project-name spoke-project")
	assert.Contains(t, rendered, enrollmentSecret())
	assert.NotContains(t, rendered, "--hub-project-uid")
	assert.NotContains(t, rendered, "--replay-horizon-event-id")
	assert.NotContains(t, rendered, "--baseline-through-event-id")
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
	assert.Contains(t, rendered, "--hub-url")
	assert.Contains(t, rendered, "--hub-project-id 42")
	assert.Contains(t, rendered, "--hub-project-uid 01HZNQ7VFPK1XGD8R5MABCD4EX")
	assert.Contains(t, rendered, "--project-name spoke-project")
	assert.Contains(t, rendered, "--replay-horizon-event-id 9")
	assert.Contains(t, rendered, "--baseline-through-event-id 11")
	assert.Contains(t, rendered, enrollmentSecret())
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
}

func setupFederationExecutionPreview(
	t *testing.T,
	opts federationExecutionServerOptions,
) (Model, *CreateFederationReplicaInput) {
	t.Helper()
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
				respondJSON(t, w, map[string]any{"project": map[string]any{"id": 42, "name": "spoke-project"}})
				return
			}
			respondJSON(t, w, map[string]any{"projects": []map[string]any{{"id": 42, "name": "spoke-project"}}})
		},
		"/api/v1/projects/42/federation/enable": func(w http.ResponseWriter, _ *http.Request) {
			respondJSON(t, w, federationMetadataBody())
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
			respondJSON(t, w, federationMetadataBody())
		},
	})
	m := setupFederationHubProjectSelection()
	m.api = NewClient(spoke.URL, spoke.Client())
	m.activeDaemon = daemonTarget{Name: "spoke", URL: spoke.URL}
	m.federationInstance = InstanceInfo{InstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA"}
	m.federationMode = federationModePreview
	m.federationDraft.Operation = federationOperationAdoptSameName
	m.federationDraft.HubProjectID = 42
	m.federationDraft.HubProjectName = "spoke-project"
	m.federationDraft.HubTarget = daemonTarget{Name: "hub", URL: hub.URL, AllowInsecure: true}
	m.federationDraft.AllowInsecure = true
	return m, &joinBody
}

func federationMetadataBody() api.ProjectFederationBody {
	return api.ProjectFederationBody{
		ProjectID:              42,
		ProjectUID:             "01HZNQ7VFPK1XGD8R5MABCD4EX",
		ProjectName:            "spoke-project",
		ReplayHorizonEventID:   9,
		BaselineThroughEventID: 11,
	}
}

func enrollmentSecret() string {
	return strings.Join([]string{"enrollment", "secret"}, "-")
}
