<script lang="ts">
  import { onMount } from 'svelte'
  import { MediaQuery } from 'svelte/reactivity'
  import {
    DetailDrawer,
    EmptyState,
    MEDIA,
    IconButton,
    Checkbox,
    PageHeader,
    SegmentedControl,
    SidebarToggle,
    SplitResizeHandle,
    type SplitResizeEvent,
    Tooltip,
    TopBar,
    type TypeaheadOption,
  } from '@kenn-io/kit-ui'
  import KanbanIcon from '@lucide/svelte/icons/square-kanban'
  import ListIcon from '@lucide/svelte/icons/list'
  import LayoutPanelLeftIcon from '@lucide/svelte/icons/layout-panel-left'
  import LayoutPanelTopIcon from '@lucide/svelte/icons/layout-panel-top'
  import MenuIcon from '@lucide/svelte/icons/menu'
  import PlusIcon from '@lucide/svelte/icons/plus'

  import type { TokenOut, UIIssueReference } from '../lib/api/generated'
  import type { WebDaemonInfo } from '../lib/daemons/client'
  import type { KataRoute, SettingsSection, ShareableFilters, SystemView } from '../lib/router'
  import {
    deriveKataAreas,
    defaultKataTaskSearchFilters,
    projectKataWorkspaceView,
  } from '../lib/kata/authority'
  import { type BoardColumnID, type BoardMovePlan } from '../lib/kata/board'
  import { attentionViewer } from '../lib/kata/attention'
  import { createKataLinkFilters } from '../lib/kata/linkFilters'
  import { normalizeKataUISnapshot, withPendingSelection } from '../lib/kata/projection'
  import { localDateString } from '../lib/kata/dates'
  import type {
    KataTaskMutationResponse,
    KataCommentReplyIntent,
    KataTaskCloseRequest,
    KataTaskEditPatch,
    KataTaskSearchFilters,
    KataTaskStatusFilter,
    KataTaskSummary,
    KataTaskViewName,
  } from '../lib/kata/types'
  import {
    collectionIntentKey,
    snapshotIntentForRoute,
    type UISnapshot,
  } from '../lib/state/snapshot'
  import {
    defaultPreferences,
    maxSidebarWidth,
    minSidebarWidth,
    type Preferences,
  } from '../lib/state/preferences'
  import AccessManagement from './AccessManagement.svelte'
  import DisplayMenu from './DisplayMenu.svelte'
  import IssueBoard from './IssueBoard.svelte'
  import type { MutationContext, MutationResult } from '../lib/mutations/controller'
  import CredentialAudit from './CredentialAudit.svelte'
  import ProjectSyncStatus from './ProjectSyncStatus.svelte'
  import IssueCollection from './IssueCollection.svelte'
  import IssueDetail from './IssueDetail.svelte'
  import IssueFilters from './IssueFilters.svelte'
  import IssueGraph from './IssueGraph.svelte'
  import KataDaemonSwitcher from './KataDaemonSwitcher.svelte'
  import QuickCapture from './QuickCapture.svelte'
  import ProjectRecurrences from './ProjectRecurrences.svelte'
  import SearchSettings from './SearchSettings.svelte'
  import SettingsView, { type SettingsSectionID } from './SettingsView.svelte'
  import Sidebar from './Sidebar.svelte'
  import SplitLayout from './SplitLayout.svelte'

  type AppRoute = Exclude<KataRoute, { kind: 'route-error' }>
  type ChildParent = { projectID: number; projectUID: string; issueUID: string }

  interface Props {
    syncRefreshGeneration?: number
    route: AppRoute
    requestedRoute?: AppRoute | undefined
    snapshot: UISnapshot
    selfActor?: string | undefined
    loading: boolean
    canMutate: boolean
    mutationPending: boolean
    mutationMessage?: string | undefined
    draftResetGeneration: number
    draftFenceGeneration?: number | undefined
    ownerOptions: TypeaheadOption[]
    preferences?: Preferences | undefined
    daemons?: WebDaemonInfo[] | undefined
    activeDaemonID?: string | undefined
    daemonSwitching?: boolean | undefined
    reconnecting?: boolean | undefined
    stale?: boolean | undefined
    readOnly?: boolean | undefined
    daemonError?: string | undefined
    credentialTokens?: readonly TokenOut[] | undefined
    credentialLoading?: boolean | undefined
    credentialError?: string | undefined
    searchSettingsPostFetch?: typeof fetch | undefined
    canAdministerDaemon?: boolean | undefined
    onPreferencesChange?: ((preferences: Preferences) => void) | undefined
    onSelectDaemon?: ((id: string) => void) | undefined
    onOpenDaemon?: (() => void) | undefined
    onAccessMutation?:
      | ((
          draft: unknown,
          mutate: (context: MutationContext) => Promise<MutationResult>,
        ) => Promise<boolean>)
      | undefined
    onRefreshCredentials?: (() => void | Promise<void>) | undefined
    onSearchEmpty?: (() => void) | undefined
    onNavigate: (route: AppRoute) => void | Promise<void>
    onCreateProject: (name: string, designateInbox?: boolean) => Promise<KataTaskMutationResponse>
    onCreateIssue: (
      title: string,
      parent?: ChildParent,
      projectUID?: string,
    ) => void | Promise<void>
    searchReferences: (query: string) => Promise<UIIssueReference[]>
    onMoveIssue: (toProjectUID: string) => boolean | Promise<boolean>
    onPatchMetadata: (uid: string, patch: Record<string, unknown>) => boolean | Promise<boolean>
    onUploadAttachment?: ((uid: string, file: File) => Promise<void>) | undefined
    onAddComment: (
      uid: string,
      body: string,
      reply?: KataCommentReplyIntent,
    ) => boolean | Promise<boolean>
    onEditIssue: (uid: string, patch: KataTaskEditPatch) => boolean | Promise<boolean>
    onAssignOwner: (uid: string, owner: string) => boolean | Promise<boolean>
    onClaimAssignment?:
      | ((uid: string, ttlSeconds: number) => boolean | Promise<boolean>)
      | undefined
    onUnassignOwner: (uid: string) => boolean | Promise<boolean>
    onSetPriority: (uid: string, priority: number | null) => boolean | Promise<boolean>
    onAddLabel: (uid: string, label: string) => boolean | Promise<boolean>
    onRemoveLabel: (uid: string, label: string) => void | Promise<void>
    onCloseIssue: (request: KataTaskCloseRequest) => boolean | Promise<boolean>
    onReopenIssue: () => void | Promise<void>
    recurrenceRevision?: number
    onCreateRecurrence: (
      projectID: number,
      input: import('../lib/kata/types').KataCreateRecurrenceInput,
    ) => Promise<void>
    onPatchRecurrence: (
      recurrence: import('../lib/kata/types').KataRecurrence,
      input: import('../lib/kata/types').KataPatchRecurrenceInput,
      etag: string,
    ) => Promise<void>
    onDeleteRecurrence: (recurrence: import('../lib/kata/types').KataRecurrence) => Promise<void>
    onMoveBoardIssue?:
      | ((issue: KataTaskSummary, plan: BoardMovePlan) => Promise<boolean>)
      | undefined
    /** Sends a `kata notify` request from a board card. */
    onAskBoardIssue?:
      | ((issue: KataTaskSummary, to: string, message: string) => Promise<boolean>)
      | undefined
    onCompleteBoardIssue?:
      | ((issue: KataTaskSummary, request: KataTaskCloseRequest) => Promise<boolean>)
      | undefined
    /** Creates an issue in a project from the board's Open column. */
    onCreateBoardIssue?: ((projectID: number, title: string) => Promise<boolean>) | undefined
    /** Loads the next page of the board's closed issues from the server. */
    onShowMoreBoardDone?: (() => void) | undefined
  }

  let {
    syncRefreshGeneration = 0,
    route: acceptedRoute,
    requestedRoute = undefined,
    snapshot,
    selfActor = undefined,
    loading,
    canMutate,
    mutationPending,
    mutationMessage = undefined,
    draftResetGeneration,
    draftFenceGeneration = 0,
    ownerOptions,
    preferences = defaultPreferences,
    daemons = [],
    activeDaemonID = undefined,
    daemonSwitching = false,
    reconnecting = false,
    stale = false,
    readOnly = false,
    daemonError = undefined,
    credentialTokens = [],
    credentialLoading = false,
    credentialError = undefined,
    searchSettingsPostFetch = fetch,
    canAdministerDaemon = false,
    onPreferencesChange = () => {},
    onSelectDaemon = () => {},
    onOpenDaemon = () => {},
    onAccessMutation = undefined,
    onRefreshCredentials = () => {},
    onSearchEmpty = undefined,
    onNavigate,
    onCreateProject,
    onCreateIssue,
    searchReferences,
    onMoveIssue,
    onPatchMetadata,
    onUploadAttachment,
    onAddComment,
    onEditIssue,
    onAssignOwner,
    onClaimAssignment = async () => false,
    onUnassignOwner,
    onSetPriority,
    onAddLabel,
    onRemoveLabel,
    onCloseIssue,
    onReopenIssue,
    recurrenceRevision = 0,
    onCreateRecurrence,
    onPatchRecurrence,
    onDeleteRecurrence,
    onMoveBoardIssue = async () => false,
    onCompleteBoardIssue = async () => false,
    onAskBoardIssue = undefined,
    onShowMoreBoardDone = undefined,
    onCreateBoardIssue = undefined,
  }: Props = $props()

  let acceptedProjection = $derived(normalizeKataUISnapshot(snapshot))
  // A click on another task in the loaded list shows that task at once, from the
  // list's copy of it, while App loads the snapshot that carries its comments,
  // links, and history. Any other route change waits for its snapshot.
  let pendingSelection = $derived.by(() => {
    if (!requestedRoute?.issueUID || requestedRoute.graph) return undefined
    if (
      requestedRoute.view === 'settings' ||
      requestedRoute.view === 'credentials' ||
      acceptedRoute.view === 'settings' ||
      acceptedRoute.view === 'credentials'
    ) {
      return undefined
    }
    if (
      collectionIntentKey(snapshotIntentForRoute(requestedRoute)) !==
      collectionIntentKey(snapshotIntentForRoute(acceptedRoute))
    ) {
      return undefined
    }
    return withPendingSelection(acceptedProjection, requestedRoute.issueUID)
  })
  let route = $derived(pendingSelection && requestedRoute ? requestedRoute : acceptedRoute)
  let projection = $derived(pendingSelection ?? acceptedProjection)

  let connectionNow = $state(Date.now())
  let today = $derived(localDateString(new Date(connectionNow)))
  onMount(() => {
    const timer = setInterval(() => {
      connectionNow = Date.now()
    }, 1000)
    return () => clearInterval(timer)
  })
  let activeConnection = $derived(daemons.find((daemon) => daemon.id === activeDaemonID))
  let credentialExpired = $derived(
    snapshot.capabilities.expires_at !== undefined &&
      Date.parse(snapshot.capabilities.expires_at) <= connectionNow,
  )
  let connectionStatus = $derived(
    daemonSwitching
      ? 'Connecting'
      : activeConnection?.health === 'auth_required' ||
          /authentication required/i.test(daemonError ?? '')
        ? 'Authentication required'
        : credentialExpired
          ? 'Credential expired'
          : reconnecting
            ? 'Reconnecting'
            : stale || daemonError || activeConnection?.health === 'down'
              ? 'Connection unavailable'
              : activeConnection?.health === 'upgrade_required'
                ? 'Upgrade required'
                : readOnly || !snapshot.capabilities.writable
                  ? 'Read-only'
                  : 'Connected',
  )
  const actorlessConnectionLabels: Record<string, string> = {
    none: 'Local, no sign-in',
    web_local: 'Local, no sign-in',
    static_token: 'Shared daemon token',
    bootstrap: 'Bootstrap token',
    insecure_readonly: 'Anonymous (read-only)',
    unauthenticated_private_network: 'Unauthenticated private network',
    trusted_proxy_absent: 'Proxy without account',
  }
  let connectionAccount = $derived(
    daemonSwitching || stale || reconnecting || loading || daemonError || credentialExpired
      ? '—'
      : snapshot.capabilities.account ||
          (snapshot.capabilities.auth_kind === undefined
            ? 'Not reported by hub'
            : actorlessConnectionLabels[snapshot.capabilities.auth_kind] || 'No account'),
  )

  let captureOpen = $state(false)
  let mobileNavigationOpen = $state(false)
  let detailBeside = $state(false)
  let creationIntent = $state<'project' | 'task' | null>(null)
  const mediumScreen = new MediaQuery(MEDIA.medium)
  let linkFilters = $state(createKataLinkFilters('all'))
  let navigationGeneration = $state(0)
  let viewer = $derived(attentionViewer(snapshot.capabilities.account))
  let daemonRoute = $derived(route.view === 'credentials' || route.view === 'settings')
  let settingsHost = $derived.by(() => {
    const remote = activeConnection && !activeConnection.local ? activeConnection.url : undefined
    const host = remote ? new URL(remote).hostname : globalThis.location?.hostname
    return host && !loopbackHost(host) ? host : undefined
  })
  let accessAdministrationAvailable = $derived(
    snapshot.capabilities.access_admin === true &&
      !credentialExpired &&
      !stale &&
      !reconnecting &&
      !daemonError &&
      !daemonSwitching &&
      (!activeDaemonID || activeConnection?.local === true),
  )
  let credentialAuditAvailable = $derived(snapshot.capabilities.token_audit_read === true)
  // Settings opens on Search by meaning. A section this session can't use
  // shows Appearance instead, without reading anything it isn't allowed to.
  let settingsSection = $derived.by<SettingsSectionID>(() => {
    if (route.view === 'credentials') return credentialAuditAvailable ? 'credentials' : 'appearance'
    const section = route.section ?? 'search'
    return section === 'access' && !accessAdministrationAvailable ? 'appearance' : section
  })
  let inboxProject = $derived(
    projection.projects.find((project) => project.metadata.role === 'inbox'),
  )
  let viewName = $derived(viewNameForRoute(route))
  let searchFilters = $derived(searchFiltersForRoute(route, viewName))
  let filterRoute = $derived(
    requestedRoute &&
      requestedRoute.view !== 'settings' &&
      requestedRoute.view !== 'credentials' &&
      viewNameForRoute(requestedRoute) === viewName &&
      requestedRoute.projectUID === route.projectUID
      ? requestedRoute
      : route,
  )
  let inputFilters = $derived(searchFiltersForRoute(filterRoute, viewName))
  let currentView = $derived(
    projectKataWorkspaceView({
      view: viewName,
      filters: searchFilters,
      snapshot: projection,
      issues: projection.issues,
      today,
      viewer,
    }),
  )
  let pendingQuery = $state('')
  let countedQuery = $state('')
  function submitSearch(query: string): void {
    query = query.trim()
    if (!query || query === countedQuery) return
    pendingQuery = query
  }
  $effect(() => {
    if (loading || stale) return
    const query = searchFilters.query.trim()
    if (query !== countedQuery) countedQuery = ''
    if (!pendingQuery || query !== pendingQuery) return
    if (currentView.groups.every((group) => group.issues.length === 0)) {
      countedQuery = pendingQuery
      onSearchEmpty?.()
    }
    pendingQuery = ''
  })
  let areas = $derived(deriveKataAreas(projection.projects))
  let selectedProject = $derived.by(() => {
    const scope = searchFilters.scope
    return scope.kind === 'project'
      ? projection.projects.find((project) => project.uid === scope.project_uid)
      : undefined
  })
  let boardRoute = $derived(route.board === true && !daemonRoute)
  let boardIssues = $derived(
    selectedProject
      ? projection.issues.filter((issue) => issue.project_uid === selectedProject!.uid)
      : projection.issues,
  )
  let projectView = $derived(Boolean(route.projectUID && !route.view))
  let selectedCommentUID = $state<string | undefined>()
  let returnComment = $state<{ issueUID: string; commentUID: string } | undefined>()
  let selectedIssueUID = $derived(route.issueUID ?? null)
  let scopedAuthority = $derived(snapshot.capabilities.scope !== undefined)
  let projectWideMutationAllowed = $derived(canMutate && !scopedAuthority)
  let childParent = $derived.by<ChildParent | undefined>(() => {
    const issue = projection.selected_detail?.issue
    if (!issue) return undefined
    return {
      projectID: issue.project_id,
      projectUID: issue.project_uid,
      issueUID: issue.uid,
    }
  })
  const controlID = $props.id()
  const newTaskReasonID = `${controlID}-new-task-reason`
  const createProjectReasonID = `${controlID}-create-project-reason`
  let writeBlockedReason = $derived(
    canMutate
      ? undefined
      : stale
        ? 'Editing is paused while workspace data may be out of date.'
        : readOnly || !snapshot.capabilities.writable
          ? 'This credential is read-only.'
          : 'Changes are paused while Kata connects.',
  )
  let pendingReason = $derived(
    mutationPending ? 'Wait for the current change to finish.' : undefined,
  )
  let newTaskBlockedReason = $derived(
    writeBlockedReason ??
      (scopedAuthority && childParent === undefined
        ? 'Open a task first. This credential can only add subtasks.'
        : pendingReason),
  )
  let projectCreationBlockedReason = $derived(
    writeBlockedReason ??
      (scopedAuthority ? 'This credential cannot create projects.' : pendingReason),
  )

  function toggleBoardColumn(columnID: BoardColumnID): void {
    const collapsed = preferences.collapsedBoardColumns.includes(columnID)
    onPreferencesChange({
      ...preferences,
      collapsedBoardColumns: collapsed
        ? preferences.collapsedBoardColumns.filter((id) => id !== columnID)
        : [...preferences.collapsedBoardColumns, columnID],
    })
  }

  function navigate(next: AppRoute): void {
    const query = (next.filters.text ?? '').trim()
    if (query !== countedQuery) countedQuery = ''
    if (query !== pendingQuery) pendingQuery = ''
    navigationGeneration += 1
    void onNavigate(next)
  }

  // Closing the drawer drops the form it showed, as unmounting its sidebar did.
  function closeNavigation(): void {
    if (!mobileNavigationOpen) return
    mobileNavigationOpen = false
    creationIntent = null
  }

  $effect(() => {
    if (!mediumScreen.current) closeNavigation()
  })

  function openView(name: KataTaskViewName): void {
    closeNavigation()
    navigate({
      kind: 'kata',
      view: systemViewName(name),
      graph: false,
      // The board spans a project or all open work, not the dated system views.
      ...(route.board && name === 'all' ? { board: true as const } : {}),
      filters: emptyShareableFilters(),
    })
  }

  function openProject(projectUID: string): void {
    closeNavigation()
    navigate({
      kind: 'kata',
      projectUID,
      graph: false,
      ...(route.board ? { board: true as const } : {}),
      filters: emptyShareableFilters(),
    })
  }

  function openSettings(): void {
    closeNavigation()
    navigate({
      kind: 'kata',
      view: 'settings',
      graph: false,
      filters: emptyShareableFilters(),
    })
  }

  // Credentials keeps its own address; every other section is a Settings one.
  function openSettingsSection(section: SettingsSectionID): void {
    if (
      section === settingsSection &&
      (section === 'credentials'
        ? route.view === 'credentials'
        : route.view === 'settings' && (route.section ?? 'search') === section)
    )
      return
    navigate({
      kind: 'kata',
      ...(section === 'credentials'
        ? { view: 'credentials' as const }
        : { view: 'settings' as const, section: section as SettingsSection }),
      graph: false,
      filters: emptyShareableFilters(),
    })
  }

  function loopbackHost(host: string): boolean {
    return host === 'localhost' || host === '[::1]' || /^127\./.test(host)
  }

  // The landing route has no view and means Inbox, but adding a scope or issue
  // to it would read as All Open. Pin Inbox so these moves stay in Inbox.
  function withResolvedView(current: AppRoute): AppRoute {
    if (current.view || viewName !== 'inbox') return current
    return { ...current, view: 'inbox' }
  }

  function updateFilters(
    filters: KataTaskSearchFilters,
    changed: keyof KataTaskSearchFilters,
  ): void {
    const shareable = shareableFilters(filters, viewName, changed)
    if (filters.scope.kind === 'project') {
      navigate({
        ...withResolvedView(filterRoute),
        projectUID: filters.scope.project_uid,
        filters: shareable,
      })
      return
    }
    const next = { ...filterRoute, filters: shareable }
    delete next.projectUID
    navigate(next)
  }

  function selectIssue(issueUID: string): void {
    statusRequest = null
    navigate({
      ...withResolvedView(filterRoute),
      issueUID,
      graph: false,
    })
  }

  let statusRequest = $state<{ uid: string; generation: number } | null>(null)
  let statusRequestGeneration = 0

  $effect(() => {
    if (!canMutate || (requestedRoute && requestedRoute.issueUID !== statusRequest?.uid))
      statusRequest = null
  })

  function openGraph(issueUID: string): void {
    const next: AppRoute = {
      ...withResolvedView(filterRoute),
      issueUID,
      graph: true,
    }
    delete next.board
    navigate(next)
  }

  function closeIssueDrawer(): void {
    const next = { ...route }
    delete next.issueUID
    navigate(next)
  }

  function setLayout(layout: string): void {
    const next: AppRoute = { ...route, graph: false }
    if (layout === 'board') {
      next.board = true
      // The board covers a project or all open work; dated views fall back to All Open.
      if (next.view && next.view !== 'all-open' && !next.projectUID) next.view = 'all-open'
      else if (next.view && next.projectUID) delete next.view
    } else {
      delete next.board
    }
    // Status filters mean different defaults in each layout.
    navigate({ ...next, filters: { ...next.filters, status: [] } })
  }

  function closeGraph(): void {
    if (!route.issueUID) return
    navigate({ ...route, graph: false })
  }

  function toggleSplitDirection(): void {
    onPreferencesChange({
      ...preferences,
      splitDirection: preferences.splitDirection === 'vertical' ? 'horizontal' : 'vertical',
    })
  }

  function setSplitDirection(direction: 'horizontal' | 'vertical'): void {
    onPreferencesChange({ ...preferences, splitDirection: direction })
  }

  // A drag shows its width at once and saves it when the pointer lets go.
  let sidebarDragWidth = $state<number | null>(null)
  let sidebarWidth = $derived(sidebarDragWidth ?? preferences.sidebarWidth)
  let sidebarResizeStart = 0

  function sidebarWidthAfter(event: SplitResizeEvent): number {
    return Math.max(minSidebarWidth, Math.min(maxSidebarWidth, sidebarResizeStart + event.delta))
  }

  function finishSidebarResize(event: SplitResizeEvent): void {
    onPreferencesChange({ ...preferences, sidebarWidth: sidebarWidthAfter(event) })
    sidebarDragWidth = null
  }

  // The toggle a person pressed disappears, so focus moves to the one that replaces it.
  let navigationElement: HTMLDivElement | null = $state(null)
  let sidebarToggleFocus = $state<boolean | null>(null)

  function setSidebarCollapsed(collapsed: boolean): void {
    sidebarToggleFocus = collapsed
    onPreferencesChange({ ...preferences, sidebarCollapsed: collapsed })
  }

  $effect(() => {
    if (sidebarToggleFocus === null || preferences.sidebarCollapsed !== sidebarToggleFocus) return
    const pane = sidebarToggleFocus ? '.navigation-strip' : '.navigation-pane'
    navigationElement?.querySelector<HTMLElement>(`${pane} .kit-sidebar-toggle`)?.focus()
    sidebarToggleFocus = null
  })

  function resizeSplit(size: number): void {
    if (route.graph) graphSize = size
    else onPreferencesChange({ ...preferences, splitSize: size })
  }

  let graphSize = $state<number | null>(null)
  $effect(() => {
    if (!route.graph) graphSize = null
  })
  $effect(() => {
    void detailBeside
    graphSize = null
  })

  function beginNewTask(): void {
    if (newTaskBlockedReason) return
    closeNavigation()
    if (scopedAuthority) {
      captureOpen = true
      return
    }
    if (projection.projects.length === 0) {
      beginProjectCreation('task')
      return
    }
    captureOpen = true
  }

  function beginProjectCreation(intent: 'project' | 'task' = 'project'): void {
    if (projectCreationBlockedReason) return
    if (mediumScreen.current) mobileNavigationOpen = true
    else if (preferences.sidebarCollapsed)
      onPreferencesChange({ ...preferences, sidebarCollapsed: false })
    creationIntent = intent
  }

  async function createProject(name: string): Promise<KataTaskMutationResponse> {
    const task = creationIntent === 'task'
    const result = await onCreateProject(name, task)
    if (task && creationIntent === 'task') {
      mobileNavigationOpen = false
      captureOpen = true
    }
    return result
  }

  function submitCapture(title: string, projectUID?: string): void | Promise<void> {
    return scopedAuthority && childParent
      ? onCreateIssue(title, childParent)
      : onCreateIssue(title, undefined, projectUID)
  }

  function viewNameForRoute(current: AppRoute): KataTaskViewName {
    if (current.view === 'credentials' || current.view === 'settings') return 'all'
    // The board spans a project or all open work, never a dated system view.
    if (current.board) return 'all'
    if (current.view) return current.view === 'all-open' ? 'all' : current.view
    return current.projectUID || current.issueUID ? 'all' : 'inbox'
  }

  function systemViewName(name: KataTaskViewName): SystemView {
    return name === 'all' ? 'all-open' : name
  }

  function searchFiltersForRoute(
    current: AppRoute,
    currentView: KataTaskViewName,
  ): KataTaskSearchFilters {
    const defaults = defaultKataTaskSearchFilters(currentView)
    if (current.board) defaults.status = 'all'
    return {
      scope: current.projectUID
        ? { kind: 'project', project_uid: current.projectUID }
        : { kind: 'all' },
      status: statusFilter(current.filters.status, defaults.status),
      owner: current.filters.owner.length === 1 ? current.filters.owner[0]! : '',
      label: current.filters.label.length === 1 ? current.filters.label[0]! : '',
      query: current.filters.text ?? '',
      relationships: [...current.filters.relationship],
    }
  }

  function statusFilter(
    values: readonly string[],
    fallback: KataTaskStatusFilter,
  ): KataTaskStatusFilter {
    if (values.length !== 1) return values.length === 0 ? fallback : 'all'
    const value = values[0]
    return value === 'open' || value === 'ready' || value === 'closed' || value === 'all'
      ? value
      : 'all'
  }

  function shareableFilters(
    filters: KataTaskSearchFilters,
    currentView: KataTaskViewName,
    changed: keyof KataTaskSearchFilters,
  ): ShareableFilters {
    const defaultStatus = route.board ? 'all' : defaultKataTaskSearchFilters(currentView).status
    const result: ShareableFilters = {
      status:
        changed === 'status'
          ? filters.status === defaultStatus
            ? []
            : [filters.status]
          : [...filterRoute.filters.status],
      owner:
        changed === 'owner'
          ? filters.owner.trim()
            ? [filters.owner.trim()]
            : []
          : [...filterRoute.filters.owner],
      label:
        changed === 'label'
          ? filters.label.trim()
            ? [filters.label.trim()]
            : []
          : [...filterRoute.filters.label],
      relationship: [...(filters.relationships ?? [])],
    }
    if (filters.query.trim()) result.text = filters.query.trim()
    return result
  }

  function emptyShareableFilters(): ShareableFilters {
    return { status: [], owner: [], label: [], relationship: [] }
  }

  function scopeLabel(): string {
    if (selectedProject) return selectedProject.name
    return viewLabel()
  }

  function viewLabel(): string {
    return (
      [
        { name: 'inbox', label: 'Inbox' },
        { name: 'today', label: 'Today' },
        { name: 'delegated', label: 'Delegated' },
        { name: 'scheduled', label: 'Scheduled' },
        { name: 'attention', label: 'Needs attention' },
        { name: 'all', label: 'All open' },
        { name: 'logbook', label: 'Logbook' },
      ].find((view) => view.name === viewName)?.label ?? 'Kata'
    )
  }

  function tabTitle(): string {
    if (route.view === 'settings') return 'Settings'
    if (route.view === 'credentials') return 'Credentials'
    if (route.issueUID) {
      if (projection.selected_state === 'archived') return `Archived issue ${route.issueUID}`
      if (projection.selected_state === 'missing') return `Issue unavailable ${route.issueUID}`
      const issue = projection.selected_detail?.issue
      if (issue?.uid === route.issueUID) return `${issue.title} (${issue.qualified_id})`
      return 'Issue'
    }
    if (selectedProject && viewName !== 'all') return `${viewLabel()} · ${selectedProject.name}`
    return scopeLabel()
  }
</script>

<svelte:head>
  <title>{tabTitle()} · Kata</title>
</svelte:head>

{#snippet newTaskButton()}
  <!-- aria-disabled keeps the blocked button focusable so keyboard users reach its reason. -->
  <button
    type="button"
    class="quiet-button header-action"
    aria-disabled={newTaskBlockedReason ? 'true' : undefined}
    aria-describedby={newTaskBlockedReason ? newTaskReasonID : undefined}
    title={newTaskBlockedReason ? undefined : 'New task'}
    onclick={beginNewTask}
  >
    <PlusIcon size={16} aria-hidden="true" />
    <span class="kit-sr-only">New task</span>
  </button>
{/snippet}

{#snippet createProjectButton()}
  <!-- aria-disabled keeps the blocked button focusable so keyboard users reach its reason. -->
  <button
    type="button"
    class="accent-button"
    aria-disabled={projectCreationBlockedReason ? 'true' : undefined}
    aria-describedby={projectCreationBlockedReason ? createProjectReasonID : undefined}
    onclick={() => beginProjectCreation()}
  >
    New project
  </button>
{/snippet}

{#snippet noProjects()}
  <EmptyState title="No projects yet" class="no-projects">
    {#if projectCreationBlockedReason}
      <Tooltip text={projectCreationBlockedReason}>
        {@render createProjectButton()}
      </Tooltip>
      <span id={createProjectReasonID} class="kit-sr-only">{projectCreationBlockedReason}</span>
    {:else}
      {@render createProjectButton()}
    {/if}
    <p class="no-projects-hint">Or run <code>kata init</code> in a repository.</p>
  </EmptyState>
{/snippet}

{#snippet navigationSidebar(collapsible: boolean)}
  <Sidebar
    {areas}
    currentView={{
      name: currentView.view,
      groups: currentView.groups,
      fetched_at: currentView.fetched_at,
    }}
    {searchFilters}
    {projectCreationBlockedReason}
    bind:creationIntent
    {draftFenceGeneration}
    settingsActive={daemonRoute}
    onOpenSettings={openSettings}
    onOpenView={openView}
    onOpenProject={openProject}
    onCreateProject={createProject}
  >
    {#snippet header()}
      <div class="header-server">
        {#if daemons.length > 0}
          <KataDaemonSwitcher
            {daemons}
            account={connectionAccount}
            credential={connectionStatus === 'Connected' ? undefined : connectionStatus}
            activeId={activeDaemonID}
            activeStatusLabel={daemonError ?? (reconnecting ? 'Reconnecting…' : undefined)}
            activeStatusTone={daemonError ? 'error' : undefined}
            disabled={daemonSwitching || mutationPending}
            onSelect={onSelectDaemon}
            onOpen={onOpenDaemon}
          />
        {:else}
          <strong>Kata</strong>
          {#if daemonError || reconnecting}<span class="daemon-fallback-status" role="status"
              >{daemonError ?? 'Reconnecting…'}</span
            >{/if}
        {/if}
      </div>
      <div class="header-task">
        {#if newTaskBlockedReason}
          <Tooltip text={newTaskBlockedReason}>{@render newTaskButton()}</Tooltip>
        {:else}
          {@render newTaskButton()}
        {/if}
        {#if collapsible}
          <SidebarToggle state="expanded" onclick={() => setSidebarCollapsed(true)} />
        {/if}
      </div>
    {/snippet}
  </Sidebar>
{/snippet}

<section class="kata-feature" aria-label="Kata workspace">
  {#if newTaskBlockedReason}
    <span id={newTaskReasonID} class="kit-sr-only">{newTaskBlockedReason}</span>
  {/if}
  {#if connectionStatus !== 'Connected'}
    <div class="active-connection" role="status" aria-label="Active connection">
      <span
        >Hub: <strong>{activeDaemonID ?? (globalThis.location?.host || 'Local daemon')}</strong
        ></span
      >
      <span>Account: <strong>{connectionAccount}</strong></span>
      <span>Credential: <strong>{connectionStatus}</strong></span>
      {#if stale}
        <span>Workspace data may be out of date. Editing is paused.</span>
      {/if}
    </div>
  {/if}
  <div class="mobile-navigation-trigger">
    <TopBar ariaLabel="Kata workspace">
      {#snippet left()}
        <IconButton
          ariaLabel="Open navigation"
          onclick={() => {
            mobileNavigationOpen = true
          }}><MenuIcon size={18} /></IconButton
        >
        <strong>{daemonRoute ? 'Settings' : scopeLabel()}</strong>
      {/snippet}
      {#snippet right()}
        {#if mediumScreen.current}
          {#if newTaskBlockedReason}
            <Tooltip text={newTaskBlockedReason}>{@render newTaskButton()}</Tooltip>
          {:else}
            {@render newTaskButton()}
          {/if}
        {/if}
      {/snippet}
    </TopBar>
  </div>
  <div class="kata-layout" aria-busy={loading}>
    <div class="desktop-navigation" bind:this={navigationElement}>
      <!-- Hidden rather than unmounted, so collapsing keeps the sidebar's own state. -->
      <div
        class="navigation-pane"
        hidden={preferences.sidebarCollapsed}
        style:width={`${sidebarWidth}px`}
      >
        {@render navigationSidebar(true)}
      </div>
      {#if preferences.sidebarCollapsed}
        <div class="navigation-strip">
          <SidebarToggle
            state="collapsed"
            class="kit-sidebar-toggle--compact"
            onclick={() => setSidebarCollapsed(false)}
          />
        </div>
      {:else}
        <SplitResizeHandle
          ariaLabel="Resize sidebar"
          orientation="horizontal"
          ariaValueMin={minSidebarWidth}
          ariaValueMax={maxSidebarWidth}
          ariaValueNow={sidebarWidth}
          onResizeStart={() => (sidebarResizeStart = sidebarWidth)}
          onResize={(event) => (sidebarDragWidth = sidebarWidthAfter(event))}
          onResizeEnd={finishSidebarResize}
        />
      {/if}
    </div>
    <div class="kata-main">
      {#if mutationMessage && !captureOpen && (settingsSection !== 'access' || !daemonRoute || !accessAdministrationAvailable || !onAccessMutation)}
        <p class="mutation-message" role="alert">{mutationMessage}</p>
      {/if}
      {#if daemonRoute}
        <SettingsView
          active={settingsSection}
          theme={preferences.theme}
          onThemeChange={(theme) => onPreferencesChange({ ...preferences, theme })}
          credentialsAvailable={credentialAuditAvailable}
          accessAvailable={accessAdministrationAvailable}
          onSelect={openSettingsSection}
        >
          {#snippet search()}
            {#key activeDaemonID ?? 'direct'}
              <SearchSettings
                postFetch={searchSettingsPostFetch}
                {canAdministerDaemon}
                originStable={snapshot.origin_stable}
                remoteHost={settingsHost}
              />
            {/key}
          {/snippet}
          {#snippet credentials()}
            <CredentialAudit
              tokens={credentialTokens}
              loading={credentialLoading}
              error={credentialError}
              onRefresh={onRefreshCredentials}
            />
          {/snippet}
          {#snippet access()}
            {#if onAccessMutation}
              {#key `${activeDaemonID ?? 'direct'}:${draftFenceGeneration}`}
                <AccessManagement
                  projects={snapshot.catalog}
                  pending={mutationPending || loading || !accessAdministrationAvailable}
                  message={captureOpen ? undefined : mutationMessage}
                  onMutation={onAccessMutation}
                />
              {/key}
            {/if}
          {/snippet}
        </SettingsView>
      {:else if boardRoute}
        {@render boardPane()}
      {:else}
        <SplitLayout
          orientation={preferences.splitDirection}
          primarySize={route.graph ? graphSize : preferences.splitSize}
          defaultShare={route.graph ? 2 / 3 : undefined}
          minPrimary={preferences.splitDirection === 'vertical' ? 220 : 320}
          minSecondary={preferences.splitDirection === 'vertical' ? 220 : 360}
          responsiveBreakpoint={700}
          ariaLabel="Resize Kata panes"
          onResize={resizeSplit}
          bind:secondaryBeside={detailBeside}
          primary={listPane}
          secondary={route.issueUID ? detailPane : undefined}
        />
      {/if}
    </div>
  </div>
</section>

{#if boardRoute && route.issueUID}
  <DetailDrawer
    title={projection.selected_detail?.issue.title ?? 'Issue'}
    ariaLabel="Issue detail"
    width="min(780px, 100vw)"
    onclose={closeIssueDrawer}
  >
    <div class="drawer-detail">
      {@render detailPane()}
    </div>
  </DetailDrawer>
{/if}

{#if mobileNavigationOpen}
  <DetailDrawer title="Navigation" width="min(320px, calc(100vw - 40px))" onclose={closeNavigation}>
    {@render navigationSidebar(false)}
  </DetailDrawer>
{/if}

{#snippet paneDirection()}
  <IconButton
    ariaLabel={preferences.splitDirection === 'vertical'
      ? 'Switch to side-by-side layout'
      : 'Switch to stacked layout'}
    onclick={toggleSplitDirection}
  >
    {#if preferences.splitDirection === 'vertical'}<LayoutPanelLeftIcon
        size={15}
      />{:else}<LayoutPanelTopIcon size={15} />{/if}
  </IconButton>
{/snippet}

{#snippet projectSync()}
  {#if selectedProject && !daemonRoute && !stale && !daemonSwitching && !reconnecting && !credentialExpired && !daemonError && !scopedAuthority && (!activeDaemonID || activeConnection?.local === true)}
    {#key `${activeDaemonID ?? 'direct'}:${draftFenceGeneration}:${selectedProject.uid}`}
      <ProjectSyncStatus
        projectID={selectedProject.id}
        projectUID={selectedProject.uid}
        refreshGeneration={syncRefreshGeneration}
      />
    {/key}
  {/if}
{/snippet}

{#snippet listPane()}
  <div class="list-column kata-list">
    {#if route.issueUID && route.graph}
      {#if projection.selected_graph && projection.selected_detail}
        {#snippet graphActions()}
          {@render projectSync()}
          {@render paneDirection()}
        {/snippet}
        <IssueGraph
          headerActions={graphActions}
          graph={projection.selected_graph}
          sourceIssue={projection.selected_detail.issue}
          layoutDirection={preferences.splitDirection === 'horizontal' ? 'LR' : 'TB'}
          onBack={closeGraph}
          onSelectIssue={(uid) => {
            if (uid !== route.issueUID) openGraph(uid)
          }}
        />
      {:else}
        <section class="detail-unavailable" role="status" aria-label="Selected issue status">
          The reachable graph is unavailable from the current authority.
        </section>
      {/if}
    {:else}
      {#snippet filtersToolbar()}
        <IssueFilters
          filters={inputFilters}
          projects={projection.projects}
          {projectView}
          showStatus={currentView.view !== 'logbook'}
          onChange={updateFilters}
          onSubmitSearch={submitSearch}
        />
      {/snippet}
      {#snippet projectRecurrences()}
        {@render projectSync()}
        {#if selectedProject && !scopedAuthority}
          {#key selectedProject.uid}
            <ProjectRecurrences
              projectID={selectedProject.id}
              actor={selfActor ?? 'kata-web'}
              disabled={!projectWideMutationAllowed || mutationPending}
              {draftFenceGeneration}
              refreshKey={`${recurrenceRevision}:${draftResetGeneration}`}
              onCreate={onCreateRecurrence}
              onPatch={onPatchRecurrence}
              onDelete={onDeleteRecurrence}
            />
          {/key}
        {/if}
      {/snippet}
      <IssueCollection
        {viewer}
        toolbar={filtersToolbar}
        headerAccessory={projectRecurrences}
        {layoutControl}
        now={connectionNow}
        resultsKey={JSON.stringify([route.projectUID ?? null, route.filters])}
        {navigationGeneration}
        currentView={{
          name: currentView.view,
          groups: currentView.groups,
          fetched_at: currentView.fetched_at,
        }}
        issueCatalog={projection.issues}
        scopeLabel={scopeLabel()}
        scopedProjectName={selectedProject?.name ?? null}
        {projectView}
        {detailBeside}
        splitDirection={preferences.splitDirection}
        onSplitDirectionChange={setSplitDirection}
        {selectedIssueUID}
        {loading}
        statusFilter={searchFilters.status}
        readyIssueUIDs={projection.member_issue_uid_set}
        onSelect={(issue) => selectIssue(issue.uid)}
        onStatusAction={canMutate && !mutationPending
          ? (issue) => {
              selectIssue(issue.uid)
              statusRequest = { uid: issue.uid, generation: ++statusRequestGeneration }
            }
          : undefined}
        onOpenGraph={(issue) => openGraph(issue.uid)}
        empty={!loading && projection.projects.length === 0 ? noProjects : undefined}
      />
    {/if}
  </div>
{/snippet}

{#snippet layoutControl()}
  <SegmentedControl
    class="layout-toggle"
    ariaLabel="Layout"
    value={boardRoute ? 'board' : 'list'}
    options={[
      { value: 'list', label: 'List', title: 'List and detail' },
      { value: 'board', label: 'Board', title: 'Columns of cards' },
    ]}
    onchange={setLayout}
  >
    {#snippet segment(option)}
      <span class="layout-segment">
        {#if option.value === 'board'}
          <KanbanIcon size={14} strokeWidth={1.8} aria-hidden="true" />
        {:else}
          <ListIcon size={14} strokeWidth={1.8} aria-hidden="true" />
        {/if}
        <span class="layout-segment-label">{option.label}</span>
      </span>
    {/snippet}
  </SegmentedControl>
{/snippet}

{#snippet boardPane()}
  <div class="list-column board-pane">
    <!-- The same header as the list, so switching layouts keeps every control in place. -->
    <div class="board-header">
      <PageHeader level={2} title={scopeLabel()}>
        {#snippet actions()}
          {@render layoutControl()}
          <DisplayMenu>
            <div class="display-menu__section">
              <div class="display-menu__title">Cards</div>
              <SegmentedControl
                ariaLabel="Cards"
                options={[
                  { value: 'comfortable', label: 'Comfortable' },
                  { value: 'compact', label: 'Compact' },
                ]}
                value={preferences.boardCompact ? 'compact' : 'comfortable'}
                onchange={(value) =>
                  onPreferencesChange({ ...preferences, boardCompact: value === 'compact' })}
                block
              />
            </div>
            <div class="display-menu__section">
              <Checkbox
                checked={preferences.boardHighlightRemote}
                label="Highlight changes by others"
                onchange={(checked) =>
                  onPreferencesChange({ ...preferences, boardHighlightRemote: checked })}
              />
            </div>
          </DisplayMenu>
        {/snippet}
      </PageHeader>
    </div>
    <IssueFilters
      filters={inputFilters}
      projects={projection.projects}
      {projectView}
      onChange={updateFilters}
      onSubmitSearch={submitSearch}
    />
    <!-- A board belongs to one project: switching projects drops half-typed
         forms, so a card typed for one project is never created in another. -->
    {#key selectedProject?.uid ?? 'all'}
      <IssueBoard
        issues={boardIssues}
        {selectedIssueUID}
        showProject={!selectedProject}
        {loading}
        moveBlockedReason={writeBlockedReason ??
          (scopedAuthority ? 'This credential can only change one task.' : undefined)}
        onOpenIssue={(issue) => selectIssue(issue.uid)}
        onMoveIssue={onMoveBoardIssue}
        onCompleteIssue={onCompleteBoardIssue}
        onAskIssue={onAskBoardIssue}
        {viewer}
        doneTotal={snapshot.closed_total}
        onShowMoreDone={onShowMoreBoardDone}
        onOpenLogbook={() => openView('logbook')}
        busy={mutationPending}
        {draftFenceGeneration}
        compact={preferences.boardCompact}
        highlightRemoteChanges={preferences.boardHighlightRemote}
        scopeKey={`${selectedProject?.uid ?? 'all'}|${JSON.stringify(route.filters)}`}
        onAddCard={selectedProject &&
        onCreateBoardIssue &&
        !readOnly &&
        snapshot.capabilities.writable &&
        !scopedAuthority
          ? (title) => onCreateBoardIssue(selectedProject!.id, title)
          : undefined}
        collapsedColumns={preferences.collapsedBoardColumns}
        onToggleColumn={toggleBoardColumn}
      />
    {/key}
  </div>
{/snippet}

{#snippet detailPane()}
  <div class="detail-column">
    {#if projection.selected_state === 'available' && projection.selected_detail}
      <IssueDetail
        issue={projection.selected_detail}
        {today}
        {selfActor}
        events={[...projection.selected_history]}
        issueCatalog={projection.issues}
        {searchReferences}
        {linkFilters}
        onLinkFiltersChange={(next) => {
          linkFilters = next
        }}
        projects={[...projection.projects]}
        {ownerOptions}
        selectedRecurrences={[...projection.selected_recurrences]}
        actionsDisabled={!canMutate || mutationPending || projection.selected_pending === true}
        authorityBlocked={stale || projection.selected_pending === true}
        pending={projection.selected_pending === true}
        {statusRequest}
        onStatusRequestConsumed={() => {
          statusRequest = null
        }}
        graphOpen={route.graph}
        onCloseGraph={closeGraph}
        attachmentsEnabled={Boolean(snapshot.capabilities.attachments)}
        {onUploadAttachment}
        administrativeActionsAllowed={!scopedAuthority}
        {draftResetGeneration}
        {draftFenceGeneration}
        movePending={mutationPending}
        {onMoveIssue}
        {onPatchMetadata}
        {onAddComment}
        mutationError={captureOpen ? undefined : mutationMessage}
        {selectedCommentUID}
        {returnComment}
        {onEditIssue}
        {onAssignOwner}
        {onClaimAssignment}
        {onUnassignOwner}
        {onSetPriority}
        {onAddLabel}
        {onRemoveLabel}
        {onCloseIssue}
        {onReopenIssue}
        onSelectIssue={(target) => {
          returnComment = target.returnComment
          selectedCommentUID = target.commentUID
          selectIssue(target.uid)
        }}
        onOpenGraph={(issue) => openGraph(issue.uid)}
        {onCreateRecurrence}
      />
    {:else}
      <section class="detail-unavailable" role="status" aria-label="Selected issue status">
        {projection.selected_state === 'archived'
          ? 'This issue is archived.'
          : 'This issue is unavailable from the current authority.'}
      </section>
    {/if}
  </div>
{/snippet}

<QuickCapture
  open={captureOpen}
  disabled={newTaskBlockedReason !== undefined}
  {draftFenceGeneration}
  projects={projectWideMutationAllowed
    ? projection.projects.map((project) => ({ uid: project.uid, name: project.name }))
    : undefined}
  inboxUID={inboxProject?.uid}
  onClose={() => {
    captureOpen = false
  }}
  onSubmit={submitCapture}
/>

<style>
  .kata-feature {
    height: 100%;
    min-height: 0;
    background: var(--bg-primary);
    color: var(--text-primary);
    display: flex;
    flex-direction: column;
    position: relative;
  }

  .active-connection {
    display: flex;
    flex-wrap: wrap;
    gap: 0.25rem 1rem;
    padding: 0.375rem 0.75rem;
    border-start-start-radius: 0;
    border-end-start-radius: 0;
    border-inline-start: 0.25rem solid var(--accent-amber);
    background: color-mix(in srgb, var(--accent-amber) 15%, var(--bg-surface));
    color: var(--text-primary);
    font-size: var(--font-size-xs);
    overflow-wrap: anywhere;
  }

  :global(.no-projects .kit-empty-state__title) {
    color: var(--text-secondary);
    line-height: 1.5;
  }

  /* The kata init line sits under New project, not beside it. */
  :global(.no-projects .kit-empty-state__actions) {
    flex-direction: column;
    align-items: center;
    gap: var(--space-4);
  }
  .no-projects-hint {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .daemon-fallback-status {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    white-space: nowrap;
  }

  .quiet-button.header-action {
    display: inline-flex;
    flex: 0 0 auto;
    align-items: center;
    justify-content: center;
    width: 28px;
    height: 28px;
    padding: 0;
    border: 1px solid transparent;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-secondary);
    cursor: pointer;
  }

  .quiet-button.header-action:hover:not([aria-disabled='true']) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .quiet-button[aria-disabled='true'] {
    color: var(--text-muted);
    cursor: not-allowed;
  }

  .accent-button {
    border: 1px solid var(--accent-blue);
    border-radius: var(--radius-md);
    background: var(--accent-blue);
    color: var(--bg-surface);
    height: 28px;
    padding: 0 var(--space-5);
    font: inherit;
    font-size: var(--font-size-md);
    font-weight: 600;
    line-height: 16px;
  }

  .accent-button:hover:not([aria-disabled='true']) {
    filter: brightness(1.08);
  }

  .accent-button[aria-disabled='true'] {
    opacity: var(--opacity-disabled);
    cursor: not-allowed;
  }

  .kata-layout {
    min-height: 0;
    flex: 1;
    display: flex;
  }

  .desktop-navigation {
    flex: none;
    min-height: 0;
    display: flex;
  }

  .navigation-pane {
    min-width: 0;
    min-height: 0;
    display: flex;
  }

  .navigation-pane[hidden] {
    display: none;
  }

  .navigation-strip {
    width: 28px;
    display: flex;
    justify-content: center;
    padding-top: var(--space-3);
    border-right: 1px solid var(--border-muted);
    background: var(--bg-primary);
  }

  .header-task {
    display: flex;
    align-items: center;
    gap: var(--space-1);
  }

  /* The slim bar and the drawer's header take the sidebar header's 48px. */
  .mobile-navigation-trigger {
    display: none;
    --header-height: var(--layout-header-h);
  }

  :global(.kit-detail-drawer:has(.kata-sidebar) > .kit-detail-drawer__header) {
    box-sizing: border-box;
    height: var(--layout-header-h);
    padding-block: 0;
  }

  .kata-main {
    flex: 1;
    min-width: 0;
    min-height: 0;
    display: flex;
    position: relative;
    overflow: hidden;
  }

  .list-column {
    min-width: 0;
    min-height: 0;
    display: flex;
    flex: 1 1 auto;
    flex-direction: column;
    overflow: hidden;
    background: var(--bg-surface);
    container-type: inline-size;
    container-name: list-pane;
  }

  .board-pane {
    background: var(--bg-primary);
  }

  .board-header {
    display: flex;
    align-items: center;
    flex-shrink: 0;
    min-height: var(--layout-header-h);
    padding: 16px 12px 8px;
    background: var(--bg-surface);
  }

  .board-header > :global(.kit-page-header) {
    flex: 1;
    align-items: center;
  }

  .layout-segment {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
  }

  .detail-column {
    min-width: 0;
    width: 100%;
    min-height: 0;
    display: flex;
    border-left: 1px solid var(--border-default);
  }

  /* In the board's drawer the detail has no list beside it, so no divider. */
  .drawer-detail {
    display: flex;
    min-height: 100%;
  }

  .drawer-detail .detail-column {
    border-left: 0;
  }

  .detail-unavailable {
    margin: auto;
    padding: var(--space-6);
    color: var(--text-muted);
    text-align: center;
  }

  .mutation-message {
    position: absolute;
    z-index: 10;
    top: var(--space-3);
    right: var(--space-3);
    max-width: min(420px, calc(100% - 24px));
    margin: 0;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-primary);
    padding: var(--space-3) var(--space-4);
    font-size: var(--font-size-sm);
  }

  @media (max-width: 760px) {
    .desktop-navigation {
      display: none;
    }

    .mobile-navigation-trigger {
      display: block;
    }

    .layout-segment-label {
      display: none;
    }
  }
</style>
