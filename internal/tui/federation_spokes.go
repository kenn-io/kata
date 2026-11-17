package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

type federationHubSpokesState struct {
	enrollments []FederationEnrollment
	cursor      int
	gen         uint64
	loading     bool
	err         error
	name        textinput.Model
	returnMode  federationMode
	saving      bool
}

type federationSpokesLoadedMsg struct {
	connGen, gen uint64
	enrollments  []FederationEnrollment
	err          error
}

type federationSpokeNamedMsg struct {
	connGen, gen uint64
	instanceUID  string
	name         string
	err          error
}

func (m Model) loadFederationSpokes() (Model, tea.Cmd) {
	m.federation.spokes.gen++
	m.federation.spokes.loading = true
	m.federation.spokes.err = nil
	api := m.api
	connGen, gen := m.connGen, m.federation.spokes.gen
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rows, err := api.ListFederationEnrollments(ctx)
		return federationSpokesLoadedMsg{connGen: connGen, gen: gen, enrollments: rows, err: err}
	}
}

func (m Model) handleFederationSpokesLoaded(msg federationSpokesLoadedMsg) Model {
	if m.staleConnMsg(msg.connGen) || msg.gen != m.federation.spokes.gen {
		return m
	}
	m.federation.spokes.loading = false
	m.federation.spokes.err = msg.err
	if msg.err == nil {
		m.federation.spokes.enrollments = msg.enrollments
		m.federation.spokes.cursor = clampFederationIndex(m.federation.spokes.cursor, len(m.federationHubSpokes()), 0)
	}
	return m
}

// A name belongs to the instance across all of its project enrollments.
func (m Model) federationHubSpokes() []FederationEnrollment {
	seen := make(map[string]bool)
	var rows []FederationEnrollment
	for _, row := range m.federation.spokes.enrollments {
		if !seen[row.SpokeInstanceUID] {
			seen[row.SpokeInstanceUID] = true
			rows = append(rows, row)
		}
	}
	return rows
}

func (m Model) routeFederationSpokesKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	rows := m.federationHubSpokes()
	if m.federation.mode == federationModeSpokes {
		if cursor, ok := nextFederationCursor(msg, m.federation.spokes.cursor, len(rows)); ok {
			m.federation.spokes.cursor = cursor
			return m, nil
		}
	}
	switch msg.String() {
	case "esc", "backspace":
		if m.federation.mode == federationModeSpokeDetail {
			m.federation.mode = federationModeSpokes
		} else {
			m.federation.mode = federationModeList
			m.federation.spokes.gen++
		}
	case "r":
		return m.loadFederationSpokes()
	case "enter", "N":
		cursor := m.federation.spokes.cursor
		if m.federation.spokes.loading || m.federation.spokes.err != nil || cursor < 0 || cursor >= len(rows) {
			return m, nil
		}
		if msg.String() == "N" {
			return m.startFederationSpokeName(rows[cursor].SpokeName)
		}
		m.federation.mode = federationModeSpokeDetail
	}
	return m, nil
}

func (m Model) startFederationSpokeName(name string) (Model, tea.Cmd) {
	m.federation.spokes.returnMode = m.federation.mode
	m.federation.spokes.gen++
	m.federation.spokes.err = nil
	m.federation.spokes.saving = false
	m.federation.spokes.name = textinput.New()
	m.federation.spokes.name.SetValue(name)
	m.federation.spokes.name.CursorEnd()
	m.federation.mode = federationModeSpokeName
	return m, m.federation.spokes.name.Focus()
}

func (m Model) routeFederationSpokeNameKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	if m.federation.spokes.saving {
		return m, nil
	}
	switch msg.String() {
	case "esc":
		m.federation.mode = m.federation.spokes.returnMode
		m.federation.spokes.err = nil
		return m, nil
	case "enter":
		name := m.federation.spokes.name.Value()
		if m.federation.spokes.returnMode == federationModePreview {
			m.federation.draft.SpokeName = name
			m.federation.mode = federationModePreview
			return m, nil
		}
		rows := m.federationHubSpokes()
		uid := rows[m.federation.spokes.cursor].SpokeInstanceUID
		m.federation.spokes.saving = true
		m.federation.spokes.err = nil
		api := m.api
		connGen, gen := m.connGen, m.federation.spokes.gen
		return m, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			name, err := api.NameFederationSpoke(ctx, uid, name)
			return federationSpokeNamedMsg{connGen: connGen, gen: gen, instanceUID: uid, name: name, err: err}
		}
	}
	var cmd tea.Cmd
	m.federation.spokes.name, cmd = m.federation.spokes.name.Update(msg)
	return m, cmd
}

func (m Model) handleFederationSpokeNamed(msg federationSpokeNamedMsg) Model {
	if m.staleConnMsg(msg.connGen) || msg.gen != m.federation.spokes.gen ||
		m.federation.mode != federationModeSpokeName || !m.federation.spokes.saving {
		return m
	}
	m.federation.spokes.saving = false
	m.federation.spokes.err = msg.err
	if msg.err != nil {
		return m
	}
	for i := range m.federation.spokes.enrollments {
		if m.federation.spokes.enrollments[i].SpokeInstanceUID == msg.instanceUID {
			m.federation.spokes.enrollments[i].SpokeName = msg.name
		}
	}
	m.federation.mode = m.federation.spokes.returnMode
	return m
}

func renderFederationSpokes(m Model) string {
	body := federationModeHeader(m, "Hub spokes")
	rows := m.federationHubSpokes()
	cursor := m.federation.spokes.cursor
	switch {
	case m.federation.spokes.loading:
		body = append(body, "loading hub spokes...")
	case m.federation.spokes.err != nil:
		body = append(body, errorStyle.Render(sanitizeForLine(m.federation.spokes.err.Error())))
	case len(rows) == 0:
		body = append(body, "no enrolled spokes")
	case m.federation.mode == federationModeSpokeDetail && cursor >= 0 && cursor < len(rows):
		row := rows[cursor]
		body = append(body, "name: "+sanitizeForLine(emptyDash(row.SpokeName)), "instance: "+sanitizeForLine(row.SpokeInstanceUID))
		for _, enrollment := range m.federation.spokes.enrollments {
			if enrollment.SpokeInstanceUID == row.SpokeInstanceUID {
				state := "active"
				if enrollment.RevokedAt != nil {
					state = "revoked"
				}
				body = append(body, fmt.Sprintf("enrollment %d: actor %s · %s", enrollment.ID, sanitizeForLine(enrollment.Actor), state))
			}
		}
	default:
		budget := max(m.height-len(body)-3, 1)
		start, end := windowBounds(len(rows), cursor, budget)
		for i := start; i < end; i++ {
			body = append(body, renderFederationChoice(sanitizeForLine(emptyDash(rows[i].SpokeName))+"  "+sanitizeForLine(rows[i].SpokeInstanceUID), i == cursor))
		}
	}
	body = append(body, "", renderAuxiliaryFooter(m, "[↑/↓ k/j] move  [enter] detail  [N] name  [r] refresh  [esc] back"))
	return strings.Join(body, "\n")
}

func renderFederationSpokeName(m Model) string {
	body := federationModeHeader(m, "Spoke name")
	hint := "Blank clears the hub-local name."
	if m.federation.spokes.returnMode == federationModePreview {
		hint = "Optional. Blank keeps the current hub-local name."
	} else {
		rows := m.federationHubSpokes()
		body = append(body, "instance: "+sanitizeForLine(rows[m.federation.spokes.cursor].SpokeInstanceUID))
	}
	body = append(body, m.federation.spokes.name.View(), subtleStyle.Render(hint))
	if m.federation.spokes.saving {
		body = append(body, "saving...")
	}
	if m.federation.spokes.err != nil {
		body = append(body, errorStyle.Render(sanitizeForLine(m.federation.spokes.err.Error())))
	}
	body = append(body, "", renderAuxiliaryFooter(m, "[enter] save  [esc] cancel"))
	return strings.Join(body, "\n")
}
