<script lang="ts">
  import { Button, cleanupTheme, initTheme, setThemeMode } from '@kenn-io/kit-ui'
  import { startBrowserTelemetry } from '@kenn-io/kit-ui/utils/browser-telemetry'
  import { onMount } from 'svelte'

  import { commentRetryIdentity } from './lib/kata/commentRetry'
  import type { KataCommentReplyIntent, KataTaskSummary } from './lib/kata/types'
  import { webActor } from './lib/kata/attention'
  import { defaultDoneLimit, maxDoneLimit, type BoardMovePlan } from './lib/kata/board'
  import AppShell from './components/AppShell.svelte'
  import KataDaemonSwitcher from './components/KataDaemonSwitcher.svelte'
  import LaunchHint from './components/LaunchHint.svelte'
  import LoginView from './components/LoginView.svelte'
  import RouteError from './components/RouteError.svelte'
  import StatusPage from './components/shared/StatusPage.svelte'
  import VersionMismatch from './components/VersionMismatch.svelte'
  import { createCredentialedFetch, setGeneratedFetch } from './lib/api/client'
  import {
    addAttachment,
    addLabel as addLabelRequest,
    assignIssue,
    captureTelemetryEvent,
    claimIssue as claimIssueRequest,
    closeIssue as closeIssueRequest,
    createComment,
    createIssue as createIssueRequest,
    createLink,
    createRecurrence as createRecurrenceRequest,
    deleteRecurrence as deleteRecurrenceRequest,
    editIssue as editIssueRequest,
    initProject,
    listTokens,
    moveIssue as moveIssueRequest,
    notifyIssue,
    patchIssueMetadata,
    patchProjectMetadata,
    patchRecurrence as patchRecurrenceRequest,
    readUIReferences,
    resolveUIIssueReference,
    removeLabel as removeLabelRequest,
    reopenIssue as reopenIssueRequest,
    setIssuePriority,
    unassignIssue,
    type CreateRecurrenceRequestBody,
    type PatchRecurrenceRequestBody,
    type ProjectOut,
    type RecurrenceTemplateInput,
    type RecurrenceTemplateUpdateInput,
    type UIIssueReference,
    type UIReferencesResponseBody,
    type TokenOut,
  } from './lib/api/generated'
  import {
    createDaemonFetch,
    fetchWebDaemons,
    rosterUnavailable,
    type WebDaemonInfo,
  } from './lib/daemons/client'
  import { loadDaemonRoute, saveDaemonRoute } from './lib/daemons/state'
  import {
    AuthenticationRequiredError,
    clearSessionCredentials,
    consumeLaunchFragment,
    exchangeLoginToken,
    isAuthenticationRequiredError,
    loadSessionCredentials,
    openLocalSession,
    openTrustedProxySession,
    selectAuthenticationMode,
    type AuthenticationMode,
    type LaunchState,
  } from './lib/auth/session'
  import {
    EventStreamController,
    InvalidationController,
    RefreshScheduler,
  } from './lib/events/controller'
  import { openEventStream } from './lib/events/sse'
  import { parseRoute, serializeRoute, screenNameForRoute, type KataRoute } from './lib/router'
  import type {
    KataCreateRecurrenceInput,
    KataPatchRecurrenceInput,
    KataRecurrence,
    KataTaskCloseRequest,
    KataTaskEditPatch,
  } from './lib/kata/types'
  import {
    MutationController,
    type MutationContext,
    type MutationResult,
    type MutationState,
  } from './lib/mutations/controller'
  import {
    collectionIntentKey,
    createUISnapshotRequest,
    projectUISnapshot,
    selectsInSameList,
    SnapshotController,
    snapshotIntentForRoute,
    snapshotRequestIntent,
    uiSnapshotIntentKey,
    type SnapshotState,
    type UISnapshot,
    type UISnapshotIntent,
  } from './lib/state/snapshot'
  import { loadPreferences, savePreferences, type Preferences } from './lib/state/preferences'
  import { applicationRoutePath, currentApplicationBaseURL } from './lib/applicationBase'
  import { startSessionEndedReporting } from './lib/session-ended'

  type ShellMode = 'loading' | 'launch' | 'login' | 'route-error' | 'ready'
  type AppRoute = Exclude<KataRoute, { kind: 'route-error' }>
  const requestActor = webActor
  const routePath = applicationRoutePath()
  const mountedApplication = currentApplicationBaseURL().pathname !== '/'

  initTheme({ storageKey: 'kata.kit-ui.theme.v1' })

  if (window.location.pathname === '/') {
    history.replaceState(null, '', `/kata${window.location.search}${window.location.hash}`)
  }
  const launch = consumeLaunchFragment(window.location, history.replaceState.bind(history))
  const launchDaemonID = launch.daemonID
  const directDaemonTarget = launch.directTarget === true || mountedApplication
  const selectedAuthentication = selectAuthenticationMode(launch)
  const initialRoute = parseRoute(new URL(window.location.href))
  let route = $state(initialRoute)
  let linkUpgradeRequired = false
  let acceptedRoute = $state<AppRoute | undefined>()
  let returnPath = $state(launch.returnPath)
  let mode = $state(initialMode(launch, initialRoute, selectedAuthentication))
  let authority = $state.raw<SnapshotState<UISnapshotIntent, UISnapshot> | undefined>()
  // The list route last shown from the snapshot already loaded, without a
  // request of its own.
  let listFromLoadedSnapshot = $state.raw<AppRoute | undefined>()
  let shownSnapshot:
    | { source: UISnapshot; projectUID: string | undefined; listOnly: boolean; value: UISnapshot }
    | undefined
  // The daemon sends every project's tasks; the shell shows the accepted route's
  // share of them. The same inputs keep the same object, so an unchanged
  // revalidation does not re-render the list.
  const shellSnapshot = $derived.by(() => {
    const source = authority?.snapshot
    if (!source) return undefined
    const projectUID = acceptedRoute?.projectUID
    // A list shown from a task's snapshot leaves that task's detail behind.
    const listOnly = acceptedRoute !== undefined && acceptedRoute === listFromLoadedSnapshot
    if (
      shownSnapshot?.source === source &&
      shownSnapshot.projectUID === projectUID &&
      shownSnapshot.listOnly === listOnly
    ) {
      return shownSnapshot.value
    }
    let value = projectUID ? projectUISnapshot(source, projectUID) : source
    if (listOnly && (value.selected || value.graph)) {
      value = { ...value }
      delete value.selected
      delete value.graph
    }
    shownSnapshot = { source, projectUID, listOnly, value }
    return value
  })
  let references = $state<UIReferencesResponseBody | undefined>()
  let mutationPending = $state(false)
  let mutationState = $state<MutationState>({ kind: 'idle' })
  let draftFenceGeneration = $state(0)
  let pendingCreate:
    | { title: string; parentIssueUID?: string; projectUID: string; key: string }
    | undefined
  let pendingComment: { identity: string; key: string } | undefined
  let automaticSessionAttempted: 'loopback' | 'proxy' | undefined
  let advertisedAuthentication: 'loopback' | 'login' | 'proxy' | 'unavailable' | undefined
  let preferences = $state(loadPreferences())
  // How many closed issues the board's Done column has loaded from the server.
  let boardClosedLimit = defaultDoneLimit
  let versionMismatch = $state(false)
  let liveUpdatesReconnecting = $state(false)
  let daemonInfos = $state<WebDaemonInfo[]>([])
  let activeDaemonID = $state<string | undefined>()
  let explicitDaemonID = launchDaemonID
  let daemonRosterLoaded = false
  let daemonRosterAbort: AbortController | undefined
  let daemonRefreshAbort: AbortController | undefined
  let authenticationRecoveryPending = false
  let authenticationRecoveryCompletion: Promise<boolean> | undefined
  let daemonSwitching = $state(false)
  let daemonError = $state<string | undefined>()
  let referenceAbort: AbortController | undefined
  let referenceGeneration = 0
  let credentialTokens = $state<TokenOut[]>([])
  let syncRefreshGeneration = $state(0)
  let credentialLoading = $state(false)
  let credentialError = $state<string | undefined>()
  let credentialAbort: AbortController | undefined
  let credentialGeneration = 0
  let destroyed = false
  const observedFetch: typeof fetch = async (input, init) => {
    const response = await fetch(input, init)
    const authentication = response.headers.get('X-Kata-Web-Authentication')
    if (
      authentication === 'loopback' ||
      authentication === 'login' ||
      authentication === 'proxy' ||
      authentication === 'unavailable'
    ) {
      advertisedAuthentication = authentication
    }
    return response
  }
  const daemonFetch = createDaemonFetch(() => activeDaemonID, observedFetch)
  const browserFetch = createCredentialedFetch(
    undefined,
    daemonFetch,
    rejectCredentialsAndRequireAuthentication,
  )
  const searchSettingsPostFetch = createCredentialedFetch(
    undefined,
    fetch,
    rejectCredentialsAndRequireAuthentication,
  )
  setGeneratedFetch(browserFetch)
  let browserTelemetry: ReturnType<typeof startBrowserTelemetry> | undefined
  let stopQuietSubscription: (() => void) | undefined
  let sessionEnded: ReturnType<typeof startSessionEndedReporting> | undefined
  // Only a signed-in tab counts as an opening; anonymous viewers send nothing.
  function reportAppOpened(): void {
    if (destroyed || loadSessionCredentials() === undefined) return
    sessionEnded ??= startSessionEndedReporting(async (duration) => {
      browserTelemetry?.reportVisit(
        { event: 'session_ended', properties: { surface: 'web', duration_bucket: duration } },
        { keepalive: true },
      )
    }, true)
    if (browserTelemetry) {
      sessionEnded.resume()
      return
    }
    browserTelemetry = startBrowserTelemetry({
      route: '/api/v1/ui/telemetry',
      surface: 'web',
      storageKey: 'kata.web.app-opened.v1',
      post: async (_route, event, options) => {
        const response = await captureTelemetryEvent(event, options)
        if (response.status === 401) {
          restartAppOpenedAfterAuthentication()
          throw new Error('Telemetry authentication expired')
        }
        return response
      },
    })
    stopQuietSubscription = browserTelemetry.onQuietChange((quiet) => sessionEnded?.setQuiet(quiet))
    sessionEnded.resume()
    reportScreenViewed()
  }
  function restartAppOpenedAfterAuthentication(): void {
    sessionEnded?.pause()
    stopQuietSubscription?.()
    browserTelemetry?.stop()
    browserTelemetry = undefined
    reportAppOpened()
  }
  function reportScreenViewed(): void {
    if (destroyed || mode !== 'ready' || !acceptedRoute || !loadSessionCredentials()) return
    const screen = screenNameForRoute(acceptedRoute)
    if (screen) browserTelemetry?.reportScreen(screen)
  }
  $effect(() => {
    reportScreenViewed()
  })
  const snapshots = new SnapshotController(
    createUISnapshotRequest(),
    uiSnapshotIntentKey,
    selectsInSameList,
  )
  const mutations = new MutationController({
    authority: () => ({
      canMutate: authority?.canMutate ?? false,
      actorPolicy: authority?.snapshot?.capabilities.actor_policy ?? 'readonly',
    }),
    refresh: refreshMutationAuthority,
    onAuthenticationRequired: requireAuthentication,
  })
  const accessMutations = new MutationController({
    authority: () => ({
      canMutate: canAdministerAccess(),
      actorPolicy: authority?.snapshot?.capabilities.actor_policy ?? 'readonly',
    }),
    refresh: async () => {
      await refreshSnapshot(true)
      return canAdministerAccess()
    },
    onAuthenticationRequired: requireAuthentication,
  })

  function canAdministerAccess(): boolean {
    return authority?.snapshot?.capabilities.access_admin === true && canAdministerDaemon()
  }

  function canAdministerDaemon(): boolean {
    const expiresAt = authority?.snapshot?.capabilities.expires_at
    return (
      authority?.snapshot !== undefined &&
      !authority.stale &&
      !authority.loading &&
      !authority.authenticationRequired &&
      !daemonSwitching &&
      !daemonError &&
      !liveUpdatesReconnecting &&
      (!expiresAt || Date.parse(expiresAt) > Date.now()) &&
      (!activeDaemonID ||
        daemonInfos.find((daemon) => daemon.id === activeDaemonID)?.local === true)
    )
  }

  let recurrenceRevision = $state(0)
  const invalidations = new InvalidationController((full) => refreshSnapshot(full))
  const stream = new EventStreamController({
    connect: (cursor, signal, onOpen) => openEventStream(browserFetch, cursor, signal, onOpen),
    onFrame: (frame) => {
      if (frame.event.startsWith('recurrence.')) recurrenceRevision++
      invalidations.frame(frame)
    },
    onAuthenticationRequired: requireAuthentication,
    onState: (state) => {
      const wasReconnecting = liveUpdatesReconnecting
      liveUpdatesReconnecting = state === 'reconnecting'
      if (state === 'reconnecting' || (state === 'online' && wasReconnecting)) {
        invalidations.reconnect()
      } else if (state === 'online') {
        invalidations.refresh()
      }
    },
  })
  const scheduler = new RefreshScheduler({
    refresh: () => invalidations.refresh(),
    openEvents: () => {
      if (authority?.snapshot) stream.start(authority.cursor)
    },
    wallClockSensitive: () =>
      route.kind !== 'route-error' && route.filters.status.includes('ready'),
  })
  function reportOutcome(event: 'search_empty' | 'error_shown', kind?: string): void {
    if (!loadSessionCredentials()) return
    browserTelemetry?.reportVisit({
      event,
      properties: { surface: 'web', ...(kind ? { kind } : {}) },
    })
  }
  const unsubscribe = snapshots.subscribe((state) => {
    authority = { ...state }
    if (
      state.snapshot &&
      !state.loading &&
      !state.authenticationRequired &&
      route.kind !== 'route-error' &&
      (!route.link || route.issueUID !== undefined)
    ) {
      mode = 'ready'
    }
  })

  $effect(() => {
    setThemeMode(preferences.theme)
  })

  $effect(() => {
    if (
      route.kind === 'kata' &&
      route.view === 'credentials' &&
      authority?.snapshot &&
      authority.snapshot.capabilities.token_audit_read !== true &&
      !authority.loading &&
      !authority.stale &&
      !authority.authenticationRequired &&
      !daemonSwitching &&
      !daemonError
    ) {
      fenceCredentialAudit()
      route = { ...route, view: 'settings', section: 'appearance' }
      history.replaceState(null, '', serializeRoute(route))
      if (activeDaemonID) saveDaemonRoute(activeDaemonID, serializeRoute(route))
      loadRoute()
    }
  })

  $effect(() => {
    if (
      versionMismatch ||
      mode === 'loading' ||
      route.kind === 'route-error' ||
      !authority?.snapshot
    ) {
      document.title = 'Kata'
    }
  })

  onMount(() => {
    const visibility = () => {
      scheduler.visibilityChanged(!document.hidden)
      if (!document.hidden) {
        syncRefreshGeneration++
        if (!credentialLoading) void refreshCredentials()
      }
    }
    const focus = () => {
      scheduler.focused()
    }
    const environment = () => scheduler.environmentChanged()
    const showVersionMismatch = () => {
      versionMismatch = true
    }
    const popstate = () => {
      const next = parseRoute(new URL(window.location.href))
      if (route.kind !== 'route-error' && route.view === 'credentials') fenceCredentialAudit()
      route = next
      if (next.kind === 'route-error') {
        mode = 'route-error'
        return
      }
      mode = authority?.snapshot && !next.link ? 'ready' : 'loading'
      loadRoute()
    }
    document.addEventListener('visibilitychange', visibility)
    window.addEventListener('focus', focus)
    window.addEventListener('pageshow', environment)
    window.addEventListener('popstate', popstate)
    window.addEventListener('kata:versionMismatch', showVersionMismatch)
    const credentialRefreshTimer = window.setInterval(() => {
      if (!document.hidden) {
        syncRefreshGeneration++
        if (!credentialLoading) void refreshCredentials()
      }
    }, 30_000)
    reportAppOpened()
    if (route.kind !== 'route-error' && launch.kind !== 'login') {
      if (loadSessionCredentials() !== undefined) {
        void startAuthority()
      } else if (selectedAuthentication === undefined) {
        void startAutomaticSession(launch.returnPath, 'loopback')
      }
    }
    return () => {
      destroyed = true
      document.removeEventListener('visibilitychange', visibility)
      window.removeEventListener('focus', focus)
      window.removeEventListener('pageshow', environment)
      window.removeEventListener('popstate', popstate)
      window.removeEventListener('kata:versionMismatch', showVersionMismatch)
      window.clearInterval(credentialRefreshTimer)
      stopQuietSubscription?.()
      browserTelemetry?.stop()
      sessionEnded?.stop()
      scheduler.stop()
      stream.stop()
      invalidations.stop()
      snapshots.abort()
      daemonRosterAbort?.abort()
      daemonRefreshAbort?.abort()
      referenceGeneration += 1
      referenceAbort?.abort()
      fenceCredentialAudit()
      unsubscribe()
      cleanupTheme()
    }
  })

  function updatePreferences(next: Preferences): void {
    preferences = next
    savePreferences(next)
  }

  async function login(token: string, requestedPath: string): Promise<void> {
    const session = await exchangeLoginToken(token, requestedPath)
    invalidateDaemonRoster()
    await navigateAfterAuthentication(session.returnPath)
  }

  function invalidateDaemonRoster(): void {
    daemonRosterAbort?.abort()
    daemonRefreshAbort?.abort()
    if (!directDaemonTarget) daemonRosterLoaded = false
  }

  async function navigateAfterAuthentication(target: string): Promise<boolean> {
    reportAppOpened()
    const parsed = new URL(target, window.location.origin)
    const canonicalTarget =
      parsed.pathname === '/'
        ? `${routePath}${parsed.search}`
        : `${parsed.pathname}${parsed.search}`
    history.replaceState(null, '', canonicalTarget)
    route = parseRoute(new URL(window.location.href))
    mode = route.kind === 'route-error' ? 'route-error' : authority?.snapshot ? 'ready' : 'loading'
    return route.kind !== 'route-error' && (await startAuthority())
  }

  function search(reference: string): void {
    navigate({
      kind: 'kata',
      view: 'all-open',
      graph: false,
      filters: { status: ['all'], owner: [], label: [], relationship: [], text: reference },
    })
  }

  function navigate(next: AppRoute): void {
    const current = route.kind === 'kata' ? route : undefined
    if (
      next.link &&
      (!next.issueUID ||
        next.issueUID !== current?.issueUID ||
        next.projectUID !== current?.projectUID)
    ) {
      next = { ...next }
      delete next.link
    }
    if (
      route.kind !== 'route-error' &&
      route.view === 'credentials' &&
      next.view !== 'credentials'
    ) {
      fenceCredentialAudit()
    }
    history.pushState(null, '', serializeRoute(next))
    if (
      route.kind === 'route-error' ||
      route.projectUID !== next.projectUID ||
      route.board !== next.board ||
      serializeFilters(route) !== serializeFilters(next)
    ) {
      boardClosedLimit = defaultDoneLimit
    }
    route = next
    if (activeDaemonID) saveDaemonRoute(activeDaemonID, serializeRoute(next))
    mode = authority?.snapshot ? 'ready' : 'loading'
    loadRoute()
  }

  // Shows a list the accepted snapshot already holds, such as another project,
  // in the same frame. Live updates keep that snapshot current; a polled daemon
  // is asked again in the background. Any other route waits for its snapshot.
  function loadRoute(): void {
    // The $state proxy of the route, which refreshSnapshot accepts again after
    // a revalidation. Accepting the same object keeps shellSnapshot, so an
    // unchanged revalidation does not normalize and render the list again.
    const next = route
    if (next.kind === 'route-error') return
    if (!showsFromAcceptedSnapshot(next)) {
      void startAuthority()
      return
    }
    acceptedRoute = next
    listFromLoadedSnapshot = next
    mode = 'ready'
    if (authority?.snapshot?.capabilities.updates === 'poll') invalidations.refresh()
  }

  function showsFromAcceptedSnapshot(next: AppRoute): boolean {
    const accepted = snapshots.state
    return (
      next.issueUID === undefined &&
      !next.link &&
      !isDaemonView(next.view) &&
      accepted.snapshot !== undefined &&
      accepted.intent !== undefined &&
      !accepted.loading &&
      !accepted.stale &&
      !accepted.authenticationRequired &&
      !daemonSwitching &&
      daemonError === undefined &&
      collectionIntentKey(accepted.intent) === collectionIntentKey(requestIntentForRoute(next))
    )
  }

  // Reads the route afresh after an await; TypeScript would keep a narrowing
  // of the route variable from before it.
  function currentRoute(): KataRoute {
    return route
  }

  function requestIntentForRoute(next: AppRoute): UISnapshotIntent {
    const intent = snapshotRequestIntent(
      snapshotIntentForRoute(next, undefined, undefined, { closedLimit: boardClosedLimit }),
    )
    return activeDaemonID ? { ...intent, daemonID: activeDaemonID } : intent
  }

  async function createProject(name: string, makeInbox = false): Promise<{ changed: boolean }> {
    let changed = false
    const accepted = await runMutation({}, async (context) => {
      const result = await initProject(context.body({ name }, requestActor))
      if (result.status !== 200) return result
      changed = result.data.created
      return makeInbox ? patchInboxProject(result.data.project, context) : result
    })
    if (!accepted) {
      if (changed && mutationState.kind === 'domain-error') await refreshSnapshot(true)
      throw new Error(mutationMessage(mutationState) ?? 'Could not create project.')
    }
    return { changed }
  }

  function patchInboxProject(project: ProjectOut, context: MutationContext) {
    context.headers.set('If-Match', `"rev-${project.revision}"`)
    return patchProjectMetadata(
      { projectId: project.id },
      { actor: requestActor, patch: { role: 'inbox' } },
      { headers: context.headers },
    )
  }

  async function createIssue(
    title: string,
    parent?: { projectID: number; projectUID: string; issueUID: string },
    projectUID?: string,
  ): Promise<void> {
    const project = parent
      ? authority?.snapshot?.catalog?.find(({ project }) => project.id === parent.projectID)
          ?.project
      : authority?.snapshot?.catalog?.find(({ project }) =>
          projectUID ? project.uid === projectUID : project.metadata?.role === 'inbox',
        )?.project
    if (!project) throw new Error('Task project is not available.')

    const create =
      pendingCreate?.title === title &&
      pendingCreate.parentIssueUID === parent?.issueUID &&
      pendingCreate.projectUID === project.uid
        ? pendingCreate
        : {
            title,
            projectUID: project.uid,
            ...(parent ? { parentIssueUID: parent.issueUID } : {}),
            key: globalThis.crypto.randomUUID(),
          }
    pendingCreate = create
    const options = { draft: title, createKey: create.key, actionKind: 'issue_created' }
    const accepted = await runMutation(options, (context) =>
      createIssueRequest(
        { projectId: String(project.id) },
        context.body(
          {
            title,
            ...(parent
              ? {
                  links: [
                    {
                      type: 'parent' as const,
                      to_ref: parent.issueUID,
                      to_project_uid: parent.projectUID,
                    },
                  ],
                }
              : {}),
          },
          requestActor,
        ),
        { headers: { 'Idempotency-Key': create.key, 'X-Kata-Surface': 'web' } },
      ),
    )
    if (!accepted) throw new Error(mutationMessage(mutationState) ?? 'Could not create task.')
    pendingCreate = undefined
  }

  async function loadReferences(): Promise<void> {
    if (destroyed) return
    const generation = referenceGeneration + 1
    referenceGeneration = generation
    referenceAbort?.abort()
    const abort = new AbortController()
    referenceAbort = abort
    const daemonID = activeDaemonID
    try {
      const result = await readUIReferences({ limit: 200 }, { signal: abort.signal })
      if (
        result.status === 200 &&
        !destroyed &&
        !abort.signal.aborted &&
        generation === referenceGeneration &&
        daemonID === activeDaemonID
      ) {
        references = result.data
      }
    } catch {
      // Snapshot authority remains usable when reference enrichment is canceled or unavailable.
    } finally {
      if (referenceAbort === abort) referenceAbort = undefined
    }
  }

  async function searchReferences(query: string): Promise<UIIssueReference[]> {
    const generation = referenceGeneration
    const daemonID = activeDaemonID
    const result = await readUIReferences({ q: query, limit: 20 })
    if (destroyed || generation !== referenceGeneration || daemonID !== activeDaemonID) return []
    return result.status === 200 ? (result.data.issues ?? []) : []
  }

  async function editIssue(uid: string, patch: KataTaskEditPatch): Promise<boolean> {
    const target = selectedMutationTarget(uid)
    if (!target) return false
    const related = patch.links_delta?.add_related?.[0]
    if (related) {
      return runMutation({ draft: patch }, (context) =>
        createLink(
          { projectId: target.project_id, ref: target.ref },
          context.body({ type: 'related' as const, to_ref: related }, requestActor),
        ),
      )
    }
    const body: { title?: string; body?: string; expected_content_revision?: number } = {}
    if (patch.title !== undefined) body.title = patch.title
    if (patch.body !== undefined) body.body = patch.body
    if (patch.expected_content_revision !== undefined)
      body.expected_content_revision = patch.expected_content_revision
    return runMutation({ draft: patch }, (context) =>
      editIssueRequest(
        { projectId: String(target.project_id), ref: target.ref },
        context.body(body, requestActor),
      ),
    )
  }

  async function patchMetadata(uid: string, patch: Record<string, unknown>): Promise<boolean> {
    const target = selectedMutationTarget(uid)
    const revision = selectedRevision(uid)
    if (!target || !revision) return false
    return runMutation({ draft: patch, revision }, (context) =>
      patchIssueMetadata(
        { projectId: target.project_id, ref: target.ref },
        context.body({ patch }, requestActor),
        { headers: context.headers },
      ),
    )
  }

  async function attachFile(uid: string, file: File): Promise<void> {
    const target = selectedMutationTarget(uid)
    if (!target) throw new Error('issue_not_found')
    const accepted = await runMutation({ draft: file }, (context) =>
      addAttachment(
        { projectId: target.project_id, ref: target.ref },
        file,
        { name: file.name, ...context.body<{ actor?: string }>({}, requestActor) },
        { headers: context.headers },
      ),
    )
    if (!accepted) throw new Error(mutationMessage(mutationState) ?? 'attachment_upload_failed')
  }

  async function addComment(
    uid: string,
    body: string,
    reply?: KataCommentReplyIntent,
  ): Promise<boolean> {
    const target = selectedMutationTarget(uid)
    if (!target) return false
    const identity = commentRetryIdentity(uid, body, reply)
    const comment =
      pendingComment?.identity === identity
        ? pendingComment
        : { identity, key: crypto.randomUUID() }
    pendingComment = comment
    const accepted = await runMutation({ draft: body }, (context) =>
      createComment(
        { projectId: String(target.project_id), ref: target.ref },
        context.body(
          {
            body,
            ...(reply ? { reply_to: reply.replyTo, kind: reply.kind, force: reply.force } : {}),
          },
          requestActor,
        ),
        { headers: { 'Idempotency-Key': comment.key } },
      ),
    )
    if (accepted) pendingComment = undefined
    return accepted
  }

  async function createRecurrence(
    projectID: number,
    input: KataCreateRecurrenceInput,
  ): Promise<void> {
    const body: CreateRecurrenceRequestBody = {
      initial_issue_ref: input.initialIssueRef,
      rrule: input.rrule,
      dtstart: input.dtstart,
      timezone: input.timezone,
      template: recurrenceTemplate(input.template),
    }
    const accepted = await runMutation({ draft: input }, (context) =>
      createRecurrenceRequest(
        { projectId: projectID },
        context.body(body, input.actor || requestActor),
      ),
    )
    if (!accepted) throw new Error(mutationMessage(mutationState) ?? 'Could not create recurrence.')
    recurrenceRevision++
  }

  async function patchRecurrence(
    recurrence: KataRecurrence,
    input: KataPatchRecurrenceInput,
    etag: string,
  ): Promise<void> {
    const body: PatchRecurrenceRequestBody = {}
    if (input.rrule !== undefined) body.rrule = input.rrule
    if (input.dtstart !== undefined) body.dtstart = input.dtstart
    if (input.timezone !== undefined) body.timezone = input.timezone
    if (input.template !== undefined) body.template = recurrenceTemplateUpdate(input.template)
    const accepted = await runMutation({ draft: input, revision: etag }, (context) =>
      patchRecurrenceRequest(
        { projectId: recurrence.project_id, recurrenceUid: recurrence.uid },
        context.body(body, input.actor || requestActor),
        { headers: context.headers },
      ),
    )
    if (accepted) {
      recurrenceRevision++
      return
    }
    if (mutationState.kind === 'revision-conflict') {
      throw Object.assign(new Error(mutationState.detail), {
        status: 412,
        code: mutationState.code,
      })
    }
    throw new Error(mutationMessage(mutationState) ?? 'Could not update recurrence.')
  }

  async function deleteRecurrence(recurrence: KataRecurrence): Promise<void> {
    const accepted = await runMutation(
      { draft: recurrence, revision: `"rev-${recurrence.revision}"` },
      async (context) => {
        const result = await deleteRecurrenceRequest(
          { projectId: recurrence.project_id, recurrenceUid: recurrence.uid },
          authority?.snapshot?.capabilities.actor_policy === 'identity'
            ? undefined
            : { actor: requestActor },
          { headers: context.headers },
        )
        return { ...result, data: result.status === 204 ? true : result.data }
      },
    )
    if (!accepted) {
      if (mutationState.kind === 'revision-conflict') {
        throw Object.assign(new Error(mutationState.detail), { status: 412 })
      }
      throw new Error(mutationMessage(mutationState) ?? 'Could not stop repeating.')
    }
    recurrenceRevision++
  }

  function recurrenceTemplate(
    input: KataCreateRecurrenceInput['template'],
  ): RecurrenceTemplateInput {
    const template: RecurrenceTemplateInput = {
      title: input.title,
    }
    if (input.body !== undefined) template.body = input.body
    if (input.owner !== undefined) template.owner = input.owner
    if (input.priority !== undefined) template.priority = input.priority
    if (input.labels !== undefined) template.labels = input.labels
    if (input.metadata !== undefined) template.metadata = input.metadata
    return template
  }

  function recurrenceTemplateUpdate(
    input: NonNullable<KataPatchRecurrenceInput['template']>,
  ): RecurrenceTemplateUpdateInput {
    const template: RecurrenceTemplateUpdateInput = {}
    if (input.title !== undefined) template.title = input.title
    if (input.body !== undefined) template.body = input.body
    if (input.owner !== undefined) template.owner = input.owner
    if (input.clearOwner !== undefined) template.clear_owner = input.clearOwner
    if (input.priority !== undefined) template.priority = input.priority
    if (input.clearPriority !== undefined) template.clear_priority = input.clearPriority
    if (input.labels !== undefined) template.labels = input.labels
    if (input.metadata !== undefined) template.metadata = input.metadata
    return template
  }

  async function moveIssue(toProjectUID: string): Promise<boolean> {
    const selected = selectedIssue()
    const target = selected ? selectedMutationTarget(selected.uid) : undefined
    const revision = selected ? selectedRevision(selected.uid) : undefined
    if (!target || !revision) return false
    return runMutation({ draft: toProjectUID, revision }, (context) =>
      moveIssueRequest(
        { projectId: target.project_id, ref: target.ref },
        context.body({ to_project_uid: toProjectUID }, requestActor),
        { headers: context.headers },
      ),
    )
  }

  async function assignOwner(uid: string, owner: string): Promise<boolean> {
    const target = selectedMutationTarget(uid)
    if (!target) return false
    return runMutation({ draft: owner }, (context) =>
      assignIssue(
        { projectId: target.project_id, ref: target.ref },
        context.body({ owner }, requestActor),
      ),
    )
  }

  async function unassignOwner(uid: string): Promise<boolean> {
    const target = selectedMutationTarget(uid)
    if (!target) return false
    return runMutation({}, (context) =>
      unassignIssue(
        { projectId: target.project_id, ref: target.ref },
        context.body({}, requestActor),
      ),
    )
  }

  async function claimAssignment(uid: string, ttlSeconds: number): Promise<boolean> {
    const target = selectedMutationTarget(uid)
    if (!target) return false
    return runMutation({ draft: ttlSeconds }, (context) =>
      claimIssueRequest(
        { projectId: target.project_id, ref: target.ref },
        context.body({ ttl_seconds: ttlSeconds }, requestActor),
        { headers: context.headers },
      ),
    )
  }

  async function setPriority(uid: string, priority: number | null): Promise<boolean> {
    const target = selectedMutationTarget(uid)
    if (!target) return false
    return runMutation({ draft: priority }, (context) =>
      setIssuePriority(
        { projectId: target.project_id, ref: target.ref },
        context.body(priority === null ? {} : { priority }, requestActor),
      ),
    )
  }

  async function addLabel(uid: string, label: string): Promise<boolean> {
    const target = selectedMutationTarget(uid)
    if (!target) return false
    return runMutation({ draft: label }, (context) =>
      addLabelRequest(
        { projectId: String(target.project_id), ref: target.ref },
        context.body({ label }, requestActor),
      ),
    )
  }

  async function removeLabel(uid: string, label: string): Promise<void> {
    const target = selectedMutationTarget(uid)
    if (!target) return
    await runMutation({ draft: label }, () =>
      removeLabelRequest(
        { projectId: target.project_id, ref: target.ref, label },
        authority?.snapshot?.capabilities.actor_policy === 'identity'
          ? undefined
          : { actor: requestActor },
      ),
    )
  }

  async function closeIssue(request: KataTaskCloseRequest): Promise<boolean> {
    const selected = selectedIssue()
    const target = selected ? selectedMutationTarget(selected.uid) : undefined
    if (!target) return false
    return runMutation({ draft: request, actionKind: 'issue_closed' }, (context) =>
      closeIssueRequest(
        { projectId: target.project_id, ref: target.ref },
        context.body({ ...request }, requestActor),
        { headers: { 'X-Kata-Surface': 'web' } },
      ),
    )
  }

  async function reopenIssue(): Promise<void> {
    const selected = selectedIssue()
    const target = selected ? selectedMutationTarget(selected.uid) : undefined
    if (!target) return
    await runMutation({}, (context) =>
      reopenIssueRequest(
        { projectId: target.project_id, ref: target.ref },
        context.body({}, requestActor),
      ),
    )
  }

  // Board cards act on any issue in the collection, not only the selected one.
  // A move is one mutation: its kata actions run in order and stop at the
  // first refusal, and the board refreshes once afterwards. Running each
  // action as its own mutation let the refresh after one action mark the
  // board unwritable before the next. Attention is cleared first, while the
  // issue is still the revision the board showed; then the reopen, label
  // removal, and claim or release follow.
  function moveBoardIssue(issue: KataTaskSummary, plan: BoardMovePlan): Promise<boolean> {
    const target = { projectId: issue.project_id, ref: issue.uid }
    const identityPolicy = authority?.snapshot?.capabilities.actor_policy === 'identity'
    const metadata = plan.clearAttention?.metadata ?? {}
    return runMutation({ draft: plan }, async (context) => {
      const steps: (() => Promise<MutationResult>)[] = []
      if (Object.keys(metadata).length > 0) {
        // If anyone changed the issue since the board showed it, the daemon
        // refuses this write and the move stops before any other step.
        steps.push(() =>
          patchIssueMetadata(target, context.body({ patch: metadata }, requestActor), {
            headers: { 'If-Match': `"rev-${issue.revision}"` },
          }),
        )
      }
      if (plan.reopen) {
        steps.push(() => reopenIssueRequest(target, context.body({}, requestActor)))
      }
      for (const label of plan.clearAttention?.removeLabels ?? []) {
        steps.push(() =>
          removeLabelRequest(
            { ...target, label },
            identityPolicy ? undefined : { actor: requestActor },
          ),
        )
      }
      if (plan.claim) {
        steps.push(() => claimIssueRequest(target, context.body({}, requestActor)))
      }
      if (plan.release) {
        steps.push(() => unassignIssue(target, context.body({}, requestActor)))
      }
      let result: MutationResult = { status: 204, headers: new Headers(), data: {} }
      for (const step of steps) {
        result = await step()
        if (result.status < 200 || result.status >= 300) break
      }
      return result
    })
  }

  // The board's "Ask…": the same request `kata notify <ref> --to <to>` sends.
  function askBoardIssue(issue: KataTaskSummary, to: string, message: string): Promise<boolean> {
    return runMutation({ draft: message }, (context) =>
      notifyIssue(
        { projectId: String(issue.project_id), ref: issue.uid },
        context.body({ to, message }, requestActor),
        { headers: context.headers },
      ),
    )
  }

  function completeBoardIssue(
    issue: KataTaskSummary,
    request: KataTaskCloseRequest,
  ): Promise<boolean> {
    return runMutation({ draft: request, actionKind: 'issue_closed' }, (context) =>
      closeIssueRequest(
        { projectId: issue.project_id, ref: issue.uid },
        context.body({ ...request }, requestActor),
        { headers: { 'X-Kata-Surface': 'web' } },
      ),
    )
  }

  // A card added from the board's Open column, created in that project.
  let pendingBoardCreate: { identity: string; key: string } | undefined
  async function createBoardIssue(projectID: number, title: string): Promise<boolean> {
    const identity = JSON.stringify([projectID, title])
    const create =
      pendingBoardCreate?.identity === identity
        ? pendingBoardCreate
        : { identity, key: globalThis.crypto.randomUUID() }
    pendingBoardCreate = create
    const accepted = await runMutation(
      { draft: title, createKey: create.key, actionKind: 'issue_created' },
      (context) =>
        createIssueRequest(
          { projectId: String(projectID) },
          context.body({ title }, requestActor),
          { headers: { 'Idempotency-Key': create.key, 'X-Kata-Surface': 'web' } },
        ),
    )
    if (accepted) pendingBoardCreate = undefined
    return accepted
  }

  function showMoreBoardDone(): void {
    boardClosedLimit = Math.min(boardClosedLimit + defaultDoneLimit, maxDoneLimit)
    void refreshSnapshot(false)
  }

  function serializeFilters(value: AppRoute): string {
    return JSON.stringify(value.filters)
  }

  async function runMutation(
    options: { draft?: unknown; revision?: string; createKey?: string; actionKind?: string },
    mutate: (context: MutationContext) => Promise<MutationResult>,
    controller: MutationController = mutations,
  ): Promise<boolean> {
    mutationPending = true
    try {
      const result = await controller.execute(options, mutate)
      mutationState = { ...controller.state }
      if (options.actionKind && mutationMessage(controller.state))
        reportOutcome('error_shown', options.actionKind)
      return result !== false
    } finally {
      mutationPending = false
    }
  }

  function selectedIssue(): NonNullable<UISnapshot['selected']>['issue'] | undefined {
    return authority?.snapshot?.selected?.issue
  }

  function selectedMutationTarget(uid: string): { project_id: number; ref: string } | undefined {
    const selected = selectedIssue()
    return selected?.uid === uid
      ? { project_id: selected.project_id, ref: selected.uid }
      : undefined
  }

  function selectedRevision(uid: string): string | undefined {
    const selected = selectedIssue()
    return selected?.uid === uid ? `"rev-${selected.revision}"` : undefined
  }

  async function refreshMutationAuthority(): Promise<boolean> {
    const accepted = await refreshSnapshot(true)
    return accepted && authority?.stale === false && authority.canMutate
  }

  function mutationMessage(state: MutationState): string | undefined {
    if (state.kind === 'revision-conflict') {
      return `${state.code}: ${state.detail} Your draft was preserved.`
    }
    if (state.kind === 'domain-error') return `${state.code}: ${state.detail}`
    if (state.kind === 'uncertain') {
      return 'The write result is uncertain. Kata refreshed current authority before retry.'
    }
    if (state.kind === 'blocked') return "Can't make changes. Refresh or check access."
    return undefined
  }

  function initialMode(
    launchState: LaunchState,
    currentRoute: KataRoute,
    authentication: AuthenticationMode | undefined,
  ): ShellMode {
    if (launchState.kind === 'login') return 'login'
    if (loadSessionCredentials()) {
      return currentRoute.kind === 'route-error' ? 'route-error' : 'loading'
    }
    if (authentication === undefined) {
      return currentRoute.kind === 'route-error' ? 'route-error' : 'loading'
    }
    return authenticationView(authentication)
  }

  function authenticationView(authentication: AuthenticationMode | undefined): ShellMode {
    if (authentication === 'login') return 'login'
    return advertisedAuthentication === 'login' ? 'login' : 'launch'
  }

  function rejectCredentialsAndRequireAuthentication(): boolean {
    sessionEnded?.pause()
    fenceCredentialAudit()
    clearSessionCredentials()
    draftFenceGeneration += 1
    return requireAuthentication()
  }

  function requireAuthentication(): boolean {
    if (authenticationRecoveryPending || loadSessionCredentials() !== undefined) return false
    fenceCredentialAudit()
    scheduler.stop()
    stream.stop()
    invalidations.pause()
    if (!snapshots.state.authenticationRequired) snapshots.markAuthenticationRequired()
    returnPath = window.location.pathname + window.location.search
    if (
      (advertisedAuthentication === 'loopback' || advertisedAuthentication === 'proxy') &&
      selectedAuthentication === undefined &&
      automaticSessionAttempted !== advertisedAuthentication
    ) {
      authenticationRecoveryPending = true
      mode = authority?.snapshot ? 'ready' : 'loading'
      authenticationRecoveryCompletion = startAutomaticSession(returnPath, advertisedAuthentication)
      void authenticationRecoveryCompletion
      return true
    }
    mode = authenticationView(selectedAuthentication)
    return true
  }

  async function startAutomaticSession(
    requestedPath: string,
    authentication: 'loopback' | 'proxy',
  ): Promise<boolean> {
    automaticSessionAttempted = authentication
    try {
      const session =
        authentication === 'proxy'
          ? await openTrustedProxySession(requestedPath)
          : await openLocalSession(requestedPath)
      if (destroyed) return false
      if (session) {
        invalidateDaemonRoster()
        let accepted = false
        try {
          accepted = await navigateAfterAuthentication(session.returnPath)
        } finally {
          authenticationRecoveryPending = false
        }
        if (!accepted && snapshots.state.authenticationRequired) requireAuthentication()
        return accepted
      }
    } catch {
      // Fall through to ordinary anonymous/login authority discovery.
    }
    authenticationRecoveryPending = false
    return startAuthority()
  }

  async function startAuthority(): Promise<boolean> {
    scheduler.stop()
    stream.stop()
    const daemonAvailable = await loadDaemonRoster()
    if (destroyed || !daemonAvailable) return false
    const accepted = await invalidations.resume()
    if (destroyed) return false
    if (authority?.authenticationRequired) return false
    if (authority?.snapshot) automaticSessionAttempted = undefined
    if (authority?.snapshot) void loadReferences()
    if (authority?.snapshot) scheduler.start(authority.snapshot.capabilities.updates)
    if (authority?.snapshot) void refreshCredentials()
    return accepted
  }

  function readDaemonRoster(signal: AbortSignal): Promise<WebDaemonInfo[]> {
    return fetchWebDaemons(browserFetch, signal, explicitDaemonID)
  }

  async function loadDaemonRoster(): Promise<boolean> {
    if (daemonRosterLoaded) return directDaemonTarget || activeDaemonID !== undefined
    if (directDaemonTarget) {
      daemonRosterLoaded = true
      daemonInfos = []
      activeDaemonID = undefined
      return true
    }
    const retainedDaemonID = activeDaemonID
    daemonRosterAbort?.abort()
    const abort = (daemonRosterAbort = new AbortController())
    try {
      const loadedDaemons = await readDaemonRoster(abort.signal)
      if (abort.signal.aborted || destroyed) return false
      const nextDaemonID =
        loadedDaemons.find(
          (daemon) => daemon.id === retainedDaemonID && daemon.health !== 'upgrade_required',
        )?.id ??
        loadedDaemons.find(
          (daemon) => daemon.id === launchDaemonID && daemon.health !== 'upgrade_required',
        )?.id ??
        loadedDaemons.find((daemon) => daemon.default && daemon.health !== 'upgrade_required')
          ?.id ??
        loadedDaemons.find((daemon) => daemon.health !== 'upgrade_required')?.id
      if (retainedDaemonID !== undefined && nextDaemonID !== retainedDaemonID) {
        snapshots.clear()
        referenceGeneration += 1
        referenceAbort?.abort()
        references = undefined
        acceptedRoute = undefined
        mode = 'loading'
      }
      daemonInfos = loadedDaemons
      daemonRosterLoaded = true
      activeDaemonID = nextDaemonID
      if (!activeDaemonID) {
        snapshots.clear()
        daemonError = 'No compatible Kata daemon is available'
        return false
      }
      daemonError = undefined
      if (activeDaemonID && window.location.pathname === routePath && !window.location.search) {
        const persisted = loadDaemonRoute(activeDaemonID)
        if (persisted) {
          history.replaceState(null, '', persisted)
          const restored = parseRoute(new URL(window.location.href))
          if (restored.kind !== 'route-error') route = restored
        }
      }
    } catch (error) {
      if (abort.signal.aborted || destroyed) return false
      if (
        isAuthenticationRequiredError(error) &&
        error instanceof AuthenticationRequiredError &&
        error.authenticationTransitioned
      ) {
        daemonError = undefined
        return false
      }
      snapshots.clear()
      referenceGeneration += 1
      referenceAbort?.abort()
      references = undefined
      acceptedRoute = undefined
      mode = 'loading'
      daemonError = rosterUnavailable
      return false
    } finally {
      if (daemonRosterAbort === abort) daemonRosterAbort = undefined
    }
    return true
  }

  async function refreshDaemonRoster(): Promise<void> {
    if (directDaemonTarget) return
    daemonRefreshAbort?.abort()
    const abort = (daemonRefreshAbort = new AbortController())
    try {
      const loaded = await readDaemonRoster(abort.signal)
      if (abort.signal.aborted || destroyed) return
      daemonInfos = loaded
    } catch {
      // Keep the current roster when refresh fails.
    } finally {
      if (daemonRefreshAbort === abort) daemonRefreshAbort = undefined
    }
  }

  async function switchDaemon(id: string): Promise<void> {
    if (daemonSwitching || authenticationRecoveryPending || id === activeDaemonID) return
    if (daemonInfos.find((daemon) => daemon.id === id)?.health === 'upgrade_required') return
    const sourceDaemon = activeDaemonID
    const sourceExplicitDaemon = explicitDaemonID
    const sourceRoute = route.kind === 'route-error' ? acceptedRoute : route
    if (sourceDaemon && sourceRoute) saveDaemonRoute(sourceDaemon, serializeRoute(sourceRoute))
    const restoredPath = loadDaemonRoute(id) ?? `${routePath}?view=all-open`
    const restored = parseRoute(new URL(restoredPath, window.location.origin))
    if (restored.kind === 'route-error') return

    daemonSwitching = true
    daemonError = undefined
    scheduler.stop()
    stream.stop()
    invalidations.pause()
    fenceCredentialAudit()
    snapshots.clear()
    referenceGeneration += 1
    referenceAbort?.abort()
    references = undefined
    activeDaemonID = id
    explicitDaemonID = id
    linkUpgradeRequired = false
    route = restored
    acceptedRoute = undefined
    history.replaceState(null, '', serializeRoute(restored))
    mode = 'loading'
    authenticationRecoveryCompletion = undefined
    const accepted = await startAuthority()
    const recovery = authenticationRecoveryCompletion
    const recovered = !accepted && recovery ? await recovery : false
    if ((accepted || recovered) && activeDaemonID === id) {
      saveDaemonRoute(id, serializeRoute(restored))
      daemonSwitching = false
      if (authenticationRecoveryCompletion === recovery)
        authenticationRecoveryCompletion = undefined
      return
    }
    if (authority?.authenticationRequired) {
      daemonSwitching = false
      if (authenticationRecoveryCompletion === recovery)
        authenticationRecoveryCompletion = undefined
      return
    }

    if ((linkUpgradeRequired || currentRoute().kind === 'route-error') && activeDaemonID === id) {
      daemonSwitching = false
      return
    }

    daemonError = `Could not connect to ${id}`
    if (sourceDaemon && sourceRoute) {
      activeDaemonID = sourceDaemon
      explicitDaemonID = sourceExplicitDaemon
      route = sourceRoute
      acceptedRoute = undefined
      history.replaceState(null, '', serializeRoute(sourceRoute))
      snapshots.clear()
      await startAuthority()
    }
    daemonSwitching = false
    if (authenticationRecoveryCompletion === recovery) authenticationRecoveryCompletion = undefined
  }

  async function refreshSnapshot(full: boolean): Promise<boolean> {
    if (route.kind === 'route-error') return false
    if (route.link && !route.issueUID) {
      linkUpgradeRequired = false
      const pending = route
      const link = route.link
      const daemonID = activeDaemonID
      mode = 'loading'
      try {
        const result = await resolveUIIssueReference({
          ref: link.issue,
          ...(link.scope ? { project: link.scope } : {}),
        })
        if (destroyed || route !== pending || daemonID !== activeDaemonID) return false
        if (result.status === 401) {
          snapshots.markAuthenticationRequired()
          requireAuthentication()
          return false
        }
        if (result.status === 422) {
          linkUpgradeRequired = true
          snapshots.markFailure('Upgrade the selected daemon to open short issue links.')
          return false
        }
        if (result.status === 404) {
          route = {
            kind: 'route-error',
            path: window.location.pathname + window.location.search,
            reason: 'issue_uid',
            searchRef: link.issue,
          }
          mode = 'route-error'
          return false
        }
        if (result.status !== 200) {
          snapshots.markFailure()
          return false
        }
        const resolved: AppRoute = { ...pending, issueUID: result.data.issue.uid }
        if (result.data.scope_project_uid) resolved.projectUID = result.data.scope_project_uid
        else delete resolved.projectUID
        route = resolved
      } catch {
        if (!destroyed && route === pending && daemonID === activeDaemonID) snapshots.markFailure()
        return false
      }
    }
    const snapshotRoute = currentRoute()
    if (snapshotRoute.kind === 'route-error') return false
    const accepted = await snapshots.load(requestIntentForRoute(snapshotRoute), { full })
    if (snapshots.state.authenticationRequired) {
      requireAuthentication()
      return false
    }
    // The route may have changed while this request ran. The snapshot serves
    // the current route when that route requests the same snapshot, such as
    // another project's list.
    const current = currentRoute()
    if (
      current.kind !== 'route-error' &&
      snapshots.state.snapshot &&
      snapshots.state.intent &&
      uiSnapshotIntentKey(snapshots.state.intent) ===
        uiSnapshotIntentKey(requestIntentForRoute(current))
    ) {
      acceptedRoute = current
      return accepted
    }
    return false
  }

  async function refreshCredentials(): Promise<void> {
    if (
      destroyed ||
      document.hidden ||
      route.kind === 'route-error' ||
      route.view !== 'credentials' ||
      authority?.snapshot?.capabilities.token_audit_read !== true
    ) {
      return
    }
    const generation = credentialGeneration + 1
    credentialGeneration = generation
    credentialAbort?.abort()
    const abort = new AbortController()
    credentialAbort = abort
    const daemonID = activeDaemonID
    credentialLoading = true
    credentialError = undefined
    try {
      const result = await listTokens({ signal: abort.signal })
      if (
        destroyed ||
        abort.signal.aborted ||
        generation !== credentialGeneration ||
        daemonID !== activeDaemonID
      ) {
        return
      }
      if (result.status === 200) {
        credentialTokens = result.data.tokens
        credentialError = undefined
      } else if (result.status === 401) {
        fenceCredentialAudit()
        requireAuthentication()
      } else {
        credentialTokens = []
        credentialError = "Couldn't load credentials."
      }
    } catch {
      if (
        !destroyed &&
        !abort.signal.aborted &&
        generation === credentialGeneration &&
        daemonID === activeDaemonID
      ) {
        credentialTokens = []
        credentialError = "Couldn't load credentials."
      }
    } finally {
      if (generation === credentialGeneration) credentialLoading = false
      if (credentialAbort === abort) credentialAbort = undefined
    }
  }

  function fenceCredentialAudit(): void {
    credentialGeneration += 1
    credentialAbort?.abort()
    credentialAbort = undefined
    credentialTokens = []
    credentialLoading = false
    credentialError = undefined
  }

  function isDaemonView(view: AppRoute['view']): boolean {
    return view === 'credentials' || view === 'settings'
  }
</script>

<main aria-label="Kata" class="kata-app" class:ready={mode === 'ready' && !versionMismatch}>
  {#if mode === 'ready' && !versionMismatch}<h1 class="kit-sr-only">Kata</h1>{/if}
  {#if versionMismatch}
    <VersionMismatch />
  {:else if mode === 'loading'}
    {#if daemonError}
      <StatusPage alert tone="danger" title="Can't reach a server" description={daemonError}>
        <Button
          tone="info"
          surface="solid"
          label="Try again"
          onclick={() => void startAuthority()}
        />
        {#if daemonInfos.length > 0}
          <KataDaemonSwitcher
            daemons={daemonInfos}
            activeId={activeDaemonID}
            activeStatusLabel={daemonError}
            activeStatusTone="error"
            disabled={daemonSwitching}
            onSelect={(id) => void switchDaemon(id)}
            onOpen={() => void refreshDaemonRoster()}
          />
        {/if}
      </StatusPage>
    {:else if authority?.error && !authority.loading}
      <StatusPage
        alert
        tone="danger"
        title="Kata couldn't load this workspace"
        description={authority.error}
      >
        <Button tone="info" surface="solid" label="Retry" onclick={() => void startAuthority()} />
        {#if daemonInfos.length > 0}
          <KataDaemonSwitcher
            daemons={daemonInfos}
            activeId={activeDaemonID}
            activeStatusLabel={daemonError}
            activeStatusTone={daemonError ? 'error' : undefined}
            disabled={daemonSwitching}
            onSelect={(id) => void switchDaemon(id)}
            onOpen={() => void refreshDaemonRoster()}
          />
        {/if}
      </StatusPage>
    {:else}
      <div role="status"><h1 class="loading-heading">Loading…</h1></div>
    {/if}
  {:else if route.kind === 'route-error'}
    <RouteError
      {route}
      onSearch={search}
      onOpenInbox={() =>
        navigate({
          kind: 'kata',
          view: 'inbox',
          graph: false,
          filters: { status: [], owner: [], label: [], relationship: [], text: '' },
        })}
    />
  {:else}
    {#if mode === 'launch'}
      <LaunchHint issueRef={undefined} />
    {:else if mode === 'login'}
      <LoginView {returnPath} onLogin={login} />
    {/if}
    {#if daemonInfos.length > 0 && (mode === 'launch' || mode === 'login')}
      <KataDaemonSwitcher
        daemons={daemonInfos}
        activeId={activeDaemonID}
        activeStatusLabel={daemonError}
        activeStatusTone={daemonError ? 'error' : undefined}
        disabled={daemonSwitching}
        onSelect={(id) => void switchDaemon(id)}
        onOpen={() => void refreshDaemonRoster()}
      />
    {/if}
    {#if authority?.snapshot && mode === 'ready'}
      <AppShell
        {syncRefreshGeneration}
        {searchSettingsPostFetch}
        route={acceptedRoute ?? route}
        requestedRoute={route !== acceptedRoute ? route : undefined}
        onSearchEmpty={() => reportOutcome('search_empty')}
        snapshot={shellSnapshot!}
        selfActor={shellSnapshot!.capabilities.actor_policy === 'identity'
          ? shellSnapshot!.capabilities.account
          : requestActor}
        loading={authority.loading}
        canMutate={authority.canMutate}
        {mutationPending}
        mutationMessage={mutationMessage(mutationState)}
        draftResetGeneration={authority.cursor}
        {draftFenceGeneration}
        {preferences}
        daemons={daemonInfos}
        {activeDaemonID}
        {daemonSwitching}
        reconnecting={liveUpdatesReconnecting}
        stale={authority.stale}
        readOnly={!authority.snapshot.capabilities.writable}
        {daemonError}
        {credentialTokens}
        {credentialLoading}
        {credentialError}
        onAccessMutation={(draft, mutate) => runMutation({ draft }, mutate, accessMutations)}
        onRefreshCredentials={refreshCredentials}
        canAdministerDaemon={canAdministerDaemon()}
        onSelectDaemon={(id) => void switchDaemon(id)}
        onOpenDaemon={() => void refreshDaemonRoster()}
        onPreferencesChange={updatePreferences}
        ownerOptions={(references?.owners ?? []).map((owner) => ({ name: owner, label: owner }))}
        onNavigate={navigate}
        onCreateProject={createProject}
        onCreateIssue={createIssue}
        {searchReferences}
        onMoveIssue={moveIssue}
        onPatchMetadata={patchMetadata}
        onUploadAttachment={attachFile}
        onAddComment={addComment}
        onEditIssue={editIssue}
        onAssignOwner={assignOwner}
        onClaimAssignment={claimAssignment}
        onUnassignOwner={unassignOwner}
        onSetPriority={setPriority}
        onAddLabel={addLabel}
        onRemoveLabel={removeLabel}
        onCloseIssue={closeIssue}
        onReopenIssue={reopenIssue}
        {recurrenceRevision}
        onCreateRecurrence={createRecurrence}
        onPatchRecurrence={patchRecurrence}
        onDeleteRecurrence={deleteRecurrence}
        onMoveBoardIssue={moveBoardIssue}
        onCompleteBoardIssue={completeBoardIssue}
        onAskBoardIssue={askBoardIssue}
        onShowMoreBoardDone={showMoreBoardDone}
        onCreateBoardIssue={createBoardIssue}
      />
    {/if}
  {/if}
</main>
