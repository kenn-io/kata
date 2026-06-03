package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
)

type federationOperation string

const (
	federationOperationAdoptSameName    federationOperation = "adopt-same-name"
	federationOperationAdoptSelectedHub federationOperation = "adopt-selected-hub"
	federationOperationCreateReplica    federationOperation = "create-replica"
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
	m = m.captureFederationSelectedProject()
	m.prevView = m.view
	m.view = viewFederation
	m.federationMode = federationModeList
	m.federationDraft = federationDraft{}
	m.federationLoading = true
	m.federationErr = nil
	m.federationGen++
	return m, m.fetchFederationStatus()
}

func (m Model) fetchFederationStatus() tea.Cmd {
	api := m.api
	connGen := m.connGen
	gen := m.federationGen
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
	if m.staleConnMsg(msg.connGen) || msg.gen != m.federationGen {
		return m
	}
	m.federationLoading = false
	m.federationErr = msg.err
	if msg.err != nil {
		return m
	}
	m.federationInstance = msg.instance
	m.federationStatuses = msg.status.Statuses
	m.federationCursor = clampFederationCursor(m.federationCursor, federationSpokeStatuses(m.federationStatuses))
	return m
}

func (m Model) handleFederationHubProjectsLoaded(msg federationHubProjectsLoadedMsg) Model {
	if m.staleConnMsg(msg.connGen) || msg.gen != m.federationEnrollGen {
		return m
	}
	if m.federationMode != federationModeSelectHubProject && m.federationMode != federationModeBrowseHubs {
		return m
	}
	m.federationHubProjectsLoading = false
	m.federationEnrollErr = msg.err
	if msg.err != nil {
		return m
	}
	m.federationDraft.HubTarget = msg.target
	m.federationDraft.HubInstance = msg.instance
	if actor := strings.TrimSpace(msg.instance.Auth.Actor); actor != "" {
		m.federationDraft.RequestedActor = actor
	}
	m.federationHubProjects = msg.projects
	count := federationHubProjectRowCount(m)
	if m.federationMode == federationModeBrowseHubs {
		count = len(m.federationHubProjects)
	}
	m.federationHubProjectCursor = clampFederationIndex(m.federationHubProjectCursor, count, 0)
	return m
}

func (m Model) handleFederationEnrollResult(msg federationEnrollResultMsg) (Model, tea.Cmd) {
	if m.staleConnMsg(msg.connGen) || msg.attempt != m.federationEnrollAttempt {
		return m, nil
	}
	m.federationEnrollRunning = false
	if msg.err != nil {
		if msg.result.Recovery.Token == "" && msg.result.Recovery.Stage == "" {
			m.federationEnrollErr = msg.err
			m.federationMode = federationModePreview
			return m, nil
		}
		m.federationRecovery = msg.result.Recovery
		m.federationRecovery.Err = msg.err
		m.federationMode = federationModeRecovery
		return m, nil
	}
	m.federationResult = msg.result
	m.federationMode = federationModeResult
	m.federationLoading = true
	m.federationErr = nil
	m.federationGen++
	return m, m.fetchFederationStatus()
}

func (m Model) routeFederationViewKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	rows := federationSpokeStatuses(m.federationStatuses)
	switch m.federationMode {
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
		m.federationLoading = true
		m.federationErr = nil
		m.federationGen++
		return m, m.fetchFederationStatus()
	case "b":
		return m.startFederationHubBrowse()
	case "enter":
		if m.federationCursor < 0 || m.federationCursor >= len(rows) {
			return m, nil
		}
		m.federationMode = federationModeDetail
		return m, nil
	}
	return m, nil
}

func (m Model) routeFederationDetailKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace":
		m.federationMode = federationModeList
		return m, nil
	case "r":
		m.federationLoading = true
		m.federationErr = nil
		m.federationGen++
		return m, m.fetchFederationStatus()
	}
	return m, nil
}

func (m Model) routeFederationLocalProjectKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	rows := federationLocalProjectRows(m)
	if next, ok := nextFederationCursor(msg, m.federationLocalProjectCursor, len(rows)); ok {
		m.federationLocalProjectCursor = next
		return m, nil
	}
	switch msg.String() {
	case "esc":
		m.federationMode = federationModeList
		return m, nil
	case "enter":
		if len(rows) == 0 {
			return m, nil
		}
		row := rows[clampFederationIndex(m.federationLocalProjectCursor, len(rows), 0)]
		if row.createReplica {
			m.federationDraft.CreateReplica = true
			m.federationDraft.SelectedLocalProject = true
			m.federationDraft.AdoptExisting = false
			m.federationDraft.SpokeProjectID = 0
			m.federationDraft.SpokeProjectName = ""
			m.federationSelectedProjectSet = true
			m.federationSelectedProjectID = 0
			m.federationSelectedProjectName = ""
		} else {
			m.federationDraft.CreateReplica = false
			m.federationDraft.SelectedLocalProject = true
			m.federationDraft.AdoptExisting = true
			m.federationDraft.SpokeProjectID = row.project.ID
			m.federationDraft.SpokeProjectName = row.project.Name
			m.federationSelectedProjectSet = true
			m.federationSelectedProjectID = row.project.ID
			m.federationSelectedProjectName = row.project.Name
		}
		m.federationMode = federationModeSelectHub
		m.federationHubCursor = 0
		m.federationEnrollErr = nil
		return m, nil
	}
	return m, nil
}

func (m Model) routeFederationHubKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	rows := federationHubRows(m)
	if next, ok := nextFederationCursor(msg, m.federationHubCursor, len(rows)); ok {
		m.federationHubCursor = next
		return m, nil
	}
	switch msg.String() {
	case "esc":
		if m.federationDraft.SelectedLocalProject {
			m.federationMode = federationModeSelectLocalProject
		} else {
			m.federationMode = federationModeList
		}
		return m, nil
	case "enter":
		if len(rows) == 0 {
			return m, nil
		}
		target := rows[clampFederationIndex(m.federationHubCursor, len(rows), 0)].target
		return m.selectFederationHub(target)
	}
	return m, nil
}

func (m Model) routeFederationHubProjectKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	count := federationHubProjectRowCount(m)
	if next, ok := nextFederationCursor(msg, m.federationHubProjectCursor, count); ok {
		m.federationHubProjectCursor = next
		return m, nil
	}
	switch msg.String() {
	case "esc":
		m.federationMode = federationModeSelectHub
		return m, nil
	case "enter":
		return m.previewFederationEnrollment()
	}
	return m, nil
}

func (m Model) routeFederationBrowseHubsKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	count := len(m.federationHubProjects)
	if next, ok := nextFederationCursor(msg, m.federationHubProjectCursor, count); ok {
		m.federationHubProjectCursor = next
		return m, nil
	}
	switch msg.String() {
	case "esc", "backspace":
		m.federationMode = federationModeList
		return m, nil
	case "enter":
		return m, nil
	}
	return m, nil
}

func (m Model) routeFederationPreviewKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace":
		m.federationMode = federationModeSelectHubProject
		return m, nil
	case "enter":
		if m.federationDraft.BlockedReason != "" || m.federationEnrollRunning {
			return m, nil
		}
		m.federationEnrollAttempt++
		m.federationEnrollRunning = true
		m.federationEnrollErr = nil
		return m, m.executeFederationEnrollment(m.federationEnrollAttempt)
	}
	return m, nil
}

func (m Model) routeFederationRecoveryKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "R":
		m.federationRecovery.Reveal = true
		return m, nil
	case "esc":
		m.federationMode = federationModePreview
		return m, nil
	}
	return m, nil
}

func (m Model) routeFederationResultKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "enter":
		m.federationMode = federationModeList
		return m, nil
	}
	return m, nil
}

func (m Model) cursorMoveFederation(msg tea.KeyMsg, rows []FederationProjectStatus) (Model, bool) {
	switch msg.String() {
	case "j", "down":
		if m.federationCursor < len(rows)-1 {
			m.federationCursor++
		}
		return m, true
	case "k", "up":
		if m.federationCursor > 0 {
			m.federationCursor--
		}
		return m, true
	case "g", "home":
		m.federationCursor = 0
		return m, true
	case "G", "end":
		m.federationCursor = len(rows) - 1
		if m.federationCursor < 0 {
			m.federationCursor = 0
		}
		return m, true
	}
	return m, false
}

func (m Model) startFederationHubBrowse() (Model, tea.Cmd) {
	target, cursor, ok := selectedFederationBrowseHub(m)
	m.federationMode = federationModeBrowseHubs
	m.federationHubCursor = cursor
	m.federationHubProjectCursor = 0
	m.federationHubProjects = nil
	m.federationHubProjectsLoading = false
	m.federationEnrollErr = nil
	m.federationDraft = federationDraft{}
	m.federationResult = federationEnrollResult{}
	m.federationRecovery = federationRecovery{}
	if !ok {
		m.federationEnrollErr = errors.New("no catalog hub daemons configured")
		return m, nil
	}
	m.federationDraft.HubTarget = target
	m.federationHubProjectsLoading = true
	m.federationEnrollGen++
	return m, m.fetchFederationHubProjects(target)
}

func (m Model) startFederationEnrollment() (Model, tea.Cmd) {
	m.federationDraft = newFederationDraft(m.list.actor)
	m.federationLocalProjectCursor = 0
	m.federationHubCursor = 0
	m.federationHubProjectCursor = 0
	m.federationHubProjects = nil
	m.federationHubProjectsLoading = false
	m.federationEnrollErr = nil
	if projectID, projectName, ok := m.defaultFederationProject(); ok {
		m.federationDraft.SpokeProjectID = projectID
		m.federationDraft.SpokeProjectName = projectName
		m.federationDraft.AdoptExisting = true
		m.federationMode = federationModeSelectHub
		return m, nil
	}
	m.federationMode = federationModeSelectLocalProject
	return m, nil
}

func (m Model) captureFederationSelectedProject() Model {
	switch m.view {
	case viewFederation:
		return m
	case viewProjects:
		projectID, projectName, ok := m.selectedProjectsViewProject()
		m.federationSelectedProjectSet = true
		if !ok {
			m.federationSelectedProjectID = 0
			m.federationSelectedProjectName = ""
			return m
		}
		m.federationSelectedProjectID = projectID
		m.federationSelectedProjectName = projectName
		return m
	default:
		m.federationSelectedProjectSet = true
		projectID, projectName, ok := m.currentFederationProject()
		if !ok {
			m.federationSelectedProjectID = 0
			m.federationSelectedProjectName = ""
			return m
		}
		m.federationSelectedProjectID = projectID
		m.federationSelectedProjectName = projectName
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
	if m.federationSelectedProjectSet {
		if m.federationSelectedProjectID == 0 || m.federationSelectedProjectName == "" {
			return 0, "", false
		}
		return m.federationSelectedProjectID, m.federationSelectedProjectName, true
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
	projects := make([]ProjectSummary, 0, len(m.projectsByID))
	for id, name := range m.projectsByID {
		projects = append(projects, ProjectSummary{ID: id, Name: name})
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
	cursor := clampFederationIndex(m.federationHubCursor, len(rows), 0)
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
	m.federationEnrollErr = nil
	if daemonTargetsMatch(target, m.activeDaemon) {
		m.federationEnrollErr = errors.New("active daemon cannot be selected as hub")
		return m, nil
	}
	if target.Local {
		m.federationEnrollErr = errors.New("local hub targets cannot be used for federation enrollment; select a hub daemon with a spoke-reachable URL")
		return m, nil
	}
	resolved, err := resolveDaemonTargetToken(target)
	if err != nil {
		m.federationEnrollErr = err
		return m, nil
	}
	if !resolved.Local {
		if _, err := normalizeRemoteURLForTUI(resolved.URL, resolved.AllowInsecure); err != nil {
			m.federationEnrollErr = err
			return m, nil
		}
	}
	m.federationDraft.HubTarget = resolved
	m.federationDraft.AllowInsecure = resolved.AllowInsecure
	m.federationMode = federationModeSelectHubProject
	m.federationHubProjectsLoading = true
	m.federationHubProjects = nil
	m.federationHubProjectCursor = 0
	m.federationEnrollGen++
	return m, m.fetchFederationHubProjects(resolved)
}

func (m Model) fetchFederationHubProjects(target daemonTarget) tea.Cmd {
	connGen := m.connGen
	gen := m.federationEnrollGen
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
	draft := m.federationDraft
	instanceUID := m.federationInstance.InstanceUID
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
		return hub.EnsureProject(ctx, draft.SpokeProjectName)
	}
	return ProjectSummary{ID: draft.HubProjectID, Name: draft.HubProjectName}, nil
}

func federationReplicaProjectName(draft federationDraft, hubProjectName string) string {
	if draft.AdoptExisting && strings.TrimSpace(draft.SpokeProjectName) != "" {
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
			SpokeAllowInsecure: active.AllowInsecure,
			SpokeToken:         active.Token,
		},
	}
}

func (m Model) previewFederationEnrollment() (Model, tea.Cmd) {
	m.federationEnrollErr = nil
	draft := m.federationDraft
	draft.Operation = ""
	draft.HubProjectID = 0
	draft.HubProjectName = ""
	draft.BlockedReason = ""
	project, hasProject := m.selectedFederationHubProject()
	if draft.CreateReplica {
		if !hasProject {
			m.federationEnrollErr = errors.New("select an existing hub project to create a local replica")
			return m, nil
		}
		draft.Operation = federationOperationCreateReplica
		draft.HubProjectID = project.ID
		draft.HubProjectName = project.Name
		draft.SpokeProjectName = project.Name
		draft.AdoptExisting = false
		m.federationSelectedProjectSet = true
		m.federationSelectedProjectID = 0
		m.federationSelectedProjectName = project.Name
		if localProjectNameExists(m, draft.SpokeProjectName) {
			draft.BlockedReason = fmt.Sprintf("local project %q already exists", draft.SpokeProjectName)
		}
	} else {
		draft.AdoptExisting = true
		if m.federationHubProjectCursor == 0 {
			draft.Operation = federationOperationAdoptSameName
			draft.HubProjectName = draft.SpokeProjectName
			if same, ok := hubProjectByName(m.federationHubProjects, draft.SpokeProjectName); ok {
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
	}
	draft.AllowInsecure = draft.HubTarget.AllowInsecure
	m.federationDraft = draft
	m.federationMode = federationModePreview
	return m, nil
}

func federationHubProjectRowCount(m Model) int {
	if m.federationDraft.CreateReplica {
		return len(m.federationHubProjects)
	}
	return len(federationSelectableHubProjects(m)) + 1
}

func (m Model) selectedFederationHubProject() (ProjectSummary, bool) {
	projects := federationSelectableHubProjects(m)
	idx := m.federationHubProjectCursor
	if !m.federationDraft.CreateReplica {
		idx--
	}
	if idx < 0 || idx >= len(projects) {
		return ProjectSummary{}, false
	}
	return projects[idx], true
}

func federationSelectableHubProjects(m Model) []ProjectSummary {
	if m.federationDraft.CreateReplica {
		return m.federationHubProjects
	}
	projects := make([]ProjectSummary, 0, len(m.federationHubProjects))
	for _, project := range m.federationHubProjects {
		if project.Name == m.federationDraft.SpokeProjectName {
			continue
		}
		projects = append(projects, project)
	}
	return projects
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

func localProjectFederationBinding(
	m Model,
	projectID int64,
	projectName string,
) (FederationProjectStatus, bool) {
	for _, status := range m.federationStatuses {
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

func nextFederationCursor(msg tea.KeyMsg, cursor, count int) (int, bool) {
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
	rows := federationSpokeStatuses(m.federationStatuses)
	if delta < 0 && m.federationCursor > 0 {
		m.federationCursor--
	}
	if delta > 0 && m.federationCursor < len(rows)-1 {
		m.federationCursor++
	}
}

func (m Model) mouseFederationClick(y int) (Model, tea.Cmd) {
	row := y - federationFirstRowY
	if row < 0 {
		return m, nil
	}
	rows := federationSpokeStatuses(m.federationStatuses)
	if len(rows) == 0 {
		return m, nil
	}
	budget := len(rows)
	if m.height > 0 {
		budget = m.height - federationViewChromeRows
		if budget < 1 {
			budget = 1
		}
	}
	start, end := windowBounds(len(rows), m.federationCursor, budget)
	idx := start + row
	if idx < start || idx >= end || idx >= len(rows) {
		return m, nil
	}
	m.federationCursor = idx
	return m, nil
}
