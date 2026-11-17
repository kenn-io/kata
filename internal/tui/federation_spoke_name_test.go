package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/splitlayout"
)

// setupSpokeNameList is the federation list with one spoke row and a fake hub
// behind it. opened records the hub target the TUI asked a client for.
func setupSpokeNameList(t *testing.T) (Model, *recordingFederationHubAdmin, *daemonTarget) {
	t.Helper()
	hub := &recordingFederationHubAdmin{}
	opened := &daemonTarget{}
	restoreFederationHubAdminClient(t, func(
		_ context.Context,
		target daemonTarget,
	) (federationHubAdminAPI, daemonTarget, error) {
		*opened = target
		return hub, target, nil
	})
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	return m, hub, opened
}

// typeThrough types text one key at a time through Model.Update, so the global
// key routing sees each character the way a terminal delivers it.
func typeThrough(m Model, text string) Model {
	for _, r := range text {
		m = sendRune(m, r)
	}
	return m
}

func pressEnter(m Model) (Model, tea.Cmd) {
	return updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
}

// finishName runs the command Enter returned and delivers its reply.
func finishName(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	require.NotNil(t, cmd)
	msg, ok := cmd().(federationNameResultMsg)
	require.True(t, ok, "the save command answers with a federationNameResultMsg")
	out, _ := updateModel(m, msg)
	return out
}

func TestFederationNameKeyOpensPromptFromListAndDetail(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode federationMode
	}{
		{"list", federationModeList},
		{"detail", federationModeDetail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, opened := setupSpokeNameList(t)
			m.federation.mode = tc.mode

			out, cmd := updateModel(m, keyRune('N'))

			require.Nil(t, cmd, "opening the prompt reads nothing")
			assert.Equal(t, federationModeNamePrompt, out.federation.mode)
			assert.Equal(t, tc.mode, out.federation.nameDraft.Return)
			assert.Equal(t, "spoke-proj", out.federation.nameDraft.ProjectName)
			assert.Empty(t, out.federation.nameDraft.Input)
			assert.Empty(t, opened.URL, "no hub client is built until the name is saved")
		})
	}
}

func TestFederationNameKeyIgnoredWithoutASpokeRowOrOverHelp(t *testing.T) {
	empty := setupFederationViewWithStatuses()
	out, cmd := updateModel(empty, keyRune('N'))
	require.Nil(t, cmd)
	assert.Equal(t, federationModeList, out.federation.mode, "an empty list has no row to name")

	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	helped := sendRune(m, '?')
	require.Equal(t, viewHelp, helped.view)
	out, _ = updateModel(helped, keyRune('N'))
	assert.Equal(t, federationModeList, out.federation.mode, "Help owns the keyboard")
}

// A name is lowercase letters, so it contains keys the TUI reserves for quit
// (q), undo (u) and global navigation. While the prompt owns input they are
// text.
func TestFederationNamePromptTakesReservedKeysAsText(t *testing.T) {
	m, _, _ := setupSpokeNameList(t)
	m = sendRune(m, 'N')

	m = typeThrough(m, "quartz-u1")

	assert.Equal(t, modalNone, m.modal, "q must not open the quit confirmation")
	assert.Equal(t, viewFederation, m.view)
	assert.Equal(t, federationModeNamePrompt, m.federation.mode)
	assert.Equal(t, "quartz-u1", m.federation.nameDraft.Input)

	m = sendKey(m, tea.KeyBackspace)
	assert.Equal(t, "quartz-u", m.federation.nameDraft.Input)
}

func TestFederationNamePromptCtrlCStillQuits(t *testing.T) {
	m, _, _ := setupSpokeNameList(t)
	m = sendRune(m, 'N')

	_, cmd := updateModel(m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})

	require.NotNil(t, cmd)
	_, quit := cmd().(tea.QuitMsg)
	assert.True(t, quit)
}

func TestFederationNamePromptAcceptsPaste(t *testing.T) {
	m, _, _ := setupSpokeNameList(t)
	m = sendRune(m, 'N')

	m, _ = updateModel(m, tea.PasteMsg{Content: "build-box\n"})

	assert.Equal(t, "build-box", m.federation.nameDraft.Input)
}

func TestFederationNameSaveSetsTheNameOnTheRowsHub(t *testing.T) {
	m, hub, opened := setupSpokeNameList(t)
	m.daemonTargets = []daemonTarget{
		m.activeDaemon,
		daemonTargetWithResolvedAuth("catalog-hub", "http://hub.internal:7777/", "hub-credential", true),
	}
	m = typeThrough(sendRune(m, 'N'), "build-box")

	m, cmd := pressEnter(m)
	assert.True(t, m.federation.op.running)
	m = finishName(t, m, cmd)

	assert.Equal(t, []spokeNameWrite{{uid: "01HZNQ7VFPK1XGD8R5MABCD4EA", name: "build-box"}}, hub.spokeNameWrites)
	assert.Equal(t, "http://hub.internal:7777", opened.URL, "the name is written to the hub this row is bound to")
	assert.Equal(t, "hub-credential", opened.resolved.Token, "with the catalog credential for that hub")
	assert.Equal(t, federationModeList, m.federation.mode)
	assert.False(t, m.federation.op.running)
	rendered := stripANSI(renderFederation(m))
	assert.Contains(t, rendered, `named this spoke "build-box" on hub.internal:7777`)
}

func TestFederationNameSaveWithEmptyInputClearsTheName(t *testing.T) {
	m, hub, _ := setupSpokeNameList(t)
	m = sendRune(m, 'N')

	m, cmd := pressEnter(m)
	m = finishName(t, m, cmd)

	assert.Equal(t, []spokeNameWrite{{uid: "01HZNQ7VFPK1XGD8R5MABCD4EA", name: ""}}, hub.spokeNameWrites)
	assert.Contains(t, stripANSI(renderFederation(m)), "cleared the name of this spoke on hub.internal:7777")
}

func TestFederationNameSaveFromDetailReturnsToDetail(t *testing.T) {
	m, hub, _ := setupSpokeNameList(t)
	m.federation.mode = federationModeDetail
	m = typeThrough(sendRune(m, 'N'), "laptop")

	m, cmd := pressEnter(m)
	m = finishName(t, m, cmd)

	assert.Equal(t, federationModeDetail, m.federation.mode)
	assert.Len(t, hub.spokeNameWrites, 1)
	assert.Contains(t, stripANSI(renderFederation(m)), `named this spoke "laptop"`)
}

func TestFederationNameSaveRefusesAnInvalidNameWithoutContactingTheHub(t *testing.T) {
	m, hub, opened := setupSpokeNameList(t)
	m = typeThrough(sendRune(m, 'N'), "Build Box")

	m, cmd := pressEnter(m)

	require.Nil(t, cmd)
	assert.Equal(t, federationModeNamePrompt, m.federation.mode)
	assert.Equal(t, "Build Box", m.federation.nameDraft.Input, "the typed text stays")
	assert.False(t, m.federation.op.running)
	assert.Empty(t, opened.URL)
	assert.Empty(t, hub.calls)
	assert.Contains(t, stripANSI(renderFederation(m)), `"Build Box" must match [a-z][a-z0-9-]{0,31}`)
}

func TestFederationNameSaveNeedsALoadedInstanceUID(t *testing.T) {
	m, hub, _ := setupSpokeNameList(t)
	m.federation.instance = InstanceInfo{}
	m = typeThrough(sendRune(m, 'N'), "build-box")

	m, cmd := pressEnter(m)

	require.Nil(t, cmd)
	assert.Empty(t, hub.calls)
	assert.Contains(t, stripANSI(renderFederation(m)), "refresh federation status before naming")
}

func TestFederationNameSaveFailuresKeepTheTextAndSayWhatHappened(t *testing.T) {
	for _, tc := range []struct {
		name         string
		hubErr       error
		clientErr    error
		wantContains string
		// unsent: nothing reached the hub, so the text must not suggest it
		// may hold the name.
		unsent bool
	}{
		{
			name:         "the daemon refuses",
			hubErr:       &APIError{Status: 403, Code: "forbidden", Message: "owner credential required"},
			wantContains: "forbidden: owner credential required",
			unsent:       true,
		},
		{
			name:         "daemon too old, nothing was sent",
			hubErr:       &mutationPreflightError{errors.New("naming a spoke requires daemon API 0.38.0 or newer; upgrade the daemon")},
			wantContains: "naming a spoke requires daemon API 0.38.0 or newer",
			unsent:       true,
		},
		{
			name:         "hub client cannot be built, nothing was sent",
			clientErr:    errors.New(`daemon "catalog-hub" token_env is unset`),
			wantContains: `token_env is unset`,
			unsent:       true,
		},
		{
			name:         "transport failure after the send",
			hubErr:       errors.New("read tcp: connection reset"),
			wantContains: "the hub may have saved this name; save again to check",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, hub, _ := setupSpokeNameList(t)
			hub.spokeNameWriteErr = tc.hubErr
			if tc.clientErr != nil {
				restoreFederationHubAdminClient(t, func(
					context.Context, daemonTarget,
				) (federationHubAdminAPI, daemonTarget, error) {
					return nil, daemonTarget{}, tc.clientErr
				})
			}
			m = typeThrough(sendRune(m, 'N'), "build-box")

			m, cmd := pressEnter(m)
			m = finishName(t, m, cmd)

			assert.Equal(t, federationModeNamePrompt, m.federation.mode)
			assert.Equal(t, "build-box", m.federation.nameDraft.Input, "the typed text stays")
			assert.False(t, m.federation.op.running)
			assert.Empty(t, m.federation.notice)
			assert.Empty(t, hub.spokeNameWrites)
			rendered := stripANSI(renderFederation(m))
			assert.Contains(t, rendered, tc.wantContains)
			if tc.unsent {
				assert.NotContains(t, rendered, "may have saved")
			}
		})
	}
}

func TestFederationNameSaveInFlightIgnoresEscAndRepeatedEnter(t *testing.T) {
	m, hub, _ := setupSpokeNameList(t)
	m = typeThrough(sendRune(m, 'N'), "build-box")
	m, cmd := pressEnter(m)
	require.NotNil(t, cmd)

	again, repeat := pressEnter(m)
	assert.Nil(t, repeat, "a second Enter must not send a second request")
	escaped := sendKey(again, tea.KeyEscape)
	assert.Equal(t, federationModeNamePrompt, escaped.federation.mode)
	typed := sendRune(escaped, 'x')
	assert.Equal(t, "build-box", typed.federation.nameDraft.Input, "typing waits for the reply")

	finished := finishName(t, typed, cmd)

	assert.Equal(t, federationModeList, finished.federation.mode)
	assert.Len(t, hub.spokeNameWrites, 1)
}

func TestFederationNameReplyIsDroppedWhenStale(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale func(msg *federationNameResultMsg, m *Model)
	}{
		{"connection changed", func(_ *federationNameResultMsg, m *Model) { m.connGen++ }},
		{"another operation owns the slot", func(msg *federationNameResultMsg, _ *Model) { msg.attempt++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := setupSpokeNameList(t)
			m.connGen = 5 // a reply stamped 0 is never treated as stale
			m = typeThrough(sendRune(m, 'N'), "build-box")
			m, cmd := pressEnter(m)
			msg := cmd().(federationNameResultMsg)
			require.Equal(t, uint64(5), msg.connGen)
			tc.stale(&msg, &m)

			out, _ := updateModel(m, msg)

			assert.Equal(t, federationModeNamePrompt, out.federation.mode)
			assert.True(t, out.federation.op.running, "the real reply is still awaited")
			assert.Empty(t, out.federation.notice)
		})
	}
}

func TestFederationNameEscCancelsWithoutWriting(t *testing.T) {
	m, hub, _ := setupSpokeNameList(t)
	m.federation.mode = federationModeDetail
	m = typeThrough(sendRune(m, 'N'), "build-box")

	m = sendKey(m, tea.KeyEscape)

	assert.Equal(t, federationModeDetail, m.federation.mode)
	assert.Empty(t, hub.calls)
}

func TestFederationNameKeyAdvertisedOnlyWhereARowCanBeNamed(t *testing.T) {
	withRow := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	assert.Contains(t, stripANSI(renderFederation(withRow)), "[N] name")
	withRow.federation.mode = federationModeDetail
	assert.Contains(t, stripANSI(renderFederation(withRow)), "[N] name")

	empty := setupFederationViewWithStatuses()
	assert.NotContains(t, stripANSI(renderFederation(empty)), "[N] name")
}

// The enroll preview sends the name the way `kata federation enroll
// --spoke-name` does: as spoke_name, after the daemon is known to accept it.
func TestFederationEnrollSendsTheSpokeNameTheCLIWouldSend(t *testing.T) {
	var enrollBody map[string]any
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{
		hubAPIVersion: "0.54.0", enrollmentBody: &enrollBody,
	})
	m.federation.draft.AdoptExisting = false
	m.federation.draft.Operation = federationOperationAdoptSelectedHub

	m = typeThrough(sendRune(m, 'N'), "build-box")
	m, cmd := pressEnter(m)
	require.Nil(t, cmd, "saving the field in the preview sends nothing")
	require.Equal(t, federationModePreview, m.federation.mode)
	assert.Equal(t, "build-box", m.federation.draft.SpokeName)
	assert.Contains(t, stripANSI(renderFederation(m)), "spoke name: build-box")

	m, cmd = pressEnter(m)
	require.NotNil(t, cmd)
	out, _ := updateModel(m, cmd())

	assert.Equal(t, federationModeResult, out.federation.mode)
	assert.Equal(t, "build-box", enrollBody["spoke_name"])
}

func TestFederationEnrollWithoutANameSendsNone(t *testing.T) {
	var enrollBody map[string]any
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{enrollmentBody: &enrollBody})
	m.federation.draft.AdoptExisting = false
	m.federation.draft.Operation = federationOperationAdoptSelectedHub

	m, cmd := pressEnter(m)
	require.NotNil(t, cmd)
	out, _ := updateModel(m, cmd())

	assert.Equal(t, federationModeResult, out.federation.mode, "no health probe is needed without a name")
	assert.NotContains(t, enrollBody, "spoke_name")
	assert.Contains(t, stripANSI(renderFederation(m)), "spoke name: -")
}

func TestFederationEnrollWithNameRefusesOlderHubBeforeAnyHubWrite(t *testing.T) {
	var enrollBody map[string]any
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{
		hubAPIVersion: "0.37.0", enrollmentBody: &enrollBody,
	})
	m.federation.draft.AdoptExisting = false
	m.federation.draft.Operation = federationOperationCreateReplica
	m.federation.draft.SpokeName = "build-box"

	m, cmd := pressEnter(m)
	require.NotNil(t, cmd)
	out, _ := updateModel(m, cmd())

	assert.Equal(t, federationModePreview, out.federation.mode)
	assert.Empty(t, enrollBody, "no enrollment was created")
	assert.Equal(t, "build-box", out.federation.draft.SpokeName, "the name stays for another hub")
	assert.Contains(t, stripANSI(renderFederation(out)), "naming a spoke requires daemon API 0.38.0 or newer")
}

func TestFederationEnrollNameRefusesAnInvalidNameLikeTheCLI(t *testing.T) {
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{})
	m = typeThrough(sendRune(m, 'N'), "Build Box")

	m, cmd := pressEnter(m)

	require.Nil(t, cmd)
	assert.Equal(t, federationModeNamePrompt, m.federation.mode)
	assert.Empty(t, m.federation.draft.SpokeName)
	assert.Contains(t, stripANSI(renderFederation(m)), `"Build Box" must match [a-z][a-z0-9-]{0,31}`)
}

func TestFederationEnrollNameEscKeepsThePreviousName(t *testing.T) {
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{})
	m.federation.draft.SpokeName = "laptop"

	m = sendRune(m, 'N')
	require.Equal(t, "laptop", m.federation.nameDraft.Input, "the prompt starts from the current name")
	m = typeThrough(m, "-x")
	m = sendKey(m, tea.KeyEscape)

	assert.Equal(t, federationModePreview, m.federation.mode)
	assert.Equal(t, "laptop", m.federation.draft.SpokeName)
}

func TestFederationEnrollNameEmptyInputRemovesTheName(t *testing.T) {
	m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{})
	m.federation.draft.SpokeName = "laptop"
	m = sendRune(m, 'N')
	for range "laptop" {
		m = sendKey(m, tea.KeyBackspace)
	}

	m, _ = pressEnter(m)

	assert.Equal(t, federationModePreview, m.federation.mode)
	assert.Empty(t, m.federation.draft.SpokeName)
}

// longSpokeNameError is a daemon error spanning several lines, one of them
// longer than the narrowest supported terminal.
var longSpokeNameError = errors.New("federation_spoke_name_conflict: \"build-box\" is held by spoke 01HZNQ7VFPK1XGD8R5MABCD4EB\n" +
	"rename that spoke first: kata federation enrollments name 01HZNQ7VFPK1XGD8R5MABCD4EB other-name\n" +
	"or choose another name here")

func TestFederationNamePromptFitsEverySupportedTerminal(t *testing.T) {
	longProject := strings.Repeat("example-project-", 12)
	fixture := func(failed bool) Model {
		m, _, _ := setupSpokeNameList(t)
		row := federationStatusFixture(longProject, "spoke")
		m.federation.statuses = []FederationProjectStatus{row}
		m = sendRune(m, 'N')
		m = typeThrough(m, "abcdefghijklmnopqrstuvwxyz-12345")
		if failed {
			m.federation.op.err = longSpokeNameError
		}
		return m
	}
	for _, size := range []struct {
		name          string
		width, height int
		split         bool
	}{
		{"80x24", 80, 24, false},
		{"80x12", 80, 12, false},
		{"100x8", 100, 8, false},
		{"split 160x40", 160, 40, true},
	} {
		for _, failed := range []bool{false, true} {
			name := size.name
			if failed {
				name += " with a multiline daemon error"
			}
			t.Run(name, func(t *testing.T) {
				m := fixture(failed)
				m.width, m.height = size.width, size.height
				if size.split {
					m.layout = splitlayout.Split
				}

				rendered := stripANSI(m.viewContent())

				lines := strings.Split(rendered, "\n")
				assert.LessOrEqual(t, len(lines), size.height, "every row fits:\n%s", rendered)
				for _, line := range lines {
					assert.LessOrEqual(t, ansi.StringWidth(line), size.width, "no line overflows:\n%s", rendered)
				}
				assert.Contains(t, rendered, "name: abcdefghijklmnopqrstuvwxyz-12345", "the prompt keeps its row")
				assert.Contains(t, rendered, "[enter] save", "the action hint keeps its row")
				if failed {
					flat := strings.Join(strings.Fields(rendered), " ")
					assert.Contains(t, flat, "federation_spoke_name_conflict")
					assert.Contains(t, flat, "or choose another name here", "the last error line is not dropped")
				}
			})
		}
	}
}

// Over the quit confirmation the footer becomes the help table, which takes
// more rows than the one-line hint.
func TestFederationNamePromptKeepsPromptAndErrorOverHelpFooter(t *testing.T) {
	m, _, _ := setupSpokeNameList(t)
	m = typeThrough(sendRune(m, 'N'), "build-box")
	m.federation.op.err = longSpokeNameError
	m.width, m.height = 80, 24
	m.modal = modalQuitConfirm

	rendered := stripANSI(renderFederation(m))

	flat := strings.Join(strings.Fields(rendered), " ")
	assert.Contains(t, flat, "name: build-box")
	assert.Contains(t, flat, "or choose another name here")
	assert.Contains(t, flat, "confirm")
	assert.LessOrEqual(t, len(strings.Split(rendered, "\n")), m.height)
}

func TestFederationListFooterWrapsToTheTerminalWidth(t *testing.T) {
	m := setupFederationViewWithStatuses(federationStatusFixture("spoke-proj", "spoke"))
	m.width = 80

	rendered := stripANSI(renderFederation(m))

	var hints []string
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, "[") {
			assert.LessOrEqual(t, ansi.StringWidth(line), 80, "hint row overflows: %q", line)
			hints = append(hints, line)
		}
	}
	assert.Greater(t, len(hints), 1, "the hints wrap onto more rows")
	joined := strings.Join(hints, " ")
	for _, hint := range []string{"[N] name", "[?] help", "[b] browse hubs"} {
		assert.Contains(t, joined, hint, "no hint is clipped")
	}
}

func TestFederationEnrollPreviewKeepsErrorAndHintsOnShortTerminals(t *testing.T) {
	for _, size := range []struct {
		name          string
		width, height int
		split         bool
	}{
		{"80x24", 80, 24, false},
		{"80x12", 80, 12, false},
		{"100x8", 100, 8, false},
		{"split 160x40", 160, 40, true},
	} {
		t.Run(size.name, func(t *testing.T) {
			m, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{})
			m.federation.draft.SpokeProjectName = strings.Repeat("example-project-", 12)
			m.federation.draft.SpokeName = "build-box"
			m.federation.op.err = longSpokeNameError
			m.width, m.height = size.width, size.height
			if size.split {
				m.layout = splitlayout.Split
			}

			rendered := stripANSI(m.viewContent())

			assert.LessOrEqual(t, len(strings.Split(rendered, "\n")), size.height, "every row fits:\n%s", rendered)
			flat := strings.Join(strings.Fields(rendered), " ")
			assert.Contains(t, flat, "federation_spoke_name_conflict")
			assert.Contains(t, flat, "or choose another name here", "the last error line is not dropped")
			assert.Contains(t, flat, "[enter] confirm [N] name [esc] back")
		})
	}
}
