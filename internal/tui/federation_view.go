package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	clientpkg "go.kenn.io/kata/internal/client"
	hubfederation "go.kenn.io/kata/internal/federation"
)

type federationMode int

const (
	federationModeList federationMode = iota
	federationModePreview
	federationModeResult
	federationModeRecovery
	federationModeDetail
	federationModeSelectLocalProject
	federationModeSelectHub
	federationModeSelectHubProject
	federationModeBrowseHubs
	federationModeLeavePreview
	// federationModeAdoptConfirm is the typed-confirmation gate between the
	// enroll preview and execution for adoption operations, which rewrite the
	// local project's event history.
	federationModeAdoptConfirm
)

type federationOperation string

const (
	federationOperationAdoptSameName    federationOperation = "adopt-same-name"
	federationOperationAdoptSelectedHub federationOperation = "adopt-selected-hub"
	federationOperationCreateReplica    federationOperation = "create-replica"
	// federationOperationRejoin rebinds a local project that previously left
	// this hub project's federation (it still shares the hub project's UID).
	federationOperationRejoin federationOperation = "rejoin"
)

type federationDraft struct {
	Operation            federationOperation
	SpokeProjectID       int64
	SpokeProjectName     string
	CreateReplica        bool
	SelectedLocalProject bool
	HubTarget            daemonTarget
	HubInstance          InstanceInfo
	HubProjectID         int64
	HubProjectName       string
	RequestedActor       string
	APICapabilities      string
	DisplayCapabilities  string
	PushEnabled          bool
	AllowInsecure        bool
	AdoptExisting        bool
	BlockedReason        string
}

type federationEnrollResult struct {
	Draft      federationDraft
	HubURL     string
	Enrollment FederationEnrollment
	Metadata   ProjectFederationMetadata
	Replica    FederationReplicaResult
	Recovery   federationRecovery
}

// federationLeaveDraft captures one spoke row's leave intent. It is the
// mutation boundary: populated when `x` is pressed on a spoke row and
// rendered in federationModeLeavePreview before any hub revoke or local
// teardown runs. Disposition is "detach" (default) or "archive". LocalOnly
// skips the hub revoke (leaving the enrollment token valid). BlockedReason is
// set when the selected row cannot be left (e.g. not a spoke).
type federationLeaveDraft struct {
	ProjectID     int64
	ProjectName   string
	HubURL        string
	HubProjectID  int64
	InstanceUID   string
	AllowInsecure bool
	Disposition   string
	Actor         string
	LocalOnly     bool
	BlockedReason string
}

// federationLeaveResult is the outcome surfaced on the result screen after a
// successful leave.
type federationLeaveResult struct {
	Draft         federationLeaveDraft
	RevokedCount  int
	SkippedRevoke bool
	// GlobalEnrollmentIDs are active hub enrollments with global (nil) project
	// scope for this spoke: they still authorize the left project but are not
	// auto-revoked, since they may serve other projects.
	GlobalEnrollmentIDs []int64
	Body                LeaveFederationReplicaResult
}

// federationOpKind names the mutation flow an in-flight or completed
// federation operation belongs to. Enroll and leave are one lifecycle —
// confirm, dispatch, guard the reply against a staleness counter, land on
// the shared result screen — so they share one operation record instead of
// two parallel attempt/running/result triples plus a bool discriminator.
type federationOpKind int

const (
	federationOpNone federationOpKind = iota
	federationOpEnroll
	federationOpLeave
)

// federationOp is the single in-flight-or-last federation mutation.
//
// attempt is the staleness counter both flows now share: starting either
// flow bumps it, so a reply from the flow that was superseded no longer
// matches and is dropped. err is deliberately flow-agnostic — the browse
// flow writes it too, which is what the old enroll-specific error name hid.
// enroll is valid when kind == federationOpEnroll, leave when
// kind == federationOpLeave.
type federationOp struct {
	kind    federationOpKind
	attempt uint64
	running bool
	err     error
	enroll  federationEnrollResult
	leave   federationLeaveResult
}

// federationState is every piece of Model state the federation views own.
// Grouping matters for one specific reason: installDaemonConnection has to
// drop all of it on a daemon switch, and a hand-written list of assignments
// cannot be verified by construction — it silently missed six fields. One
// zero-value assignment makes a forgotten field unrepresentable rather than
// merely absent.
type federationState struct {
	instance            InstanceInfo
	statuses            []FederationProjectStatus
	cursor              int
	loading             bool
	err                 error
	gen                 uint64
	selectedProjectSet  bool
	selectedProjectID   int64
	selectedProjectName string
	mode                federationMode
	draft               federationDraft
	localProjectCursor  int
	hubCursor           int
	hubProjectCursor    int
	hubProjects         []ProjectSummary
	hubProjectsLoading  bool
	enrollGen           uint64
	adoptConfirmInput   string
	recovery            federationRecovery
	leaveDraft          federationLeaveDraft
	op                  federationOp
}

// federationOpOutcome is the flow-independent shape of a completed
// operation, so one handler can serve both result messages.
type federationOpOutcome struct {
	kind    federationOpKind
	connGen uint64
	attempt uint64
	enroll  federationEnrollResult
	leave   federationLeaveResult
	err     error
}

// federationOpPreviewMode is the screen a failed operation returns to: its
// own preview, where the operator can adjust and retry.
func federationOpPreviewMode(kind federationOpKind) federationMode {
	if kind == federationOpLeave {
		return federationModeLeavePreview
	}
	return federationModePreview
}

// invalidateFederationOp abandons any dispatched or completed operation while
// retaining a monotonically increasing staleness counter. A later operation
// therefore cannot reuse the attempt number of a reply that is still in
// flight, and clearing kind prevents that reply from owning the next screen.
func (m Model) invalidateFederationOp() Model {
	m.federation.op = federationOp{attempt: m.federation.op.attempt + 1}
	return m
}

type federationRecovery struct {
	HubName       string
	SpokeName     string
	SpokeEndpoint string
	Stage         string
	Token         string
	Reveal        bool
	Command       federationRecoveryCommand
	Err           error
}

type federationRecoveryCommand struct {
	HubURL                 string
	HubProjectID           int64
	HubProjectUID          string
	ProjectName            string
	ReplayHorizonEventID   int64
	BaselineThroughEventID int64
	Token                  string
	Actor                  string
	Capabilities           string
	PushEnabled            bool
	AllowInsecure          bool
	AdoptExisting          bool
	SpokeName              string
	SpokeEndpoint          string
	SpokeAllowInsecure     bool
	SpokeToken             string
}

var (
	newFederationHubAdminClient = func(
		ctx context.Context,
		target daemonTarget,
	) (federationHubAdminAPI, daemonTarget, error) {
		return newHubAdminClient(ctx, target)
	}
	newFederationEnrollmentClient = newHubEnrollmentClient
)

func (m Model) transitionToFederation() (Model, tea.Cmd) {
	m = m.invalidateFederationOp()
	m = m.captureFederationSelectedProject()
	m.prevView = m.view
	m.view = viewFederation
	m.federation.mode = federationModeList
	m.federation.draft = federationDraft{}
	m.federation.loading = true
	m.federation.err = nil
	m.federation.gen++
	return m, m.fetchFederationStatus()
}

func (m Model) fetchFederationStatus() tea.Cmd {
	api := m.api
	connGen := m.connGen
	gen := m.federation.gen
	return func() tea.Msg {
		if api == nil {
			return federationLoadedMsg{connGen: connGen, gen: gen, err: errors.New("daemon client unavailable")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		instance, err := api.GetInstance(ctx)
		if err != nil {
			return federationLoadedMsg{connGen: connGen, gen: gen, err: err}
		}
		status, err := api.FederationStatus(ctx)
		return federationLoadedMsg{
			connGen:  connGen,
			gen:      gen,
			instance: instance,
			status:   status,
			err:      err,
		}
	}
}

func (m Model) handleFederationLoaded(msg federationLoadedMsg) Model {
	if m.staleConnMsg(msg.connGen) || msg.gen != m.federation.gen {
		return m
	}
	m.federation.loading = false
	m.federation.err = msg.err
	if msg.err != nil {
		return m
	}
	m.federation.instance = msg.instance
	m.federation.statuses = msg.status.Statuses
	m.federation.cursor = clampFederationCursor(m.federation.cursor, federationSpokeStatuses(m.federation.statuses))
	return m
}

func (m Model) handleFederationHubProjectsLoaded(msg federationHubProjectsLoadedMsg) Model {
	if m.staleConnMsg(msg.connGen) || msg.gen != m.federation.enrollGen {
		return m
	}
	if m.federation.mode != federationModeSelectHubProject && m.federation.mode != federationModeBrowseHubs {
		return m
	}
	m.federation.hubProjectsLoading = false
	m.federation.op.err = msg.err
	if msg.err != nil {
		return m
	}
	m.federation.draft.HubTarget = msg.target
	m.federation.draft.HubInstance = msg.instance
	if actor := strings.TrimSpace(msg.instance.Auth.Actor); actor != "" {
		m.federation.draft.RequestedActor = actor
	}
	m.federation.hubProjects = msg.projects
	m.federation.hubProjectCursor = clampFederationIndex(
		m.federation.hubProjectCursor, len(federationHubProjectRowsForMode(m)), 0)
	return m
}

// handleFederationOpResult lands the reply from either mutation flow. The
// guards are: not from a superseded daemon connection, matching the shared
// attempt counter, and belonging to the operation that is actually current.
//
// The enroll flow has one branch of its own: a failure that produced a
// recovery payload goes to the recovery screen (which shows the operator the
// manual `kata federation join` command) instead of the preview.
func (m Model) handleFederationOpResult(out federationOpOutcome) (Model, tea.Cmd) {
	if m.staleConnMsg(out.connGen) ||
		out.attempt != m.federation.op.attempt ||
		out.kind != m.federation.op.kind {
		return m, nil
	}
	m.federation.op.running = false
	if out.err != nil {
		if out.kind == federationOpEnroll &&
			(out.enroll.Recovery.Token != "" || out.enroll.Recovery.Stage != "") {
			m.federation.recovery = out.enroll.Recovery
			m.federation.recovery.Err = out.err
			m.federation.mode = federationModeRecovery
			return m, nil
		}
		m.federation.op.err = out.err
		m.federation.mode = federationOpPreviewMode(out.kind)
		return m, nil
	}
	m.federation.op.enroll = out.enroll
	m.federation.op.leave = out.leave
	m.federation.mode = federationModeResult
	m.federation.loading = true
	m.federation.err = nil
	m.federation.gen++
	return m, m.fetchFederationStatus()
}

func (m Model) handleFederationEnrollResult(msg federationEnrollResultMsg) (Model, tea.Cmd) {
	return m.handleFederationOpResult(federationOpOutcome{
		kind:    federationOpEnroll,
		connGen: msg.connGen,
		attempt: msg.attempt,
		enroll:  msg.result,
		err:     msg.err,
	})
}

func (m Model) routeFederationViewKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	rows := federationSpokeStatuses(m.federation.statuses)
	switch m.federation.mode {
	case federationModeDetail:
		return m.routeFederationDetailKey(msg)
	case federationModeSelectLocalProject:
		return m.routeFederationLocalProjectKey(msg)
	case federationModeSelectHub:
		return m.routeFederationHubKey(msg)
	case federationModeSelectHubProject:
		return m.routeFederationHubProjectKey(msg)
	case federationModeBrowseHubs:
		return m.routeFederationBrowseHubsKey(msg)
	case federationModePreview:
		return m.routeFederationPreviewKey(msg)
	case federationModeAdoptConfirm:
		return m.routeFederationAdoptConfirmKey(msg)
	case federationModeLeavePreview:
		return m.routeFederationLeavePreviewKey(msg)
	case federationModeRecovery:
		return m.routeFederationRecoveryKey(msg)
	case federationModeResult:
		return m.routeFederationResultKey(msg)
	}
	if next, ok := m.cursorMoveFederation(msg, rows); ok {
		return next, nil
	}
	switch msg.String() {
	case "esc":
		return m.escFromFederationView()
	case "n":
		return m.startFederationEnrollment()
	case "r":
		m.federation.loading = true
		m.federation.err = nil
		m.federation.gen++
		return m, m.fetchFederationStatus()
	case "b":
		return m.startFederationHubBrowse()
	case "x":
		if m.federation.cursor < 0 || m.federation.cursor >= len(rows) {
			return m, nil
		}
		return m.startFederationLeave(rows[m.federation.cursor])
	case "enter":
		if m.federation.cursor < 0 || m.federation.cursor >= len(rows) {
			return m, nil
		}
		m.federation.mode = federationModeDetail
		return m, nil
	}
	return m, nil
}

func (m Model) routeFederationDetailKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace":
		m.federation.mode = federationModeList
		return m, nil
	case "r":
		m.federation.loading = true
		m.federation.err = nil
		m.federation.gen++
		return m, m.fetchFederationStatus()
	case "x":
		rows := federationSpokeStatuses(m.federation.statuses)
		cursor := clampFederationCursor(m.federation.cursor, rows)
		if cursor < 0 || cursor >= len(rows) {
			return m, nil
		}
		return m.startFederationLeave(rows[cursor])
	}
	return m, nil
}

func (m Model) routeFederationLocalProjectKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	rows := federationLocalProjectRows(m)
	if next, ok := nextFederationCursor(msg, m.federation.localProjectCursor, len(rows)); ok {
		m.federation.localProjectCursor = next
		return m, nil
	}
	switch msg.String() {
	case "esc":
		m = m.invalidateFederationOp()
		m.federation.mode = federationModeList
		return m, nil
	case "enter":
		if len(rows) == 0 {
			return m, nil
		}
		row := rows[clampFederationIndex(m.federation.localProjectCursor, len(rows), 0)]
		if row.createReplica {
			m.federation.draft.CreateReplica = true
			m.federation.draft.SelectedLocalProject = true
			m.federation.draft.AdoptExisting = false
			m.federation.draft.SpokeProjectID = 0
			m.federation.draft.SpokeProjectName = ""
			m.federation.selectedProjectSet = true
			m.federation.selectedProjectID = 0
			m.federation.selectedProjectName = ""
		} else {
			m.federation.draft.CreateReplica = false
			m.federation.draft.SelectedLocalProject = true
			m.federation.draft.AdoptExisting = true
			m.federation.draft.SpokeProjectID = row.project.ID
			m.federation.draft.SpokeProjectName = row.project.Name
			m.federation.selectedProjectSet = true
			m.federation.selectedProjectID = row.project.ID
			m.federation.selectedProjectName = row.project.Name
		}
		m.federation.mode = federationModeSelectHub
		m.federation.hubCursor = 0
		m.federation.op.err = nil
		return m, nil
	}
	return m, nil
}

func (m Model) routeFederationHubKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	rows := federationHubRows(m)
	if next, ok := nextFederationCursor(msg, m.federation.hubCursor, len(rows)); ok {
		m.federation.hubCursor = next
		return m, nil
	}
	switch msg.String() {
	case "esc":
		if m.federation.draft.SelectedLocalProject {
			m.federation.mode = federationModeSelectLocalProject
		} else {
			m = m.invalidateFederationOp()
			m.federation.mode = federationModeList
		}
		return m, nil
	case "enter":
		if len(rows) == 0 {
			return m, nil
		}
		target := rows[clampFederationIndex(m.federation.hubCursor, len(rows), 0)].target
		return m.selectFederationHub(target)
	}
	return m, nil
}

func (m Model) routeFederationHubProjectKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	count := len(federationHubProjectRows(m))
	if next, ok := nextFederationCursor(msg, m.federation.hubProjectCursor, count); ok {
		m.federation.hubProjectCursor = next
		return m, nil
	}
	switch msg.String() {
	case "esc":
		m.federation.mode = federationModeSelectHub
		return m, nil
	case "enter":
		return m.previewFederationEnrollment()
	}
	return m, nil
}

func (m Model) routeFederationBrowseHubsKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	count := len(federationBrowseHubProjectRows(m))
	if next, ok := nextFederationCursor(msg, m.federation.hubProjectCursor, count); ok {
		m.federation.hubProjectCursor = next
		return m, nil
	}
	switch msg.String() {
	case "esc", "backspace":
		m.federation.mode = federationModeList
		return m, nil
	case "enter":
		return m, nil
	}
	return m, nil
}

func (m Model) routeFederationPreviewKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace":
		m.federation.mode = federationModeSelectHubProject
		return m, nil
	case "enter":
		if m.federation.draft.BlockedReason != "" || m.federation.op.running {
			return m, nil
		}
		if m.federation.draft.AdoptExisting {
			// Adoption rewrites the local project's event history; require the
			// operator to type the project name before executing.
			m.federation.adoptConfirmInput = ""
			m.federation.op.err = nil
			m.federation.mode = federationModeAdoptConfirm
			return m, nil
		}
		m.federation.op = federationOp{
			kind:    federationOpEnroll,
			attempt: m.federation.op.attempt + 1,
			running: true,
		}
		return m, m.executeFederationEnrollment(m.federation.op.attempt)
	}
	return m, nil
}

// routeFederationAdoptConfirmKey is a single-field typed confirmation: the
// operator must type the local project's name exactly before an adoption
// executes. Esc returns to the preview.
func (m Model) routeFederationAdoptConfirmKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.federation.adoptConfirmInput = ""
		m.federation.op.err = nil
		m.federation.mode = federationModePreview
		return m, nil
	case "enter":
		if m.federation.op.running {
			return m, nil
		}
		if m.federation.adoptConfirmInput != m.federation.draft.SpokeProjectName {
			m.federation.op.err = fmt.Errorf("type %q to confirm adoption", m.federation.draft.SpokeProjectName)
			return m, nil
		}
		m.federation.adoptConfirmInput = ""
		m.federation.op = federationOp{
			kind:    federationOpEnroll,
			attempt: m.federation.op.attempt + 1,
			running: true,
		}
		return m, m.executeFederationEnrollment(m.federation.op.attempt)
	case "backspace":
		if r := []rune(m.federation.adoptConfirmInput); len(r) > 0 {
			m.federation.adoptConfirmInput = string(r[:len(r)-1])
		}
		return m, nil
	}
	// Printable input (including space) arrives with Text populated in
	// Bubble Tea v2; anything else (arrows, function keys) leaves Text
	// empty and falls through as a no-op.
	if msg.Text != "" {
		m.federation.adoptConfirmInput += msg.Text
		return m, nil
	}
	return m, nil
}

func (m Model) routeFederationRecoveryKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "R":
		m.federation.recovery.Reveal = true
		return m, nil
	case "esc":
		m.federation.mode = federationModePreview
		return m, nil
	}
	return m, nil
}

func (m Model) routeFederationResultKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "enter":
		m.federation.mode = federationModeList
		return m, nil
	}
	return m, nil
}

// startFederationLeave builds the leave draft from a selected row and enters
// the preview. Leave is spoke-only: a non-spoke row is a no-op (the guard).
// The preview is the mutation boundary — nothing is torn down here.
func (m Model) startFederationLeave(row FederationProjectStatus) (Model, tea.Cmd) {
	if row.Role != "spoke" {
		return m, nil
	}
	m = m.invalidateFederationOp()
	m.federation.leaveDraft = federationLeaveDraft{
		ProjectID:    row.ProjectID,
		ProjectName:  row.ProjectName,
		HubURL:       row.HubURL,
		HubProjectID: row.HubProjectID,
		InstanceUID:  strings.TrimSpace(m.federation.instance.InstanceUID),
		// The binding's transport opt-in, so a plain-HTTP overlay hub joined
		// with allow_insecure can also be left from the TUI.
		AllowInsecure: row.AllowInsecure,
		Disposition:   "detach",
		Actor:         m.list.actor,
	}
	m.federation.mode = federationModeLeavePreview
	return m, nil
}

func (m Model) routeFederationLeavePreviewKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace":
		m = m.invalidateFederationOp()
		m.federation.mode = federationModeList
		return m, nil
	case "d":
		if m.federation.leaveDraft.Disposition == "archive" {
			m.federation.leaveDraft.Disposition = "detach"
		} else {
			m.federation.leaveDraft.Disposition = "archive"
		}
		return m, nil
	case "l":
		m.federation.leaveDraft.LocalOnly = !m.federation.leaveDraft.LocalOnly
		return m, nil
	case "enter":
		if m.federation.leaveDraft.BlockedReason != "" || m.federation.op.running {
			return m, nil
		}
		m.federation.op = federationOp{
			kind:    federationOpLeave,
			attempt: m.federation.op.attempt + 1,
			running: true,
		}
		return m, m.executeFederationLeave(m.federation.op.attempt)
	}
	return m, nil
}

func (m Model) handleFederationLeaveResult(msg federationLeaveResultMsg) (Model, tea.Cmd) {
	return m.handleFederationOpResult(federationOpOutcome{
		kind:    federationOpLeave,
		connGen: msg.connGen,
		attempt: msg.attempt,
		leave:   msg.result,
		err:     msg.err,
	})
}

func (m Model) executeFederationLeave(attempt uint64) tea.Cmd {
	connGen := m.connGen
	draft := m.federation.leaveDraft
	spoke := m.api
	hubTarget := m.federationLeaveHubTarget(draft.HubURL, draft.AllowInsecure)
	return func() tea.Msg {
		result, err := runFederationLeave(context.Background(), draft, hubTarget, spoke)
		return federationLeaveResultMsg{
			connGen: connGen,
			attempt: attempt,
			result:  result,
			err:     err,
		}
	}
}

// federationLeaveHubTarget resolves the hub admin daemonTarget for a leave by
// matching the binding's hub URL against the catalog (so its token/token_env
// is used), falling back to an unauthenticated target. The catalog entry whose
// URL matches the binding's hub_url supplies only the admin token; global daemon
// auth is never sent to a hub. The target URL is ALWAYS the binding's hub URL —
// the catalog entry never redirects the admin token to a different origin —
// and the URL comparison normalizes trailing slashes so a catalog/binding slash
// mismatch still matches (#273).
func (m Model) federationLeaveHubTarget(hubURL string, allowInsecure bool) daemonTarget {
	want := strings.TrimRight(hubURL, "/")
	for _, target := range m.daemonTargets {
		if target.Local {
			continue
		}
		if strings.TrimRight(target.URL, "/") == want {
			// Token/token_env come from the catalog entry, but pin the URL to
			// the binding's hub URL so the admin token is sent only to the
			// bound origin. allow_insecure is the UNION of the binding's
			// join-time opt-in and the same-origin catalog entry's: the
			// catalog can restore an opt-in lost with the credential during a
			// partial-leave recovery, but can never remove the binding's.
			matched := target
			matched.URL = want
			matched.resolved.BaseURL = want
			matched.resolved.AllowInsecure = allowInsecure || target.resolved.AllowInsecure
			return matched
		}
	}
	// No catalog match: an unauthenticated, non-implicit target. Implicit
	// targets could pick up the global KATA_AUTH_TOKEN/[auth].token during
	// implicit resolution, which must never be sent to the hub origin.
	return daemonTarget{
		URL: want,
		resolved: clientpkg.ResolvedDaemon{
			Source: clientpkg.DaemonSourceInjected, BaseURL: want, AllowInsecure: allowInsecure,
		},
	}
}

// runFederationLeave mirrors runFederationEnrollment: revoke-first on the hub
// (unless LocalOnly), then call the spoke's local teardown route. The hub
// revoke aborts the leave on failure so local state is never half torn down,
// matching the CLI ordering.
func runFederationLeave(
	parent context.Context,
	draft federationLeaveDraft,
	hubTarget daemonTarget,
	spoke federationSpokeAPI,
) (federationLeaveResult, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	result := federationLeaveResult{Draft: draft}
	if spoke == nil {
		return result, errors.New("spoke: leave failed: daemon client unavailable")
	}
	disposition := draft.Disposition
	if disposition == "" {
		disposition = "detach"
	}
	if !draft.LocalOnly {
		// Daemon preflight BEFORE the irreversible hub revoke, for every
		// leave that will contact the hub: the route can refuse a detach too
		// (role drift, vanished project, actor validation), and the archive
		// disposition adds the open-issue refusal. A refusal discovered only
		// after the revoke would strand the spoke locally bound with the hub
		// side gone.
		if _, err := spoke.LeaveFederationReplica(ctx, draft.ProjectID, LeaveFederationReplicaInput{
			Disposition: disposition,
			Actor:       draft.Actor,
			Preflight:   true,
		}); err != nil {
			return result, fmt.Errorf("spoke: leave preflight failed: %w", err)
		}
	}
	if draft.LocalOnly {
		result.SkippedRevoke = true
	} else {
		revoked, globals, err := revokeFederationLeaveEnrollments(ctx, draft, hubTarget)
		if err != nil {
			return result, err
		}
		result.RevokedCount = revoked
		result.GlobalEnrollmentIDs = globals
	}
	body, err := spoke.LeaveFederationReplica(ctx, draft.ProjectID, LeaveFederationReplicaInput{
		Disposition: disposition,
		Actor:       draft.Actor,
	})
	if err != nil {
		return result, fmt.Errorf("spoke: leave failed: %w", err)
	}
	result.Body = body
	return result, nil
}

// revokeFederationLeaveEnrollments lists the hub's enrollments and revokes
// every active project-scoped one bound to this spoke instance + hub project.
// Zero matches is success (already revoked). Matching GLOBAL enrollments
// (project_id NULL) are returned, not revoked — they may authorize other
// projects on the hub — so the result screen can warn that they still
// authorize this project. A hub transport/auth failure aborts the leave
// before local teardown, with guidance to retry with local-only.
func revokeFederationLeaveEnrollments(
	ctx context.Context,
	draft federationLeaveDraft,
	hubTarget daemonTarget,
) (int, []int64, error) {
	if strings.TrimSpace(draft.InstanceUID) == "" {
		return 0, nil, errors.New("spoke instance UID is not loaded; refresh federation status before leaving")
	}
	hub, _, err := newFederationHubAdminClient(ctx, hubTarget)
	if err != nil {
		return 0, nil, federationLeaveHubError(err)
	}
	enrollments, err := hub.ListFederationEnrollments(ctx)
	if err != nil {
		return 0, nil, federationLeaveHubError(err)
	}
	ids, globals, foreign := matchFederationLeaveEnrollments(enrollments, draft.InstanceUID, draft.HubProjectID)
	if len(ids) == 0 && len(foreign) > 0 {
		return 0, nil, fmt.Errorf(
			"no active enrollment matches this spoke's instance UID, but enrollment(s) %s still authorize the hub project — the instance UID can change after a clone/import, or the enrollment may belong to another spoke instance; revoke the right one with `kata federation revoke <id>` on the hub, or retry with local-only to tear down locally without revoking",
			formatEnrollmentIDs(foreign))
	}
	for _, id := range ids {
		if err := hub.RevokeFederationEnrollment(ctx, id); err != nil {
			return 0, nil, federationLeaveHubError(err)
		}
	}
	return len(ids), globals, nil
}

// matchFederationLeaveEnrollments returns the IDs of active (not-revoked)
// enrollments whose spoke instance UID and hub project ID match this spoke's
// binding, the IDs of active GLOBAL enrollments (nil project scope) for the
// same spoke — those still authorize this project but are not auto-revoked
// because they may serve the spoke's other projects on the hub — and the IDs
// of active project-scoped enrollments for OTHER spoke instances. The caller
// must not treat zero matches as success while foreign ones exist: the
// instance UID can drift from the enrollment's (clone/import refresh or an
// explicit --spoke-instance enroll), and silently proceeding would strand a
// live token. Mirrors the CLI's revokeSpokeEnrollmentsOnHub selection.
func matchFederationLeaveEnrollments(
	enrollments []FederationEnrollment,
	instanceUID string,
	hubProjectID int64,
) (ids, globals, foreign []int64) {
	for _, enrollment := range enrollments {
		if enrollment.RevokedAt != nil {
			continue
		}
		if enrollment.ProjectID == nil {
			if enrollment.SpokeInstanceUID == instanceUID {
				globals = append(globals, enrollment.ID)
			}
			continue
		}
		if *enrollment.ProjectID != hubProjectID {
			continue
		}
		if enrollment.SpokeInstanceUID == instanceUID {
			ids = append(ids, enrollment.ID)
			continue
		}
		foreign = append(foreign, enrollment.ID)
	}
	return ids, globals, foreign
}

// federationLeaveHubError wraps a hub-side failure with guidance to retry with
// local-only, since local teardown is intentionally skipped when the hub
// revoke cannot complete (mirrors the CLI guidance).
func federationLeaveHubError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("hub revoke failed: %w; retry with local-only to tear down locally, then revoke on the hub later", err)
}

func (m Model) cursorMoveFederation(msg tea.KeyPressMsg, rows []FederationProjectStatus) (Model, bool) {
	switch msg.String() {
	case "j", "down":
		if m.federation.cursor < len(rows)-1 {
			m.federation.cursor++
		}
		return m, true
	case "k", "up":
		if m.federation.cursor > 0 {
			m.federation.cursor--
		}
		return m, true
	case "g", "home":
		m.federation.cursor = 0
		return m, true
	case "G", "end":
		m.federation.cursor = max(len(rows)-1, 0)
		return m, true
	}
	return m, false
}

func (m Model) startFederationHubBrowse() (Model, tea.Cmd) {
	target, cursor, ok := selectedFederationBrowseHub(m)
	m = m.invalidateFederationOp()
	m.federation.mode = federationModeBrowseHubs
	m.federation.hubCursor = cursor
	m.federation.hubProjectCursor = 0
	m.federation.hubProjects = nil
	m.federation.hubProjectsLoading = false
	m.federation.draft = federationDraft{}
	m.federation.recovery = federationRecovery{}
	if !ok {
		m.federation.op.err = errors.New("no catalog hub daemons configured")
		return m, nil
	}
	m.federation.draft.HubTarget = target
	m.federation.hubProjectsLoading = true
	m.federation.enrollGen++
	return m, m.fetchFederationHubProjects(target)
}

func (m Model) startFederationEnrollment() (Model, tea.Cmd) {
	m = m.invalidateFederationOp()
	m.federation.draft = newFederationDraft(m.list.actor)
	m.federation.localProjectCursor = 0
	m.federation.hubCursor = 0
	m.federation.hubProjectCursor = 0
	m.federation.hubProjects = nil
	m.federation.hubProjectsLoading = false
	// Never skip the local-project step: the choice between adopting a local
	// project and creating a new replica must stay explicit and visible. An
	// active project only pre-positions the cursor (the adopt flow costs one
	// Enter), instead of silently pre-arming adoption — which is how a
	// misclick could federate the wrong project.
	if projectID, _, ok := m.defaultFederationProject(); ok {
		for i, row := range federationLocalProjectRows(m) {
			if !row.createReplica && row.project.ID == projectID {
				m.federation.localProjectCursor = i
				break
			}
		}
	}
	m.federation.mode = federationModeSelectLocalProject
	return m, nil
}

func (m Model) captureFederationSelectedProject() Model {
	switch m.view {
	case viewFederation:
		return m
	case viewProjects:
		projectID, projectName, ok := m.selectedProjectsViewProject()
		m.federation.selectedProjectSet = true
		if !ok {
			m.federation.selectedProjectID = 0
			m.federation.selectedProjectName = ""
			return m
		}
		m.federation.selectedProjectID = projectID
		m.federation.selectedProjectName = projectName
		return m
	default:
		m.federation.selectedProjectSet = true
		projectID, projectName, ok := m.currentFederationProject()
		if !ok {
			m.federation.selectedProjectID = 0
			m.federation.selectedProjectName = ""
			return m
		}
		m.federation.selectedProjectID = projectID
		m.federation.selectedProjectName = projectName
		return m
	}
}

func (m Model) selectedProjectsViewProject() (int64, string, bool) {
	rows := projectsRows(m.projectsByID, m.projectIdentByID, m.projectStats)
	if m.projectsCursor < 0 || m.projectsCursor >= len(rows) {
		return 0, "", false
	}
	row := rows[m.projectsCursor]
	if row.sentinel || row.projectID == 0 || row.name == "" {
		return 0, "", false
	}
	return row.projectID, row.name, true
}

func (m Model) defaultFederationProject() (int64, string, bool) {
	if m.federation.selectedProjectSet {
		if m.federation.selectedProjectID == 0 || m.federation.selectedProjectName == "" {
			return 0, "", false
		}
		return m.federation.selectedProjectID, m.federation.selectedProjectName, true
	}
	return m.currentFederationProject()
}

func newFederationDraft(actor string) federationDraft {
	caps, err := hubfederation.NormalizeCapabilities("pull,push,lease")
	if err != nil {
		caps.API = "claim,pull,push"
		caps.Display = "pull,push,lease"
	}
	if strings.TrimSpace(actor) == "" {
		actor = "anonymous"
	}
	return federationDraft{
		RequestedActor:      actor,
		APICapabilities:     caps.API,
		DisplayCapabilities: caps.Display,
		PushEnabled:         true,
		AdoptExisting:       true,
	}
}

func (m Model) currentFederationProject() (int64, string, bool) {
	if m.scope.allProjects || m.scope.empty || m.scope.projectID == 0 {
		return 0, "", false
	}
	name := m.scope.projectName
	if name == "" {
		name = m.scope.homeProjectName
	}
	if name == "" {
		name = m.projectsByID[m.scope.projectID]
	}
	if name == "" {
		return 0, "", false
	}
	return m.scope.projectID, name, true
}

type federationLocalProjectRow struct {
	createReplica bool
	project       ProjectSummary
}

func federationLocalProjectRows(m Model) []federationLocalProjectRow {
	rows := []federationLocalProjectRow{{createReplica: true}}
	projects := make([]ProjectSummary, 0, len(m.projectsByID)+1)
	for id, name := range m.projectsByID {
		projects = append(projects, ProjectSummary{ID: id, Name: name})
	}
	// The boot project-list fetch is asynchronous and can fail, so the
	// scoped/selected project must stay adoptable from scope state alone —
	// an empty cache must not reduce the flow to "create replica" only.
	if id, name, ok := m.defaultFederationProject(); ok {
		if _, cached := m.projectsByID[id]; !cached {
			projects = append(projects, ProjectSummary{ID: id, Name: name})
		}
	}
	sort.SliceStable(projects, func(i, j int) bool {
		li, lj := strings.ToLower(projects[i].Name), strings.ToLower(projects[j].Name)
		if li != lj {
			return li < lj
		}
		return projects[i].ID < projects[j].ID
	})
	for _, project := range projects {
		rows = append(rows, federationLocalProjectRow{project: project})
	}
	return rows
}

// federationHubProjectRow is one selectable line in the hub-project step.
// sentinel marks the adopt-same-name row — the implicit "use or create the
// hub project matching this spoke project" entry that exists only in the
// adopt flow and only at index 0. Its meaning used to live in a bare
// `cursor == 0` test, with the count, the selection and the label each
// re-deriving the same convention independently.
//
// Mirrors projectsRow (projects_view.go) and federationLocalProjectRow
// above: build the rows once, then count, render and select off that list.
type federationHubProjectRow struct {
	sentinel bool
	project  ProjectSummary
	label    string
}

// federationHubProjectRows builds the enrollment flow's hub-project rows.
//
// create-replica: one row per hub project, no sentinel — the operator is
// picking the hub project to replicate, and the local project does not
// exist yet.
//
// adopt: the sentinel first, then every hub project whose name differs from
// the spoke project's. The same-name project is folded into the sentinel
// (the sentinel's label says whether it will be reused or created), which is
// why it must not appear again as its own row.
func federationHubProjectRows(m Model) []federationHubProjectRow {
	if m.federation.draft.CreateReplica {
		rows := make([]federationHubProjectRow, 0, len(m.federation.hubProjects))
		for _, project := range m.federation.hubProjects {
			rows = append(rows, federationHubProjectRow{project: project, label: project.Name})
		}
		return rows
	}
	rows := []federationHubProjectRow{{
		sentinel: true,
		label:    federationDefaultHubProjectLabel(m.federation.draft, m.federation.hubProjects),
	}}
	for _, project := range m.federation.hubProjects {
		if project.Name == m.federation.draft.SpokeProjectName {
			continue
		}
		rows = append(rows, federationHubProjectRow{project: project, label: project.Name})
	}
	return rows
}

// federationBrowseHubProjectRows builds the read-only catalog browse rows:
// every hub project, unfiltered, with no sentinel, labeled "<id> <name>".
// Browse shares federationHubProjectCursor and federationHubProjects with
// the enrollment flow but none of its row semantics — this builder is where
// that third convention is named.
func federationBrowseHubProjectRows(m Model) []federationHubProjectRow {
	rows := make([]federationHubProjectRow, 0, len(m.federation.hubProjects))
	for _, project := range m.federation.hubProjects {
		rows = append(rows, federationHubProjectRow{
			project: project,
			label:   federationBrowseHubProjectLabel(project),
		})
	}
	return rows
}

// federationHubProjectRowsForMode picks the builder the current mode's
// cursor indexes. Used by the message handler, which runs for both modes.
func federationHubProjectRowsForMode(m Model) []federationHubProjectRow {
	if m.federation.mode == federationModeBrowseHubs {
		return federationBrowseHubProjectRows(m)
	}
	return federationHubProjectRows(m)
}

// selectedFederationHubProjectRow returns the row Enter acts on. The cursor
// is clamped exactly the way the renderer clamps it, so the highlighted row
// and the selected row are the same row by construction.
func (m Model) selectedFederationHubProjectRow() (federationHubProjectRow, bool) {
	rows := federationHubProjectRows(m)
	if len(rows) == 0 {
		return federationHubProjectRow{}, false
	}
	return rows[clampFederationIndex(m.federation.hubProjectCursor, len(rows), 0)], true
}

type federationHubRow struct {
	target daemonTarget
}

func federationHubRows(m Model) []federationHubRow {
	rows := make([]federationHubRow, 0, len(m.daemonTargets))
	for _, target := range m.daemonTargets {
		rows = append(rows, federationHubRow{target: target})
	}
	return rows
}

func selectedFederationBrowseHub(m Model) (daemonTarget, int, bool) {
	rows := federationHubRows(m)
	if len(rows) == 0 {
		return daemonTarget{}, 0, false
	}
	cursor := clampFederationIndex(m.federation.hubCursor, len(rows), 0)
	if !daemonTargetsMatch(rows[cursor].target, m.activeDaemon) {
		return rows[cursor].target, cursor, true
	}
	for i, row := range rows {
		if !daemonTargetsMatch(row.target, m.activeDaemon) {
			return row.target, i, true
		}
	}
	return daemonTarget{}, cursor, false
}

func (m Model) selectFederationHub(target daemonTarget) (Model, tea.Cmd) {
	m.federation.op.err = nil
	if daemonTargetsMatch(target, m.activeDaemon) {
		m.federation.op.err = errors.New("active daemon cannot be selected as hub")
		return m, nil
	}
	if target.Local {
		m.federation.op.err = errors.New("local hub targets cannot be used for federation enrollment; select a hub daemon with a spoke-reachable URL")
		return m, nil
	}
	if err := validateFederationHubTargetCredential(target); err != nil {
		m.federation.op.err = err
		return m, nil
	}
	if !target.Local {
		if _, err := normalizeRemoteURLForTUI(target.URL, target.resolved.AllowInsecure); err != nil {
			m.federation.op.err = err
			return m, nil
		}
	}
	m.federation.draft.HubTarget = target
	m.federation.draft.AllowInsecure = target.resolved.AllowInsecure
	m.federation.mode = federationModeSelectHubProject
	m.federation.hubProjectsLoading = true
	m.federation.hubProjects = nil
	m.federation.hubProjectCursor = 0
	m.federation.enrollGen++
	return m, m.fetchFederationHubProjects(target)
}

func (m Model) fetchFederationHubProjects(target daemonTarget) tea.Cmd {
	connGen := m.connGen
	gen := m.federation.enrollGen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		client, resolved, err := newFederationHubAdminClient(ctx, target)
		if err != nil {
			return federationHubProjectsLoadedMsg{connGen: connGen, gen: gen, target: target, err: err}
		}
		instance, err := client.GetInstance(ctx)
		if err != nil {
			return federationHubProjectsLoadedMsg{connGen: connGen, gen: gen, target: resolved, err: err}
		}
		projects, err := client.ListProjects(ctx)
		return federationHubProjectsLoadedMsg{
			connGen:  connGen,
			gen:      gen,
			target:   resolved,
			instance: instance,
			projects: projects,
			err:      err,
		}
	}
}

func (m Model) executeFederationEnrollment(attempt uint64) tea.Cmd {
	connGen := m.connGen
	draft := m.federation.draft
	instanceUID := m.federation.instance.InstanceUID
	spoke := m.api
	active := m.activeDaemon
	return func() tea.Msg {
		result, err := runFederationEnrollment(context.Background(), draft, instanceUID, active, spoke)
		return federationEnrollResultMsg{
			connGen: connGen,
			attempt: attempt,
			result:  result,
			err:     err,
		}
	}
}

func runFederationEnrollment(
	parent context.Context,
	draft federationDraft,
	instanceUID string,
	active daemonTarget,
	spoke federationSpokeAPI,
) (federationEnrollResult, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	result := federationEnrollResult{Draft: draft}
	instanceUID = strings.TrimSpace(instanceUID)
	if instanceUID == "" {
		return result, errors.New("spoke instance UID is not loaded; refresh federation status before enrollment")
	}
	hubAdmin, resolvedHub, err := newFederationHubAdminClient(ctx, draft.HubTarget)
	if err != nil {
		return result, err
	}
	draft.HubTarget = resolvedHub
	result.Draft = draft
	hubURL := federationDaemonEndpoint(resolvedHub)
	result.HubURL = hubURL
	hubProject, err := resolveFederationHubProject(ctx, hubAdmin, draft)
	if err != nil {
		return result, err
	}
	metadata, err := hubAdmin.EnableFederation(ctx, hubProject.ID, draft.RequestedActor)
	if err != nil {
		return result, err
	}
	enrollment, err := hubAdmin.CreateFederationEnrollment(ctx, CreateFederationEnrollmentInput{
		SpokeInstanceUID:             instanceUID,
		ProjectID:                    &hubProject.ID,
		Capabilities:                 draft.APICapabilities,
		Actor:                        draft.RequestedActor,
		AllowAdoptionSnapshotAuthors: draft.AdoptExisting,
	})
	if err != nil {
		return result, err
	}
	result.Enrollment = enrollment
	result.Metadata = metadata
	recovery := baseFederationRecovery(draft, active, resolvedHub, hubURL, hubProject, enrollment)
	enrollmentClient, err := newFederationEnrollmentClient(ctx, hubURL, enrollment.Token, draft.AllowInsecure)
	if err != nil {
		recovery.Stage = "metadata"
		recovery.Err = fmt.Errorf("hub %s: enrollment metadata fetch failed: %w", daemonName(resolvedHub), err)
		result.Recovery = recovery
		return result, recovery.Err
	}
	metadata, err = enrollmentClient.ProjectFederation(ctx, hubProject.ID)
	if err != nil {
		recovery.Stage = "metadata"
		recovery.Err = fmt.Errorf("hub %s: enrollment metadata fetch failed: %w", daemonName(resolvedHub), err)
		result.Recovery = recovery
		return result, recovery.Err
	}
	result.Metadata = metadata
	recovery.Command.HubProjectUID = metadata.ProjectUID
	replicaProjectName := federationReplicaProjectName(draft, metadata.ProjectName)
	recovery.Command.ProjectName = replicaProjectName
	recovery.Command.ReplayHorizonEventID = metadata.ReplayHorizonEventID
	recovery.Command.BaselineThroughEventID = metadata.BaselineThroughEventID
	if spoke == nil {
		recovery.Stage = "join"
		recovery.Err = errors.New("spoke: join failed: daemon client unavailable")
		result.Recovery = recovery
		return result, recovery.Err
	}
	replica, err := spoke.CreateFederationReplica(ctx, CreateFederationReplicaInput{
		HubURL:                 hubURL,
		HubProjectID:           hubProject.ID,
		HubProjectUID:          metadata.ProjectUID,
		ProjectName:            replicaProjectName,
		ReplayHorizonEventID:   metadata.ReplayHorizonEventID,
		BaselineThroughEventID: metadata.BaselineThroughEventID,
		Token:                  enrollment.Token,
		Capabilities:           draft.APICapabilities,
		Actor:                  enrollment.Actor,
		AllowInsecure:          draft.AllowInsecure,
		PushEnabled:            draft.PushEnabled,
		AdoptExisting:          draft.AdoptExisting,
	})
	if err != nil {
		recovery.Stage = "join"
		recovery.Err = fmt.Errorf("spoke: join failed: %w", err)
		result.Recovery = recovery
		return result, recovery.Err
	}
	result.Replica = replica
	return result, nil
}

func resolveFederationHubProject(
	ctx context.Context,
	hub federationHubAdminAPI,
	draft federationDraft,
) (ProjectSummary, error) {
	if draft.Operation == federationOperationAdoptSameName {
		if draft.HubProjectID != 0 {
			return ProjectSummary{ID: draft.HubProjectID, Name: draft.HubProjectName}, nil
		}
		return hub.EnsureProject(ctx, draft.SpokeProjectName, draft.RequestedActor)
	}
	return ProjectSummary{ID: draft.HubProjectID, Name: draft.HubProjectName}, nil
}

func federationReplicaProjectName(draft federationDraft, hubProjectName string) string {
	if strings.TrimSpace(draft.SpokeProjectName) != "" &&
		(draft.AdoptExisting || draft.Operation == federationOperationRejoin) {
		// Adoption and rejoin both target an existing local project, which may
		// be named differently from the hub project.
		return draft.SpokeProjectName
	}
	return hubProjectName
}

func baseFederationRecovery(
	draft federationDraft,
	active daemonTarget,
	hub daemonTarget,
	hubURL string,
	hubProject ProjectSummary,
	enrollment FederationEnrollment,
) federationRecovery {
	projectName := federationReplicaProjectName(draft, hubProject.Name)
	return federationRecovery{
		HubName:       daemonName(hub),
		SpokeName:     daemonName(active),
		SpokeEndpoint: federationDaemonEndpoint(active),
		Token:         enrollment.Token,
		Command: federationRecoveryCommand{
			HubURL:             hubURL,
			HubProjectID:       hubProject.ID,
			ProjectName:        projectName,
			Token:              enrollment.Token,
			Actor:              enrollment.Actor,
			Capabilities:       draft.APICapabilities,
			PushEnabled:        draft.PushEnabled,
			AllowInsecure:      draft.AllowInsecure,
			AdoptExisting:      draft.AdoptExisting,
			SpokeName:          daemonName(active),
			SpokeEndpoint:      federationDaemonEndpoint(active),
			SpokeAllowInsecure: active.resolved.AllowInsecure,
			SpokeToken:         active.resolved.Token,
		},
	}
}

func (m Model) previewFederationEnrollment() (Model, tea.Cmd) {
	m.federation.op.err = nil
	draft := m.federation.draft
	draft.Operation = ""
	draft.HubProjectID = 0
	draft.HubProjectName = ""
	draft.BlockedReason = ""
	row, hasRow := m.selectedFederationHubProjectRow()
	project, hasProject := row.project, hasRow && !row.sentinel
	if draft.CreateReplica {
		if !hasProject {
			m.federation.op.err = errors.New("select an existing hub project to create a local replica")
			return m, nil
		}
		draft.Operation = federationOperationCreateReplica
		draft.HubProjectID = project.ID
		draft.HubProjectName = project.Name
		draft.SpokeProjectName = project.Name
		draft.AdoptExisting = false
		m.federation.selectedProjectSet = true
		m.federation.selectedProjectID = 0
		m.federation.selectedProjectName = project.Name
		if holderID, holderName, ok := localProjectByUID(m, project.UID); ok {
			// The hub project's identity already exists locally, so this is a
			// rejoin of that project (it previously left this federation), not
			// a new replica — the daemon would refuse the new-replica request.
			if status, bound := localProjectFederationBinding(m, holderID, holderName); bound {
				role := status.Role
				if role == "" {
					role = "unknown"
				}
				draft.BlockedReason = fmt.Sprintf(
					"local project %q already has federation binding as %s", holderName, role)
			} else {
				draft.Operation = federationOperationRejoin
				draft.SpokeProjectName = holderName
				m.federation.selectedProjectID = holderID
				m.federation.selectedProjectName = holderName
			}
		} else if localProjectNameExists(m, draft.SpokeProjectName) {
			draft.BlockedReason = fmt.Sprintf("local project %q already exists", draft.SpokeProjectName)
		}
	} else {
		draft.AdoptExisting = true
		if hasRow && row.sentinel {
			draft.Operation = federationOperationAdoptSameName
			draft.HubProjectName = draft.SpokeProjectName
			if same, ok := hubProjectByName(m.federation.hubProjects, draft.SpokeProjectName); ok {
				draft.HubProjectID = same.ID
			}
		} else if hasProject {
			draft.Operation = federationOperationAdoptSelectedHub
			draft.HubProjectID = project.ID
			draft.HubProjectName = project.Name
		}
		if status, ok := localProjectFederationBinding(m, draft.SpokeProjectID, draft.SpokeProjectName); ok {
			role := status.Role
			if role == "" {
				role = "unknown"
			}
			projectName := draft.SpokeProjectName
			if projectName == "" {
				projectName = status.ProjectName
			}
			draft.BlockedReason = fmt.Sprintf(
				"local project %q already has federation binding as %s",
				projectName,
				role,
			)
		}
		if draft.BlockedReason == "" && draft.HubProjectID != 0 {
			// The selected local project may already share the target hub
			// project's identity — the post-leave state. Adoption would rewrite
			// its event history a second time; rejoin just rebinds it. Mirrors
			// the create-replica branch's detection for the adopt-first flow.
			// Either UID being unknown disables that comparison, so block
			// rather than default to history-rewriting adoption.
			hubUID := federationHubProjectUIDByID(m, draft.HubProjectID)
			localUID := m.projectUIDByID[draft.SpokeProjectID]
			switch {
			case hubUID != "" && localUID == hubUID:
				draft.Operation = federationOperationRejoin
				draft.AdoptExisting = false
			case localUID == "":
				draft.BlockedReason = fmt.Sprintf(
					"local project %q identity is still loading; press esc and retry",
					draft.SpokeProjectName)
			case hubUID == "":
				draft.BlockedReason = fmt.Sprintf(
					"hub project %q identity is unknown; refresh the hub project list and retry",
					draft.HubProjectName)
			}
		}
	}
	draft.AllowInsecure = draft.HubTarget.resolved.AllowInsecure
	m.federation.draft = draft
	m.federation.mode = federationModePreview
	return m, nil
}

func hubProjectByName(projects []ProjectSummary, name string) (ProjectSummary, bool) {
	for _, project := range projects {
		if project.Name == name {
			return project, true
		}
	}
	return ProjectSummary{}, false
}

func localProjectNameExists(m Model, name string) bool {
	for _, existing := range m.projectsByID {
		if existing == name {
			return true
		}
	}
	return false
}

// federationHubProjectUIDByID returns the UID of a listed hub project, or ""
// when the project (or its UID) is unknown.
func federationHubProjectUIDByID(m Model, hubProjectID int64) string {
	for _, p := range m.federation.hubProjects {
		if p.ID == hubProjectID {
			return p.UID
		}
	}
	return ""
}

// localProjectByUID finds the local project holding a hub project's UID. A
// match means the local project shares identity with the hub project — the
// post-leave rejoin state.
func localProjectByUID(m Model, uid string) (int64, string, bool) {
	if strings.TrimSpace(uid) == "" {
		return 0, "", false
	}
	for id, projectUID := range m.projectUIDByID {
		if projectUID == uid {
			return id, m.projectsByID[id], true
		}
	}
	return 0, "", false
}

func localProjectFederationBinding(
	m Model,
	projectID int64,
	projectName string,
) (FederationProjectStatus, bool) {
	for _, status := range m.federation.statuses {
		if status.Role == "" {
			continue
		}
		if projectID != 0 && status.ProjectID == projectID {
			return status, true
		}
		if projectName != "" && status.ProjectName == projectName {
			return status, true
		}
	}
	return FederationProjectStatus{}, false
}

func nextFederationCursor(msg tea.KeyPressMsg, cursor, count int) (int, bool) {
	switch msg.String() {
	case "j", "down":
		return clampFederationIndex(cursor+1, count, 0), true
	case "k", "up":
		return clampFederationIndex(cursor-1, count, 0), true
	case "g", "home":
		return 0, true
	case "G", "end":
		return clampFederationIndex(count-1, count, 0), true
	}
	return cursor, false
}

func clampFederationIndex(v, count, fallback int) int {
	if count <= 0 {
		return fallback
	}
	if v < 0 {
		return 0
	}
	if v >= count {
		return count - 1
	}
	return v
}

func (m Model) escFromFederationView() (Model, tea.Cmd) {
	m = m.invalidateFederationOp()
	if m.prevView == viewFederation {
		m.view = viewList
		return m, nil
	}
	m.view = m.prevView
	if m.view == viewHelp {
		m.view = viewList
	}
	return m, nil
}

func federationSpokeStatuses(statuses []FederationProjectStatus) []FederationProjectStatus {
	rows := make([]FederationProjectStatus, 0, len(statuses))
	for _, status := range statuses {
		if status.Role == "spoke" {
			rows = append(rows, status)
		}
	}
	return rows
}

func clampFederationCursor(cursor int, rows []FederationProjectStatus) int {
	if len(rows) == 0 || cursor < 0 {
		return 0
	}
	if cursor >= len(rows) {
		return len(rows) - 1
	}
	return cursor
}

func (m *Model) moveFederationCursor(delta int) {
	rows := federationSpokeStatuses(m.federation.statuses)
	if delta < 0 && m.federation.cursor > 0 {
		m.federation.cursor--
	}
	if delta > 0 && m.federation.cursor < len(rows)-1 {
		m.federation.cursor++
	}
}

func (m Model) mouseFederationClick(y int) (Model, tea.Cmd) {
	row := y - federationFirstRowY
	if row < 0 {
		return m, nil
	}
	rows := federationSpokeStatuses(m.federation.statuses)
	if len(rows) == 0 {
		return m, nil
	}
	budget := len(rows)
	if m.height > 0 {
		budget = max(m.height-federationViewChromeRows, 1)
	}
	start, end := windowBounds(len(rows), m.federation.cursor, budget)
	idx := start + row
	if idx < start || idx >= end || idx >= len(rows) {
		return m, nil
	}
	m.federation.cursor = idx
	return m, nil
}
