package tui

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	hubfederation "go.kenn.io/kata/internal/federation"
)

const (
	federationViewChromeRows = 11
	federationFirstRowY      = 7
)

func renderFederation(m Model) string {
	rows := federationSpokeStatuses(m.federationStatuses)
	cursor := clampFederationCursor(m.federationCursor, rows)
	switch m.federationMode {
	case federationModeDetail:
		return renderFederationDetail(m, rows, cursor)
	case federationModeSelectLocalProject:
		return renderFederationSelectLocalProject(m)
	case federationModeSelectHub:
		return renderFederationSelectHub(m)
	case federationModeSelectHubProject:
		return renderFederationSelectHubProject(m)
	case federationModeBrowseHubs:
		return renderFederationBrowseHubs(m)
	case federationModePreview:
		return renderFederationPreview(m)
	case federationModeAdoptConfirm:
		return renderFederationAdoptConfirm(m)
	case federationModeLeavePreview:
		return renderFederationLeavePreview(m)
	case federationModeResult:
		return renderFederationResult(m)
	case federationModeRecovery:
		return renderFederationRecovery(m)
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
		subtleStyle.Render(federationSelectedProjectLine(m)),
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
		"[↑/↓ k/j] move  [enter] detail  [esc] back  [r] refresh  [n] enroll  [x] leave  [b] browse hubs  [?] help"))
	return strings.Join(body, "\n")
}

func renderFederationSelectLocalProject(m Model) string {
	rows := federationLocalProjectRows(m)
	cursor := clampFederationIndex(m.federationLocalProjectCursor, len(rows), 0)
	body := federationModeHeader(m, "Select local spoke project")
	for i, row := range rows {
		label := "create new local replica from hub project"
		if !row.createReplica {
			label = row.project.Name
		}
		body = append(body, renderFederationChoice(label, i == cursor))
	}
	body = appendFederationEnrollErr(body, m)
	body = append(body, "", subtleStyle.Render("[↑/↓ k/j] move  [enter] select  [esc] back"))
	return strings.Join(body, "\n")
}

func renderFederationSelectHub(m Model) string {
	rows := federationHubRows(m)
	cursor := clampFederationIndex(m.federationHubCursor, len(rows), 0)
	body := federationModeHeader(m, "Select hub daemon")
	if m.federationDraft.SpokeProjectName != "" {
		body = append(body, "local spoke project: "+sanitizeForLine(m.federationDraft.SpokeProjectName))
	}
	if m.federationDraft.CreateReplica {
		body = append(body, "local spoke project: create after selecting hub project")
	}
	body = append(body, "")
	for i, row := range rows {
		label := daemonName(row.target) + " " + federationDaemonEndpoint(row.target) +
			" auth " + daemonAuth(row.target)
		if row.target.AllowInsecure {
			label += " allow_insecure"
		}
		if daemonTargetsMatch(row.target, m.activeDaemon) {
			label += " (active spoke)"
		}
		body = append(body, renderFederationChoice(label, i == cursor))
	}
	body = appendFederationEnrollErr(body, m)
	body = append(body, "", subtleStyle.Render("[↑/↓ k/j] move  [enter] select  [esc] back"))
	return strings.Join(body, "\n")
}

func renderFederationSelectHubProject(m Model) string {
	body := federationModeHeader(m, "Select hub project")
	body = append(body,
		"hub daemon: "+sanitizeForLine(daemonName(m.federationDraft.HubTarget))+
			" "+sanitizeForLine(federationDaemonEndpoint(m.federationDraft.HubTarget)),
		"hub auth: "+sanitizeForLine(federationAuthDisplay(
			m.federationDraft.HubTarget,
			m.federationDraft.HubInstance.Auth,
		)),
		fmt.Sprintf("allow_insecure: %t", m.federationDraft.HubTarget.AllowInsecure),
		"",
	)
	if m.federationHubProjectsLoading {
		body = append(body, subtleStyle.Render("  loading hub projects..."))
	} else {
		rows := federationHubProjectLabels(m)
		cursor := clampFederationIndex(m.federationHubProjectCursor, len(rows), 0)
		for i, label := range rows {
			body = append(body, renderFederationChoice(label, i == cursor))
		}
	}
	body = appendFederationEnrollErr(body, m)
	body = append(body, "", subtleStyle.Render("[↑/↓ k/j] move  [enter] preview  [esc] back"))
	return strings.Join(body, "\n")
}

func renderFederationBrowseHubs(m Model) string {
	target := m.federationDraft.HubTarget
	body := federationModeHeader(m, "Browse catalog hub projects")
	body = append(body,
		"catalog hub: "+sanitizeForLine(daemonName(target))+
			" "+sanitizeForLine(federationDaemonEndpoint(target)),
		"hub auth: "+sanitizeForLine(federationAuthDisplay(target, m.federationDraft.HubInstance.Auth)),
		fmt.Sprintf("allow_insecure: %t", target.AllowInsecure),
		"mode: read-only",
		"",
	)
	if m.federationHubProjectsLoading {
		body = append(body, subtleStyle.Render("  loading hub projects..."))
	} else if m.federationEnrollErr != nil {
		body = append(body, errorStyle.Render("  failed to load hub projects: "+
			sanitizeForLine(m.federationEnrollErr.Error())))
	} else if len(m.federationHubProjects) == 0 {
		body = append(body, subtleStyle.Render("  no hub projects"))
	} else {
		cursor := clampFederationIndex(m.federationHubProjectCursor, len(m.federationHubProjects), 0)
		for i, project := range m.federationHubProjects {
			body = append(body, renderFederationChoice(federationBrowseHubProjectLabel(project), i == cursor))
		}
	}
	body = append(body, "", subtleStyle.Render("[↑/↓ k/j] move  [esc] back"))
	return strings.Join(body, "\n")
}

func renderFederationPreview(m Model) string {
	draft := m.federationDraft
	body := federationModeHeader(m, "Enrollment Preview")
	body = append(body,
		"Operation: "+federationOperationLabel(draft.Operation),
		"local spoke project: "+sanitizeForLine(emptyDash(draft.SpokeProjectName)),
		"hub daemon: "+sanitizeForLine(daemonName(draft.HubTarget))+
			" "+sanitizeForLine(federationDaemonEndpoint(draft.HubTarget)),
		"hub auth: "+sanitizeForLine(federationAuthDisplay(draft.HubTarget, draft.HubInstance.Auth)),
		"hub project: "+sanitizeForLine(federationHubProjectBehavior(draft)),
		"requested actor: "+sanitizeForLine(emptyDash(draft.RequestedActor)),
		"capabilities: "+sanitizeForLine(draft.DisplayCapabilities),
		fmt.Sprintf("push enabled: %t", draft.PushEnabled),
		fmt.Sprintf("allow_insecure: %t", draft.AllowInsecure),
	)
	if draft.AdoptExisting {
		body = append(body,
			"",
			fmt.Sprintf("this federates local project %q INTO hub project %q",
				sanitizeForLine(emptyDash(draft.SpokeProjectName)),
				sanitizeForLine(emptyDash(draft.HubProjectName))),
			"adoption warning: pre-adoption event history is replaced by snapshot events for federation",
		)
	}
	if draft.Operation == federationOperationRejoin {
		body = append(body,
			"",
			fmt.Sprintf("local project %q previously left this federation and still shares the hub project's identity;",
				sanitizeForLine(draft.SpokeProjectName)),
			fmt.Sprintf("rejoining resumes syncing with hub project %q (edits made while standalone sync to the hub)",
				sanitizeForLine(draft.HubProjectName)),
		)
		if draft.SpokeProjectName != draft.HubProjectName {
			body = append(body,
				"the local project keeps its name; spoke and hub project names do not need to match",
			)
		}
	}
	if draft.BlockedReason != "" {
		body = append(body, "", errorStyle.Render("Blocked: "+sanitizeForLine(draft.BlockedReason)))
	}
	body = appendFederationEnrollErr(body, m)
	body = append(body, "", subtleStyle.Render("[enter] confirm  [esc] back"))
	return strings.Join(body, "\n")
}

// renderFederationAdoptConfirm is the typed-confirmation gate for adoption:
// the consequence is stated in full and the operator must type the local
// project's name before Enter executes.
func renderFederationAdoptConfirm(m Model) string {
	draft := m.federationDraft
	body := federationModeHeader(m, "Confirm Adoption")
	body = append(body,
		fmt.Sprintf("federate local project %q INTO hub project %q?",
			sanitizeForLine(emptyDash(draft.SpokeProjectName)),
			sanitizeForLine(emptyDash(draft.HubProjectName))),
		fmt.Sprintf("%q's issues will be adopted into the hub project and its local event history rewritten as snapshots",
			sanitizeForLine(emptyDash(draft.SpokeProjectName))),
		"",
		fmt.Sprintf("type %q to confirm: %s",
			sanitizeForLine(draft.SpokeProjectName),
			sanitizeForLine(m.federationAdoptConfirmInput)),
	)
	body = appendFederationEnrollErr(body, m)
	body = append(body, "", subtleStyle.Render("[enter] confirm  [esc] back"))
	return strings.Join(body, "\n")
}

func renderFederationLeavePreview(m Model) string {
	draft := m.federationLeaveDraft
	disposition := draft.Disposition
	if disposition == "" {
		disposition = "detach"
	}
	body := federationModeHeader(m, "Leave Federation")
	body = append(body,
		"local spoke project: "+sanitizeForLine(emptyDash(draft.ProjectName)),
		"hub: "+sanitizeForLine(federationHubDisplay(draft.HubURL)),
		fmt.Sprintf("hub project ID: %d", draft.HubProjectID),
		"local project: "+federationLeaveDispositionLabel(disposition),
		fmt.Sprintf("local-only: %t", draft.LocalOnly),
		"",
	)
	if draft.LocalOnly {
		body = append(body,
			errorStyle.Render("local-only: skipping hub revoke; the enrollment token stays valid until manually revoked"),
			"local teardown only: "+federationLeaveTeardownLabel(disposition),
		)
	} else {
		body = append(body,
			"will revoke the enrollment(s) on the hub, then "+federationLeaveTeardownLabel(disposition),
		)
	}
	if draft.BlockedReason != "" {
		body = append(body, "", errorStyle.Render("Blocked: "+sanitizeForLine(draft.BlockedReason)))
	}
	body = appendFederationEnrollErr(body, m)
	body = append(body, "",
		subtleStyle.Render("[enter] confirm  [d] keep / archive  [l] local-only  [esc] back"))
	return strings.Join(body, "\n")
}

func federationLeaveDispositionLabel(disposition string) string {
	if disposition == "archive" {
		return "archive — remove the replica (reversible via kata projects restore)"
	}
	return "keep (detach) — standalone with all issues"
}

func federationLeaveTeardownLabel(disposition string) string {
	if disposition == "archive" {
		return "archive the local replica"
	}
	return "detach locally (keep the project standalone)"
}

func renderFederationResult(m Model) string {
	if m.federationResultIsLeave {
		return renderFederationLeaveResult(m)
	}
	result := m.federationResult
	body := federationModeHeader(m, "Enrollment Result")
	status := "joined"
	if result.Replica.Adopted {
		status = "adopted"
	}
	body = append(body,
		"status: "+status,
		"actor: "+sanitizeForLine(emptyDash(result.Enrollment.Actor)),
		fmt.Sprintf("snapshot count: %d", result.Replica.AdoptionSnapshotCount),
		"hub URL: "+sanitizeForLine(result.HubURL),
		fmt.Sprintf("hub project ID: %d", result.Metadata.ProjectID),
		"hub project UID: "+sanitizeForLine(emptyDash(result.Metadata.ProjectUID)),
		"",
		subtleStyle.Render("[enter] list  [esc] list"),
	)
	return strings.Join(body, "\n")
}

func renderFederationLeaveResult(m Model) string {
	result := m.federationLeaveResult
	body := federationModeHeader(m, "Leave Result")
	status := "detached"
	if result.Body.Archived {
		status = "archived"
	}
	revoke := fmt.Sprintf("revoked %d enrollment(s) on the hub", result.RevokedCount)
	if result.SkippedRevoke {
		revoke = "hub revoke skipped (local-only): the enrollment token remains valid until manually revoked"
	}
	body = append(body,
		"status: "+status,
		"local spoke project: "+sanitizeForLine(emptyDash(result.Draft.ProjectName)),
		"hub: "+sanitizeForLine(federationHubDisplay(result.Draft.HubURL)),
		revoke,
	)
	if len(result.GlobalEnrollmentIDs) > 0 {
		body = append(body, errorStyle.Render(fmt.Sprintf(
			"warning: global enrollment(s) %s for this spoke remain active and still authorize this project; revoke manually if intended",
			formatEnrollmentIDs(result.GlobalEnrollmentIDs))))
	}
	body = append(body,
		"",
		subtleStyle.Render("[enter] list  [esc] list"),
	)
	return strings.Join(body, "\n")
}

func formatEnrollmentIDs(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf("#%d", id))
	}
	return strings.Join(parts, ", ")
}

func renderFederationRecovery(m Model) string {
	recovery := m.federationRecovery
	body := federationModeHeader(m, "Enrollment Recovery")
	if recovery.Stage == "metadata" {
		body = append(body, fmt.Sprintf("hub %s: enrollment metadata fetch failed", sanitizeForLine(recovery.HubName)))
	} else {
		body = append(body, "hub: enrollment created", "spoke: join failed")
	}
	if recovery.Err != nil {
		body = append(body, errorStyle.Render(sanitizeForLine(recovery.Err.Error())))
	}
	body = append(body, "token: hidden")
	// The token guess is only a plausible explanation for auth failures (or
	// when no specific error was captured); blaming the token for a 409 sends
	// the operator chasing the wrong problem.
	var apiErr *APIError
	if recovery.Err == nil || (errors.As(recovery.Err, &apiErr) &&
		(apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden)) {
		body = append(body, "the hub enrollment may be single-use, expired, revoked, or invalidated")
	}
	body = append(body,
		"",
		subtleStyle.Render("[R] reveal recovery command  [esc] back"),
	)
	if recovery.Reveal {
		body = append(body,
			"",
			errorStyle.Render("single-use/secret-bearing recovery command"),
			"works only while the hub enrollment remains valid and not revoked",
			"spoke target: "+sanitizeForLine(recovery.SpokeName)+" "+sanitizeForLine(recovery.SpokeEndpoint),
			federationRecoveryCommandString(recovery.Command),
		)
	}
	return strings.Join(body, "\n")
}

func federationModeHeader(m Model, title string) []string {
	return []string{
		titleStyle.Render("kata / federation"),
		subtleStyle.Render(federationHeaderLine(m)),
		subtleStyle.Render(federationSelectedProjectLine(m)),
		"",
		titleStyle.Render(title),
		"",
	}
}

func renderFederationChoice(label string, highlight bool) string {
	prefix := "  "
	if highlight {
		prefix = "▶ "
	}
	line := prefix + sanitizeForLine(label)
	if highlight {
		line = lipgloss.NewStyle().Bold(true).Render(line)
	}
	return line
}

func appendFederationEnrollErr(body []string, m Model) []string {
	if m.federationEnrollErr == nil {
		return body
	}
	return append(body, "", errorStyle.Render(sanitizeForLine(m.federationEnrollErr.Error())))
}

func federationHubProjectLabels(m Model) []string {
	labels := []string{}
	if !m.federationDraft.CreateReplica {
		labels = append(labels, federationDefaultHubProjectLabel(m.federationDraft, m.federationHubProjects))
	}
	for _, project := range federationSelectableHubProjects(m) {
		labels = append(labels, project.Name)
	}
	if len(labels) == 0 {
		return []string{"no hub projects"}
	}
	return labels
}

func federationDefaultHubProjectLabel(draft federationDraft, hubProjects []ProjectSummary) string {
	if _, ok := hubProjectByName(hubProjects, draft.SpokeProjectName); ok {
		return fmt.Sprintf("use existing hub project %q; enable federation if needed", draft.SpokeProjectName)
	}
	return fmt.Sprintf("create hub project %q and enable federation", draft.SpokeProjectName)
}

func federationBrowseHubProjectLabel(project ProjectSummary) string {
	return fmt.Sprintf("%d %s", project.ID, project.Name)
}

func federationOperationLabel(operation federationOperation) string {
	switch operation {
	case federationOperationAdoptSameName:
		return "adopt existing local project"
	case federationOperationAdoptSelectedHub:
		return "adopt existing local project into selected hub project"
	case federationOperationCreateReplica:
		return "create new local replica from hub project"
	case federationOperationRejoin:
		return "rejoin: resume syncing a local project that previously left this federation"
	default:
		return "-"
	}
}

func federationHubProjectBehavior(draft federationDraft) string {
	if draft.Operation == federationOperationAdoptSameName {
		if draft.HubProjectID != 0 {
			return fmt.Sprintf("use existing hub project %q; enable federation if needed", draft.HubProjectName)
		}
		return fmt.Sprintf("create hub project %q and enable federation", draft.SpokeProjectName)
	}
	if draft.HubProjectName != "" {
		return draft.HubProjectName
	}
	return "-"
}

func federationRecoveryCommandString(cmd federationRecoveryCommand) string {
	parts := []string{}
	if cmd.SpokeEndpoint != "" && cmd.SpokeEndpoint != "local" {
		parts = append(parts, "KATA_SERVER="+shellWord(cmd.SpokeEndpoint))
		if cmd.SpokeAllowInsecure {
			parts = append(parts, "KATA_ALLOW_INSECURE=1")
		}
		if cmd.SpokeToken != "" {
			parts = append(parts, "KATA_AUTH_TOKEN="+shellWord(cmd.SpokeToken))
		}
	}
	parts = append(parts,
		"kata",
		"federation", "join",
		"--hub-url", shellWord(cmd.HubURL),
		"--hub-project-id", fmt.Sprintf("%d", cmd.HubProjectID),
		"--project", shellWord(cmd.ProjectName),
		"--token", shellWord(cmd.Token),
		"--actor", shellWord(cmd.Actor),
		"--capabilities", shellWord(cmd.Capabilities),
	)
	if cmd.HubProjectUID != "" {
		parts = append(parts, "--hub-project-uid", shellWord(cmd.HubProjectUID))
	}
	if cmd.ReplayHorizonEventID != 0 {
		parts = append(parts, "--replay-horizon", fmt.Sprintf("%d", cmd.ReplayHorizonEventID))
	}
	if cmd.BaselineThroughEventID != 0 {
		parts = append(parts, "--baseline-through", fmt.Sprintf("%d", cmd.BaselineThroughEventID))
	}
	if cmd.PushEnabled {
		parts = append(parts, "--push")
	}
	if cmd.AllowInsecure {
		parts = append(parts, "--allow-insecure")
	}
	if cmd.AdoptExisting {
		parts = append(parts, "--adopt-existing")
	}
	return strings.Join(parts, " ")
}

func shellWord(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			strings.ContainsRune("-_./:=,", r) {
			continue
		}
		return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
	}
	return s
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
		sanitizeForLine(federationAuthDisplay(m.activeDaemon, m.federationInstance.Auth)),
	)
}

func federationSelectedProjectLine(m Model) string {
	return "selected project: " + sanitizeForLine(federationSelectedProjectDisplay(m))
}

func federationSelectedProjectDisplay(m Model) string {
	if m.federationDraft.Operation == federationOperationRejoin {
		return m.federationDraft.SpokeProjectName + " (rejoin)"
	}
	if m.federationDraft.CreateReplica {
		if m.federationDraft.SpokeProjectName != "" {
			return m.federationDraft.SpokeProjectName + " (new local replica)"
		}
		return "create new local replica from hub project"
	}
	if m.federationDraft.SpokeProjectName != "" {
		return m.federationDraft.SpokeProjectName
	}
	if m.federationSelectedProjectSet {
		if m.federationSelectedProjectName != "" {
			return m.federationSelectedProjectName
		}
		return "none"
	}
	if _, projectName, ok := m.currentFederationProject(); ok {
		return projectName
	}
	return "none"
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

func federationAuthDisplay(target daemonTarget, auth AuthInfo) string {
	base := federationAuthBase(target, auth)
	actor := strings.TrimSpace(auth.Actor)
	if actor != "" {
		return base + " actor " + actor
	}
	kind := strings.TrimSpace(auth.Kind)
	if kind == "trusted_proxy_absent" {
		return "trusted-proxy missing actor"
	}
	if base != "token" || kind == "" || kind == "none" {
		return base
	}
	switch kind {
	case "static_token":
		return base + " static"
	case "bootstrap":
		return base + " bootstrap"
	default:
		return base + " " + kind
	}
}

func federationAuthBase(target daemonTarget, auth AuthInfo) string {
	kind := strings.TrimSpace(auth.Kind)
	switch kind {
	case "db_token", "static_token", "bootstrap":
		return "token"
	case "trusted_proxy", "trusted_proxy_absent":
		return "trusted-proxy"
	default:
		return daemonAuth(target)
	}
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
		subtleStyle.Render(federationSelectedProjectLine(m)),
		"",
	}
	if cursor < 0 || cursor >= len(rows) {
		body = append(body, subtleStyle.Render("no federation selected"))
		return strings.Join(fitFederationLines(body, m.height), "\n")
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
	)
	for _, quarantine := range row.ActiveQuarantines {
		body = append(body, fmt.Sprintf(
			"quarantine #%d: %s events %d-%d at %s: %s",
			quarantine.ID,
			sanitizeForLine(quarantine.Direction),
			quarantine.FirstEventID,
			quarantine.LastEventID,
			quarantine.CreatedAt.UTC().Format("2006-01-02 15:04:05Z"),
			sanitizeForLine(quarantine.Error),
		))
	}
	body = append(body,
		"reset blocker: "+sanitizeForLine(emptyDash(row.ResetBlocker)),
		fmt.Sprintf("claim violations: %d unresolved, %d recent", row.UnresolvedViolationCount, row.RecentViolationCount),
		"last pull success: "+formatOptionalTime(row.LastPullSuccessAt),
		"last push success: "+formatOptionalTime(row.LastPushSuccessAt),
		"last sync success: "+formatOptionalTime(row.LastSuccessfulSyncAt),
		"last error: "+formatOptionalError(row),
		"",
		subtleStyle.Render("[esc] back  [r] refresh  [q] quit  [?] help"),
	)
	return strings.Join(fitFederationLines(body, m.height), "\n")
}

func fitFederationLines(lines []string, height int) []string {
	if height <= 0 || len(lines) <= height {
		return lines
	}
	if height == 1 {
		return []string{lines[len(lines)-1]}
	}
	out := make([]string, 0, height)
	out = append(out, lines[:height-2]...)
	out = append(out, subtleStyle.Render("..."))
	out = append(out, lines[len(lines)-1])
	return out
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
