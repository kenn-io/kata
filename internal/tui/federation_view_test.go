package tui

import (
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
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-project", "spoke"))
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
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-project", "spoke"))
	rendered := stripANSI(renderFederation(m))

	assert.Contains(t, rendered, "kata / federation")
	assert.Contains(t, rendered, "Federation for active daemon:")
	assert.Contains(t, rendered, "spoke-daemon")
	assert.Contains(t, rendered, "http://spoke.internal:7777")
	assert.Contains(t, rendered, "instance 01HZNQ7VFPK1XGD8R5MABCD4EA")
	assert.Contains(t, rendered, "auth token")
	assert.Contains(t, rendered, "spoke-project")
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
		federationStatusFixture("spoke-project", "spoke"),
		federationStatusFixture("hub-only", "hub"),
	)

	rendered := stripANSI(renderFederation(m))

	assert.Contains(t, rendered, "spoke-project")
	assert.NotContains(t, rendered, "hub-only")
}

func TestFederationView_HelpAndFooterIncludeFederationBinding(t *testing.T) {
	help := stripANSI(renderHelp(newKeymap(), 100, ListFilter{}))
	assert.Contains(t, help, "F")
	assert.Contains(t, help, "federation")

	m := Model{list: listModel{issues: hierarchyIssues()}}
	assertHelpItemPresent(t, flattenHelpRows(m.queueHelpRows()), helpItem{key: "F", desc: "federation"})
}

func setupFederationSourceModel() Model {
	m := newTestModel()
	m.view = viewList
	m.width, m.height = 120, 24
	m.activeDaemon = daemonTarget{
		Name:     "spoke-daemon",
		URL:      "http://spoke.internal:7777",
		TokenEnv: "KATA_SPOKE_TOKEN",
	}
	return m
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
