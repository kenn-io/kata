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

func TestFederationNameKeyOpensPromptWithoutReadingAnything(t *testing.T) {
	enroll, _ := setupFederationExecutionPreview(t, federationExecutionServerOptions{})
	enroll.federation.draft.SpokeName = "laptop"
	for _, tc := range []struct {
		name      string
		model     func(t *testing.T) Model
		wantInput string
	}{
		{"list", func(t *testing.T) Model { m, _, _ := setupSpokeNameList(t); return m }, ""},
		{"detail", func(t *testing.T) Model {
			m, _, _ := setupSpokeNameList(t)
			m.federation.mode = federationModeDetail
			return m
		}, ""},
		{"enroll preview", func(*testing.T) Model { return enroll }, "laptop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.model(t)
			from := m.federation.mode

			out, cmd := updateModel(m, keyRune('N'))

			require.Nil(t, cmd)
			assert.Equal(t, federationModeNamePrompt, out.federation.mode)
			assert.Equal(t, from, out.federation.nameDraft.Return)
			assert.Equal(t, tc.wantInput, out.federation.nameDraft.Input)
		})
	}
}

func TestFederationNameKeyNeedsASpokeRow(t *testing.T) {
	empty := setupFederationViewWithStatuses()

	out, cmd := updateModel(empty, keyRune('N'))

	require.Nil(t, cmd)
	assert.Equal(t, federationModeList, out.federation.mode)
	assert.NotContains(t, stripANSI(renderFederation(empty)), "[N] name", "an empty list shows no row hint")
}

// A name is lowercase letters, so it contains keys the TUI reserves for quit
// (q), undo (u) and global navigation. While the prompt owns input they are
// text, and so is a paste.
func TestFederationNamePromptOwnsTheKeyboard(t *testing.T) {
	m, _, _ := setupSpokeNameList(t)
	m = typeThrough(sendRune(m, 'N'), "quartz-u1")

	assert.Equal(t, modalNone, m.modal, "q must not open the quit confirmation")
	assert.Equal(t, federationModeNamePrompt, m.federation.mode)
	assert.Equal(t, "quartz-u1", m.federation.nameDraft.Input)

	m = sendKey(m, tea.KeyBackspace)
	m, _ = updateModel(m, tea.PasteMsg{Content: "-box\n"})
	assert.Equal(t, "quartz-u-box", m.federation.nameDraft.Input)
}

func TestFederationNameSaveWritesToTheRowsHub(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       federationMode
		input      string
		wantWrite  string
		wantNotice string
	}{
		{"list", federationModeList, "build-box", "build-box", `named this spoke "build-box" on hub.internal:7777`},
		{"detail returns to detail", federationModeDetail, "laptop", "laptop", `named this spoke "laptop"`},
		{"empty input clears", federationModeList, "", "", "cleared the name of this spoke on hub.internal:7777"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, hub, opened := setupSpokeNameList(t)
			m.federation.mode = tc.mode
			m.daemonTargets = []daemonTarget{
				m.activeDaemon,
				daemonTargetWithResolvedAuth("catalog-hub", "http://hub.internal:7777/", "hub-credential", true),
			}
			m = typeThrough(sendRune(m, 'N'), tc.input)

			m, cmd := pressEnter(m)
			assert.True(t, m.federation.op.running)
			m = finishName(t, m, cmd)

			assert.Equal(t, []spokeNameWrite{{uid: "01HZNQ7VFPK1XGD8R5MABCD4EA", name: tc.wantWrite}}, hub.spokeNameWrites)
			assert.Equal(t, "http://hub.internal:7777", opened.URL, "the name goes to the hub this row is bound to")
			assert.Equal(t, "hub-credential", opened.resolved.Token, "with the catalog credential for that hub")
			assert.Equal(t, tc.mode, m.federation.mode)
			assert.False(t, m.federation.op.running)
			assert.Contains(t, stripANSI(renderFederation(m)), tc.wantNotice)
		})
	}
}

func TestFederationNameSaveRefusesBadInputWithoutContactingTheHub(t *testing.T) {
	for _, tc := range []struct {
		name     string
		instance InstanceInfo
		want     string
	}{
		{"invalid name", InstanceInfo{InstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA"}, `"Build Box" must match [a-z][a-z0-9-]{0,31}`},
		{"instance not loaded", InstanceInfo{}, "refresh federation status before naming"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, hub, opened := setupSpokeNameList(t)
			m.federation.instance = tc.instance
			m = typeThrough(sendRune(m, 'N'), "Build Box")

			m, cmd := pressEnter(m)

			require.Nil(t, cmd)
			assert.Equal(t, federationModeNamePrompt, m.federation.mode)
			assert.Equal(t, "Build Box", m.federation.nameDraft.Input, "the typed text stays")
			assert.Empty(t, opened.URL)
			assert.Empty(t, hub.calls)
			assert.Contains(t, stripANSI(renderFederation(m)), tc.want)
		})
	}
}

func TestFederationNameSaveFailuresKeepTheTextAndSayWhatHappened(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hubErr    error
		clientErr error
		want      string
		unsent    bool
	}{
		{
			name:   "the daemon refuses",
			hubErr: &APIError{Status: 403, Code: "forbidden", Message: "owner credential required"},
			want:   "forbidden: owner credential required", unsent: true,
		},
		{
			name:      "hub client cannot be built, nothing was sent",
			clientErr: errors.New(`daemon "catalog-hub" token_env is unset`),
			want:      "token_env is unset", unsent: true,
		},
		{
			name:   "transport failure after the send",
			hubErr: errors.New("read tcp: connection reset"),
			want:   "the hub may have saved this name; save again to check",
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
			assert.Contains(t, rendered, tc.want)
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

func enrollWithName(t *testing.T, opts federationExecutionServerOptions, name string) Model {
	t.Helper()
	m, _ := setupFederationExecutionPreview(t, opts)
	m.federation.draft.AdoptExisting = false
	m.federation.draft.Operation = federationOperationAdoptSelectedHub
	m.federation.draft.SpokeName = name
	return m
}

// The enroll preview sends the name the way `kata federation enroll
// --spoke-name` does: as spoke_name, after the hub is known to accept it.
func TestFederationEnrollSendsTheSpokeNameTheCLIWouldSend(t *testing.T) {
	var enrollBody map[string]any
	m := enrollWithName(t, federationExecutionServerOptions{hubAPIVersion: "0.54.0", enrollmentBody: &enrollBody}, "")

	m = typeThrough(sendRune(m, 'N'), "build-box")
	m, cmd := pressEnter(m)
	require.Nil(t, cmd, "saving the field in the preview sends nothing")
	assert.Equal(t, "build-box", m.federation.draft.SpokeName)
	assert.Contains(t, stripANSI(renderFederation(m)), "spoke name: build-box")
	m, cmd = pressEnter(m)
	out, _ := updateModel(m, cmd())

	assert.Equal(t, federationModeResult, out.federation.mode)
	assert.Equal(t, "build-box", enrollBody["spoke_name"])
}

func TestFederationEnrollWithoutANameSendsNoneAndSkipsTheVersionCheck(t *testing.T) {
	var enrollBody map[string]any
	m := enrollWithName(t, federationExecutionServerOptions{enrollmentBody: &enrollBody}, "")

	m, cmd := pressEnter(m)
	out, _ := updateModel(m, cmd())

	assert.Equal(t, federationModeResult, out.federation.mode, "the hub's health route is not registered")
	assert.NotContains(t, enrollBody, "spoke_name")
	assert.Contains(t, stripANSI(renderFederation(m)), "spoke name: -")
}

func TestFederationEnrollWithNameRefusesOlderHubBeforeAnyHubWrite(t *testing.T) {
	var enrollBody map[string]any
	m := enrollWithName(t, federationExecutionServerOptions{hubAPIVersion: "0.37.0", enrollmentBody: &enrollBody}, "build-box")

	m, cmd := pressEnter(m)
	out, _ := updateModel(m, cmd())

	assert.Equal(t, federationModePreview, out.federation.mode)
	assert.Empty(t, enrollBody, "no enrollment was created")
	assert.Equal(t, "build-box", out.federation.draft.SpokeName, "the name stays for another hub")
	assert.Contains(t, stripANSI(renderFederation(out)), "naming a spoke requires daemon API 0.38.0 or newer")
}

// longSpokeNameError is a daemon error spanning several lines, one of them
// longer than the narrowest supported terminal.
var longSpokeNameError = errors.New("federation_spoke_name_conflict: \"build-box\" is held by spoke 01HZNQ7VFPK1XGD8R5MABCD4EB\n" +
	"rename that spoke first: kata federation enrollments name 01HZNQ7VFPK1XGD8R5MABCD4EB other-name\n" +
	"or choose another name here")

func TestFederationNameScreensFitShortTerminals(t *testing.T) {
	longProject := strings.Repeat("example-project-", 12)
	prompt := func(t *testing.T) Model {
		m, _, _ := setupSpokeNameList(t)
		m.federation.statuses = []FederationProjectStatus{federationStatusFixture(longProject, "spoke")}
		return typeThrough(sendRune(m, 'N'), "abcdefghijklmnopqrstuvwxyz-12345")
	}
	preview := func(t *testing.T) Model {
		m := enrollWithName(t, federationExecutionServerOptions{}, "build-box")
		m.federation.draft.SpokeProjectName = longProject
		return m
	}
	for _, screen := range []struct {
		name  string
		model func(*testing.T) Model
		want  []string
	}{
		{"prompt", prompt, []string{"name: abcdefghijklmnopqrstuvwxyz-12345", "[enter] save  [esc] cancel"}},
		{"enroll preview", preview, []string{"[enter] confirm  [N] name  [esc] back"}},
	} {
		for _, size := range []struct {
			name          string
			width, height int
			split, help   bool
		}{
			{"80x12", 80, 12, false, false},
			{"100x8", 100, 8, false, false},
			{"split 160x40", 160, 40, true, false},
			{"80x12 over help", 80, 12, false, true},
		} {
			t.Run(screen.name+" "+size.name, func(t *testing.T) {
				m := screen.model(t)
				m.federation.op.err = longSpokeNameError
				m.width, m.height = size.width, size.height
				if size.split {
					m.layout = splitlayout.Split
				}
				want := screen.want
				if size.help {
					m.modal = modalQuitConfirm
					want = []string{"confirm"} // the help table replaces the hints
				}

				// The quit box overlays the screen; judge the screen's own rows.
				rendered := stripANSI(m.viewContent())
				if size.help {
					rendered = stripANSI(renderFederation(m))
				}

				assert.LessOrEqual(t, len(strings.Split(rendered, "\n")), size.height, "every row fits:\n%s", rendered)
				flat := strings.Join(strings.Fields(rendered), " ")
				assert.Contains(t, flat, "federation_spoke_name_conflict")
				assert.Contains(t, flat, "or choose another name here", "the whole error stays")
				for _, w := range want {
					assert.Contains(t, flat, strings.Join(strings.Fields(w), " "))
				}
				if screen.name == "prompt" {
					for _, line := range strings.Split(rendered, "\n") {
						assert.LessOrEqual(t, ansi.StringWidth(line), size.width, "no line overflows:\n%s", rendered)
					}
				}
			})
		}
	}
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
