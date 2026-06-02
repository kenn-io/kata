package tui

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	hubfederation "go.kenn.io/kata/internal/federation"
)

const federationViewChromeRows = 9

func renderFederation(m Model) string {
	rows := federationSpokeStatuses(m.federationStatuses)
	cursor := clampFederationCursor(m.federationCursor, rows)
	if m.federationMode == federationModeDetail {
		return renderFederationDetail(m, rows, cursor)
	}
	rowBudget := len(rows)
	if m.height > 0 {
		rowBudget = m.height - federationViewChromeRows
		if rowBudget < 1 {
			rowBudget = 1
		}
	}
	visible := clipFederationRows(rows, cursor, rowBudget)
	body := []string{
		titleStyle.Render("kata / federation"),
		subtleStyle.Render(fmt.Sprintf("%d spoke federations", len(rows))),
		"",
		subtleStyle.Render(federationHeaderLine(m)),
		"",
		renderFederationHeader(m.width),
	}
	if m.federationLoading {
		body = append(body, subtleStyle.Render("  loading federation status..."))
	} else if m.federationErr != nil {
		body = append(body, errorStyle.Render("  failed to load federation: "+sanitizeForLine(m.federationErr.Error())))
	} else if len(rows) == 0 {
		body = append(body, subtleStyle.Render("  no spoke federation enrollments"))
	} else {
		for _, vr := range visible {
			body = append(body, renderFederationRow(vr.row, vr.index == cursor, m.width))
		}
	}
	body = append(body, "")
	if cursor >= 0 && cursor < len(rows) {
		body = append(body, subtleStyle.Render(federationFooter(rows[cursor], m.width)))
	}
	body = append(body, "")
	body = append(body, subtleStyle.Render(
		"[↑/↓ k/j] move  [enter] detail  [esc] back  [r] refresh  [n] enroll  [b] browse hubs  [?] help"))
	return strings.Join(body, "\n")
}

type federationVisibleRow struct {
	row   FederationProjectStatus
	index int
}

func clipFederationRows(rows []FederationProjectStatus, cursor, budget int) []federationVisibleRow {
	if budget <= 0 || len(rows) == 0 {
		return nil
	}
	if len(rows) <= budget {
		out := make([]federationVisibleRow, 0, len(rows))
		for i, row := range rows {
			out = append(out, federationVisibleRow{row: row, index: i})
		}
		return out
	}
	start, end := windowBounds(len(rows), cursor, budget)
	out := make([]federationVisibleRow, 0, end-start)
	for i := start; i < end; i++ {
		out = append(out, federationVisibleRow{row: rows[i], index: i})
	}
	return out
}

func federationHeaderLine(m Model) string {
	return fmt.Sprintf("Federation for active daemon: %s %s instance %s auth %s",
		sanitizeForLine(daemonName(m.activeDaemon)),
		sanitizeForLine(federationDaemonEndpoint(m.activeDaemon)),
		sanitizeForLine(emptyDash(m.federationInstance.InstanceUID)),
		daemonAuth(m.activeDaemon),
	)
}

func federationDaemonEndpoint(target daemonTarget) string {
	if target.Local {
		return "local"
	}
	if target.URL != "" {
		return target.URL
	}
	return daemonEndpoint(target)
}

func renderFederationHeader(width int) string {
	return federationRowLayout("Project", "Hub", "Actor", "Push", "Pending", "Sync", "Badges", width, false)
}

func renderFederationRow(row FederationProjectStatus, highlight bool, width int) string {
	push := "off"
	if row.PushEnabled {
		push = "push"
	}
	sync := "never"
	if row.LastSuccessfulSyncAt != nil {
		sync = humanizeRelative(*row.LastSuccessfulSyncAt)
	} else if row.LastError != nil {
		sync = "error"
	}
	return federationRowLayout(
		sanitizeForLine(row.ProjectName),
		sanitizeForLine(federationHubDisplay(row.HubURL)),
		sanitizeForLine(emptyDash(row.BoundActor)),
		push,
		fmt.Sprintf("%d", row.PendingPushCount),
		sync,
		federationBadges(row),
		width,
		highlight,
	)
}

func federationRowLayout(project, hub, actor, push, pending, sync, badges string, width int, highlight bool) string {
	const (
		hubW     = 22
		actorW   = 12
		pushW    = 6
		pendingW = 7
		syncW    = 12
		gap      = 2
	)
	badgesW := 22
	if width >= 120 {
		badgesW = 36
	}
	projectW := width - (hubW + actorW + pushW + pendingW + syncW + badgesW + 6*gap) - 2
	if projectW < 10 {
		projectW = 10
	}
	cursor := "  "
	if highlight {
		cursor = "▶ "
	}
	line := cursor + padToWidth(project, projectW) +
		strings.Repeat(" ", gap) + padToWidth(hub, hubW) +
		strings.Repeat(" ", gap) + padToWidth(actor, actorW) +
		strings.Repeat(" ", gap) + padToWidth(push, pushW) +
		strings.Repeat(" ", gap) + padL(pending, pendingW) +
		strings.Repeat(" ", gap) + padToWidth(sync, syncW) +
		strings.Repeat(" ", gap) + padToWidth(badges, badgesW)
	if highlight {
		line = lipgloss.NewStyle().Bold(true).Render(line)
	}
	return line
}

func federationBadges(row FederationProjectStatus) string {
	badges := []string{}
	if row.AllowInsecure {
		badges = append(badges, "insecure")
	}
	if row.ActiveQuarantineCount > 0 {
		badges = append(badges, "quarantine")
	}
	if row.ResetBlocker != "" {
		badges = append(badges, "reset")
	}
	if row.UnresolvedViolationCount > 0 {
		badges = append(badges, "violations")
	}
	if len(badges) == 0 {
		return "-"
	}
	return strings.Join(badges, ",")
}

func federationFooter(row FederationProjectStatus, width int) string {
	text := fmt.Sprintf("hub %s/project %d · actor %s · credential %s",
		sanitizeForLine(row.HubURL),
		row.HubProjectID,
		sanitizeForLine(emptyDash(row.BoundActor)),
		sanitizeForLine(emptyDash(row.CredentialStatus)),
	)
	return truncate(text, width)
}

func renderFederationDetail(m Model, rows []FederationProjectStatus, cursor int) string {
	body := []string{
		titleStyle.Render("kata / federation"),
		subtleStyle.Render(federationHeaderLine(m)),
		"",
	}
	if cursor < 0 || cursor >= len(rows) {
		body = append(body, subtleStyle.Render("no federation selected"))
		return strings.Join(body, "\n")
	}
	row := rows[cursor]
	body = append(body,
		titleStyle.Render(sanitizeForLine(row.ProjectName)),
		"hub URL: "+sanitizeForLine(row.HubURL),
		fmt.Sprintf("hub project ID: %d", row.HubProjectID),
		"hub project UID: "+sanitizeForLine(emptyDash(row.HubProjectUID)),
		"actor: "+sanitizeForLine(emptyDash(row.BoundActor)),
		"capabilities: "+sanitizeForLine(hubfederation.DisplayCapabilities(row.Capabilities)),
		fmt.Sprintf("push enabled: %t", row.PushEnabled),
		"credential: "+sanitizeForLine(emptyDash(row.CredentialStatus)),
		fmt.Sprintf("allow_insecure: %t", row.AllowInsecure),
		"",
		fmt.Sprintf("pull cursor: %d", row.PullCursorEventID),
		fmt.Sprintf("push cursor: %d", row.PushCursorEventID),
		fmt.Sprintf("pending push: %d", row.PendingPushCount),
		fmt.Sprintf("pending push high water: %d", row.PendingPushHighWaterEventID),
		fmt.Sprintf("pending claims: %d", row.PendingClaimCount),
		fmt.Sprintf("live claims: %d", row.LiveClaimCount),
		fmt.Sprintf("quarantine count: %d", row.ActiveQuarantineCount),
		"reset blocker: "+sanitizeForLine(emptyDash(row.ResetBlocker)),
		fmt.Sprintf("claim violations: %d unresolved, %d recent", row.UnresolvedViolationCount, row.RecentViolationCount),
		"last pull success: "+formatOptionalTime(row.LastPullSuccessAt),
		"last push success: "+formatOptionalTime(row.LastPushSuccessAt),
		"last sync success: "+formatOptionalTime(row.LastSuccessfulSyncAt),
		"last error: "+formatOptionalError(row),
		"",
		subtleStyle.Render("[esc] back  [r] refresh  [q] quit  [?] help"),
	)
	return strings.Join(body, "\n")
}

func federationHubDisplay(raw string) string {
	u, err := url.Parse(raw)
	if err == nil && u.Host != "" {
		return u.Host
	}
	if raw != "" {
		return raw
	}
	return "-"
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func formatOptionalTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

func formatOptionalError(row FederationProjectStatus) string {
	if row.LastError == nil {
		return "-"
	}
	if row.LastErrorAt == nil {
		return sanitizeForLine(*row.LastError)
	}
	return row.LastErrorAt.UTC().Format("2006-01-02 15:04:05Z") + " " + sanitizeForLine(*row.LastError)
}
