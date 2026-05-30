package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDaemonView_DKeyTransitionsFromList(t *testing.T) {
	m := setupDaemonViewSource()

	out, cmd := updateModel(m, keyRune('D'))

	require.Nil(t, cmd)
	assert.Equal(t, viewDaemons, out.view)
	assert.Equal(t, viewList, out.prevView)
	assert.Equal(t, 1, out.daemonCursor, "cursor should land on the active daemon")
}

func TestDaemonView_EscReturnsToPreviousView(t *testing.T) {
	m := setupDaemonViewSource()
	m.view = viewDetail
	m.detail = detailModel{issue: &Issue{ShortID: "abc1"}}

	out, _ := updateModel(m, keyRune('D'))
	out, cmd := out.routeDaemonsViewKey(tea.KeyMsg{Type: tea.KeyEsc})

	require.Nil(t, cmd)
	assert.Equal(t, viewDetail, out.view)
	assert.Equal(t, "abc1", out.detail.issue.ShortID)
}

func TestDaemonView_CursorMovement(t *testing.T) {
	m := setupDaemonView()

	out, _ := m.routeDaemonsViewKey(keyRune('j'))
	assert.Equal(t, 1, out.daemonCursor)
	out, _ = out.routeDaemonsViewKey(keyRune('j'))
	assert.Equal(t, 2, out.daemonCursor)
	out, _ = out.routeDaemonsViewKey(keyRune('k'))
	assert.Equal(t, 1, out.daemonCursor)
	out, _ = out.routeDaemonsViewKey(tea.KeyMsg{Type: tea.KeyHome})
	assert.Equal(t, 0, out.daemonCursor)
	out, _ = out.routeDaemonsViewKey(tea.KeyMsg{Type: tea.KeyEnd})
	assert.Equal(t, 2, out.daemonCursor)
}

func TestDaemonView_RenderIncludesDaemonRows(t *testing.T) {
	m := setupDaemonView()

	out := stripANSI(renderDaemons(m))

	assertContains(t, out, "kata / daemons", "missing daemon view title")
	assertContains(t, out, "local", "missing local daemon")
	assertContains(t, out, "shared", "missing shared daemon")
	assertContains(t, out, "daemon.internal:7777", "missing endpoint host")
	assertContains(t, out, "token", "missing token indicator")
	assertContains(t, out, "current", "missing current marker")
}

func TestDaemonView_HelpIncludesDaemonBinding(t *testing.T) {
	out := stripANSI(renderHelp(newKeymap(), 100, ListFilter{}))

	assertContains(t, out, "D", "help overlay missing daemon binding")
	assertContains(t, out, "daemons", "help overlay missing daemon description")
}

func setupDaemonViewSource() Model {
	m := initialModel(Options{})
	m.view = viewList
	m.width, m.height = 120, 24
	m.activeDaemon = daemonTarget{Name: "shared", URL: "http://daemon.internal:7777"}
	m.daemonTargets = []daemonTarget{
		{Name: "local", Local: true},
		{Name: "shared", URL: "http://daemon.internal:7777", Token: "tok", AllowInsecure: true},
		{Name: "prod", URL: "https://kata.example.test"},
	}
	return m
}

func setupDaemonView() Model {
	m := setupDaemonViewSource()
	m.view = viewDaemons
	return m
}
