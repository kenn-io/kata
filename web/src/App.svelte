<script lang="ts">
  import { onMount } from 'svelte'

  import AppShell from './components/AppShell.svelte'
  import KataDaemonSwitcher from './components/KataDaemonSwitcher.svelte'
  import LaunchHint from './components/LaunchHint.svelte'
  import LoginView from './components/LoginView.svelte'
  import RouteError from './components/RouteError.svelte'
  import VersionMismatch from './components/VersionMismatch.svelte'
  import { createCredentialedFetch, createKataClient } from './lib/api/client'
  import { createDaemonFetch, fetchWebDaemons, type WebDaemonInfo } from './lib/daemons/client'
  import { loadDaemonRoute, saveDaemonRoute } from './lib/daemons/state'
  import {
    clearSessionCredentials,
    consumeLaunchFragment,
    exchangeLoginToken,
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
  import { parseRoute, serializeRoute, type KataRoute } from './lib/router'
  import type { components } from './lib/api/schema'
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
    createUISnapshotRequest,
    SnapshotController,
    snapshotIntentForRoute,
    uiSnapshotIntentKey,
    type SnapshotState,
    type UISnapshot,
    type UISnapshotIntent,
  } from './lib/state/snapshot'
  import { loadPreferences, savePreferences, type Preferences } from './lib/state/preferences'

  type ShellMode = 'loading' | 'launch' | 'login' | 'route-error' | 'ready'
  type AppRoute = Exclude<KataRoute, { kind: 'route-error' }>
  const requestActor = 'kata-web'

  if (window.location.pathname === '/') {
    history.replaceState(null, '', `/kata${window.location.search}${window.location.hash}`)
  }
  const launch = consumeLaunchFragment(window.location, history.replaceState.bind(history))
  const launchDaemonID = launch.daemonID
  const directDaemonTarget = launch.directTarget === true
  const selectedAuthentication = selectAuthenticationMode(launch)
  const initialRoute = parseRoute(new URL(window.location.href))
  let route = $state(initialRoute)
  let acceptedRoute = $state<AppRoute | undefined>()
  let returnPath = $state(launch.returnPath)
  let mode = $state(initialMode(launch, initialRoute, selectedAuthentication))
  let authority = $state<SnapshotState<UISnapshotIntent, UISnapshot> | undefined>()
  let references = $state<components['schemas']['UIReferencesResponseBody'] | undefined>()
  let mutationPending = $state(false)
  let mutationState = $state<MutationState>({ kind: 'idle' })
  let pendingCreate: { title: string; key: string } | undefined
  let pendingComment: { issueUID: string; body: string; key: string } | undefined
  let automaticSessionAttempted: 'loopback' | 'proxy' | undefined
  let advertisedAuthentication: 'loopback' | 'login' | 'proxy' | 'unavailable' | undefined
  let preferences = $state(loadPreferences())
  let versionMismatch = $state(false)
  let liveUpdatesReconnecting = $state(false)
  let daemonInfos = $state<WebDaemonInfo[]>([])
  let activeDaemonID = $state<string | undefined>()
  let daemonRosterLoaded = false
  let authenticationRecoveryPending = false
  let authenticationRecoveryCompletion: Promise<boolean> | undefined
  let daemonSwitching = $state(false)
  let daemonError = $state<string | undefined>()
  let referenceAbort: AbortController | undefined
  let referenceGeneration = 0
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
  const browserFetch = createCredentialedFetch(undefined, daemonFetch, requireAuthentication)
  const client = createKataClient(undefined, browserFetch)
  const snapshots = new SnapshotController(
    createUISnapshotRequest(browserFetch),
    uiSnapshotIntentKey,
  )
  const mutations = new MutationController({
    authority: () => ({
      canMutate: authority?.canMutate ?? false,
      actorPolicy: authority?.snapshot?.capabilities.actor_policy ?? 'readonly',
    }),
    refresh: refreshMutationAuthority,
    onAuthenticationRequired: requireAuthentication,
  })
  const invalidations = new InvalidationController((full) => refreshSnapshot(full))
  const stream = new EventStreamController({
    connect: (cursor, signal) => openEventStream(browserFetch, cursor, signal),
    onFrame: (frame) => invalidations.frame(frame),
    onAuthenticationRequired: requireAuthentication,
    onState: (state) => {
      liveUpdatesReconnecting = state === 'reconnecting'
    },
  })
  const scheduler = new RefreshScheduler({
    refresh: () => invalidations.refresh(),
    openEvents: () => {
      if (authority?.snapshot) stream.start(authority.cursor)
    },
  })
  const unsubscribe = snapshots.subscribe((state) => {
    authority = { ...state }
    if (
      state.snapshot &&
      !state.loading &&
      !state.authenticationRequired &&
      route.kind !== 'route-error'
    ) {
      mode = 'ready'
    }
  })

  $effect(() => {
    const selectedTheme = preferences.theme
    const media =
      typeof window.matchMedia === 'function'
        ? window.matchMedia('(prefers-color-scheme: dark)')
        : undefined
    const apply = () => {
      const dark = selectedTheme === 'dark' || (selectedTheme === 'system' && media?.matches)
      document.documentElement.classList.toggle('dark', dark === true)
    }
    apply()
    if (selectedTheme !== 'system') return
    media?.addEventListener('change', apply)
    return () => media?.removeEventListener('change', apply)
  })

  onMount(() => {
    const visibility = () => scheduler.visibilityChanged(!document.hidden)
    const focus = () => scheduler.focused()
    const environment = () => scheduler.environmentChanged()
    const showVersionMismatch = () => {
      versionMismatch = true
    }
    const popstate = () => {
      const next = parseRoute(new URL(window.location.href))
      route = next
      if (next.kind === 'route-error') {
        mode = 'route-error'
        return
      }
      mode = authority?.snapshot ? 'ready' : 'loading'
      void startAuthority()
    }
    document.addEventListener('visibilitychange', visibility)
    window.addEventListener('focus', focus)
    window.addEventListener('pageshow', environment)
    window.addEventListener('popstate', popstate)
    window.addEventListener('kata:versionMismatch', showVersionMismatch)
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
      scheduler.stop()
      stream.stop()
      invalidations.stop()
      snapshots.abort()
      referenceGeneration += 1
      referenceAbort?.abort()
      unsubscribe()
    }
  })

  function updatePreferences(next: Preferences): void {
    preferences = next
    savePreferences(next)
  }

  async function login(token: string, requestedPath: string): Promise<void> {
    const session = await exchangeLoginToken(token, requestedPath)
    await navigateAfterAuthentication(session.returnPath)
  }

  async function navigateAfterAuthentication(target: string): Promise<boolean> {
    const parsed = new URL(target, window.location.origin)
    const canonicalTarget =
      parsed.pathname === '/' ? `/kata${parsed.search}` : `${parsed.pathname}${parsed.search}`
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
    history.pushState(null, '', serializeRoute(next))
    route = next
    if (activeDaemonID) saveDaemonRoute(activeDaemonID, serializeRoute(next))
    mode = authority?.snapshot ? 'ready' : 'loading'
    void startAuthority()
  }

  async function createProject(name: string): Promise<{ changed: boolean }> {
    let changed = false
    const accepted = await runMutation({}, async (context) => {
      const result = await client.POST('/api/v1/projects', {
        body: context.body({ name }, requestActor),
      })
      if (result.data) changed = result.data.created
      return result
    })
    if (!accepted) throw new Error(mutationMessage(mutationState) ?? 'Could not create project.')
    return { changed }
  }

  async function designateInbox(projectUID: string): Promise<void> {
    const projects = authority?.snapshot?.catalog?.map(({ project }) => project) ?? []
    const target = projects.find((project) => project.uid === projectUID)
    if (!target) throw new Error('Inbox project is not available.')
    if (target.metadata.role === 'inbox') return

    const accepted = await runMutation(
      { draft: projectUID, revision: `"rev-${target.revision}"` },
      (context) =>
        client.POST('/api/v1/projects/{project_id}/metadata', {
          params: {
            path: { project_id: target.id },
            header: { 'If-Match': context.headers.get('If-Match')! },
          },
          body: { actor: requestActor, patch: { role: 'inbox' } },
        }),
    )
    if (!accepted) {
      throw new Error(mutationMessage(mutationState) ?? 'Could not designate the Inbox project.')
    }
  }

  async function createIssue(title: string): Promise<void> {
    const inbox = authority?.snapshot?.catalog?.find(
      ({ project }) => project.metadata.role === 'inbox',
    )?.project
    if (!inbox) throw new Error('Task inbox project is not available.')

    const create =
      pendingCreate?.title === title
        ? pendingCreate
        : { title, key: globalThis.crypto.randomUUID() }
    pendingCreate = create
    const accepted = await runMutation({ draft: title, createKey: create.key }, (context) =>
      client.POST('/api/v1/projects/{project_id}/issues', {
        params: {
          path: { project_id: inbox.id },
          header: { 'Idempotency-Key': create.key },
        },
        body: context.body({ title }, requestActor),
      }),
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
      const { data } = await client.GET('/api/v1/ui/references', {
        params: { query: { limit: 200 } },
        signal: abort.signal,
      })
      if (
        data &&
        !destroyed &&
        !abort.signal.aborted &&
        generation === referenceGeneration &&
        daemonID === activeDaemonID
      ) {
        references = data
      }
    } catch {
      // Snapshot authority remains usable when reference enrichment is canceled or unavailable.
    } finally {
      if (referenceAbort === abort) referenceAbort = undefined
    }
  }

  async function searchReferences(
    query: string,
  ): Promise<components['schemas']['UIIssueReference'][]> {
    const generation = referenceGeneration
    const daemonID = activeDaemonID
    const { data } = await client.GET('/api/v1/ui/references', {
      params: { query: { q: query, limit: 20 } },
    })
    if (destroyed || generation !== referenceGeneration || daemonID !== activeDaemonID) return []
    return data?.issues ?? []
  }

  async function editIssue(uid: string, patch: KataTaskEditPatch): Promise<boolean> {
    const target = selectedMutationTarget(uid)
    if (!target) return false
    const related = patch.links_delta?.add_related?.[0]
    if (related) {
      return runMutation({ draft: patch }, (context) =>
        client.POST('/api/v1/projects/{project_id}/issues/{ref}/links', {
          params: { path: target },
          body: context.body({ type: 'related', to_ref: related }, requestActor),
        }),
      )
    }
    const body: { title?: string; body?: string } = {}
    if (patch.title !== undefined) body.title = patch.title
    if (patch.body !== undefined) body.body = patch.body
    return runMutation({ draft: patch }, (context) =>
      client.PATCH('/api/v1/projects/{project_id}/issues/{ref}', {
        params: { path: target },
        body: context.body(body, requestActor),
      }),
    )
  }

  async function patchMetadata(uid: string, patch: Record<string, unknown>): Promise<boolean> {
    const target = selectedMutationTarget(uid)
    const revision = selectedRevision(uid)
    if (!target || !revision) return false
    return runMutation({ draft: patch, revision }, (context) =>
      client.POST('/api/v1/projects/{project_id}/issues/{ref}/metadata', {
        params: { path: target, header: { 'If-Match': context.headers.get('If-Match')! } },
        body: context.body({ patch }, requestActor),
      }),
    )
  }

  async function addComment(uid: string, body: string): Promise<boolean> {
    const target = selectedMutationTarget(uid)
    if (!target) return false
    const comment =
      pendingComment?.issueUID === uid && pendingComment.body === body
        ? pendingComment
        : { issueUID: uid, body, key: crypto.randomUUID() }
    pendingComment = comment
    const accepted = await runMutation({ draft: body }, (context) =>
      client.POST('/api/v1/projects/{project_id}/issues/{ref}/comments', {
        params: { path: target, header: { 'Idempotency-Key': comment.key } },
        body: context.body({ body }, requestActor),
      }),
    )
    if (accepted) pendingComment = undefined
    return accepted
  }

  async function createRecurrence(
    projectID: number,
    input: KataCreateRecurrenceInput,
  ): Promise<void> {
    const body: components['schemas']['CreateRecurrenceRequestBody'] = {
      initial_issue_ref: input.initialIssueRef,
      rrule: input.rrule,
      dtstart: input.dtstart,
      timezone: input.timezone,
      template: recurrenceTemplate(input.template),
    }
    const accepted = await runMutation({ draft: input }, (context) =>
      client.POST('/api/v1/projects/{project_id}/recurrences', {
        params: { path: { project_id: projectID } },
        body: context.body(body, input.actor || requestActor),
      }),
    )
    if (!accepted) throw new Error(mutationMessage(mutationState) ?? 'Could not create recurrence.')
  }

  async function patchRecurrence(
    id: number,
    input: KataPatchRecurrenceInput,
    etag: string,
  ): Promise<void> {
    const recurrence = authority?.snapshot?.selected?.recurrences?.find((item) => item.id === id)
    if (!recurrence) throw new Error(`Recurrence is not loaded: id=${id}`)
    const body: components['schemas']['PatchRecurrenceRequestBody'] = {}
    if (input.rrule !== undefined) body.rrule = input.rrule
    if (input.dtstart !== undefined) body.dtstart = input.dtstart
    if (input.timezone !== undefined) body.timezone = input.timezone
    if (input.template !== undefined) body.template = recurrenceTemplateUpdate(input.template)
    const accepted = await runMutation({ draft: input, revision: etag }, (context) =>
      client.PATCH('/api/v1/projects/{project_id}/recurrences/{recurrence_uid}', {
        params: {
          path: { project_id: recurrence.project_id, recurrence_uid: recurrence.uid },
          header: { 'If-Match': context.headers.get('If-Match')! },
        },
        body: context.body(body, input.actor || requestActor),
      }),
    )
    if (accepted) return
    if (mutationState.kind === 'revision-conflict') {
      const replacement = authority?.snapshot?.selected?.recurrences?.find(
        (item) => item.uid === recurrence.uid,
      )
      throw Object.assign(new Error(mutationState.detail), {
        status: 412,
        code: mutationState.code,
        ...(replacement
          ? { response: { recurrence: replacement, etag: `"rev-${replacement.revision}"` } }
          : {}),
      })
    }
    throw new Error(mutationMessage(mutationState) ?? 'Could not update recurrence.')
  }

  async function deleteRecurrence(recurrence: KataRecurrence): Promise<boolean> {
    return runMutation(
      { draft: recurrence, revision: `"rev-${recurrence.revision}"` },
      async (context) => {
        const result = await client.DELETE(
          '/api/v1/projects/{project_id}/recurrences/{recurrence_uid}',
          {
            params: {
              path: { project_id: recurrence.project_id, recurrence_uid: recurrence.uid },
              header: { 'If-Match': context.headers.get('If-Match')! },
              query:
                authority?.snapshot?.capabilities.actor_policy === 'identity'
                  ? {}
                  : { actor: requestActor },
            },
          },
        )
        return { ...result, data: result.response.ok ? true : undefined }
      },
    )
  }

  function recurrenceTemplate(
    input: KataCreateRecurrenceInput['template'],
  ): components['schemas']['RecurrenceTemplateInput'] {
    const template: components['schemas']['RecurrenceTemplateInput'] = {
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
  ): components['schemas']['RecurrenceTemplateUpdateInput'] {
    const template: components['schemas']['RecurrenceTemplateUpdateInput'] = {}
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
      client.POST('/api/v1/projects/{project_id}/issues/{ref}/actions/move', {
        params: { path: target, header: { 'If-Match': context.headers.get('If-Match')! } },
        body: context.body({ to_project_uid: toProjectUID }, requestActor),
      }),
    )
  }

  async function assignOwner(uid: string, owner: string): Promise<boolean> {
    const target = selectedMutationTarget(uid)
    if (!target) return false
    return runMutation({ draft: owner }, (context) =>
      client.POST('/api/v1/projects/{project_id}/issues/{ref}/actions/assign', {
        params: { path: target },
        body: context.body({ owner }, requestActor),
      }),
    )
  }

  async function unassignOwner(uid: string): Promise<boolean> {
    const target = selectedMutationTarget(uid)
    if (!target) return false
    return runMutation({}, (context) =>
      client.POST('/api/v1/projects/{project_id}/issues/{ref}/actions/unassign', {
        params: { path: target },
        body: context.body({}, requestActor),
      }),
    )
  }

  async function setPriority(uid: string, priority: number | null): Promise<boolean> {
    const target = selectedMutationTarget(uid)
    if (!target) return false
    return runMutation({ draft: priority }, (context) =>
      client.POST('/api/v1/projects/{project_id}/issues/{ref}/actions/priority', {
        params: { path: target },
        body: context.body(priority === null ? {} : { priority }, requestActor),
      }),
    )
  }

  async function addLabel(uid: string, label: string): Promise<boolean> {
    const target = selectedMutationTarget(uid)
    if (!target) return false
    return runMutation({ draft: label }, (context) =>
      client.POST('/api/v1/projects/{project_id}/issues/{ref}/labels', {
        params: { path: target },
        body: context.body({ label }, requestActor),
      }),
    )
  }

  async function removeLabel(uid: string, label: string): Promise<void> {
    const target = selectedMutationTarget(uid)
    if (!target) return
    await runMutation({ draft: label }, () =>
      client.DELETE('/api/v1/projects/{project_id}/issues/{ref}/labels/{label}', {
        params: {
          path: { ...target, label },
          query:
            authority?.snapshot?.capabilities.actor_policy === 'identity'
              ? {}
              : { actor: requestActor },
        },
      }),
    )
  }

  async function closeIssue(request: KataTaskCloseRequest): Promise<boolean> {
    const selected = selectedIssue()
    const target = selected ? selectedMutationTarget(selected.uid) : undefined
    if (!target) return false
    return runMutation({ draft: request }, (context) =>
      client.POST('/api/v1/projects/{project_id}/issues/{ref}/actions/close', {
        params: { path: target },
        body: context.body({ ...request }, requestActor),
      }),
    )
  }

  async function reopenIssue(): Promise<void> {
    const selected = selectedIssue()
    const target = selected ? selectedMutationTarget(selected.uid) : undefined
    if (!target) return
    await runMutation({}, (context) =>
      client.POST('/api/v1/projects/{project_id}/issues/{ref}/actions/reopen', {
        params: { path: target },
        body: context.body({}, requestActor),
      }),
    )
  }

  function deleteIssue(): Promise<boolean> {
    return closeIssue({
      reason: 'wontfix',
      message:
        'Removed from active work through the web issue detail because this task is no longer intended to be completed.',
      evidence: [],
    })
  }

  async function runMutation<T>(
    options: { draft?: unknown; revision?: string; createKey?: string },
    mutate: (context: MutationContext) => Promise<MutationResult<T>>,
  ): Promise<boolean> {
    mutationPending = true
    try {
      const result = await mutations.execute(options, mutate)
      mutationState = { ...mutations.state }
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
    if (state.kind === 'blocked') return 'This issue is read-only or its authority is stale.'
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

  function requireAuthentication(): void {
    if (authenticationRecoveryPending) return
    clearSessionCredentials()
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
      return
    }
    mode = authenticationView(selectedAuthentication)
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
        authenticationRecoveryPending = false
        return navigateAfterAuthentication(session.returnPath)
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
    return accepted
  }

  async function loadDaemonRoster(): Promise<boolean> {
    if (daemonRosterLoaded) return directDaemonTarget || activeDaemonID !== undefined
    if (directDaemonTarget) {
      daemonRosterLoaded = true
      daemonInfos = []
      activeDaemonID = undefined
      return true
    }
    try {
      const loadedDaemons = await fetchWebDaemons(browserFetch)
      daemonInfos = loadedDaemons
      daemonRosterLoaded = true
      activeDaemonID =
        daemonInfos.find(
          (daemon) => daemon.id === launchDaemonID && daemon.health !== 'upgrade_required',
        )?.id ??
        daemonInfos.find((daemon) => daemon.default && daemon.health !== 'upgrade_required')?.id ??
        daemonInfos.find((daemon) => daemon.health !== 'upgrade_required')?.id
      if (!activeDaemonID) {
        snapshots.clear()
        daemonError = 'No compatible Kata daemon is available'
        return false
      }
      daemonError = undefined
      if (activeDaemonID && window.location.pathname === '/kata' && !window.location.search) {
        const persisted = loadDaemonRoute(activeDaemonID)
        if (persisted) {
          history.replaceState(null, '', persisted)
          const restored = parseRoute(new URL(window.location.href))
          if (restored.kind !== 'route-error') route = restored
        }
      }
    } catch {
      // A malformed gateway response cannot own daemon selection. The direct
      // local authority path remains available so the recovery UI can render.
      if (!daemonRosterLoaded) {
        daemonInfos = []
        activeDaemonID = undefined
      }
    }
    return true
  }

  async function switchDaemon(id: string): Promise<void> {
    if (daemonSwitching || authenticationRecoveryPending || id === activeDaemonID) return
    if (daemonInfos.find((daemon) => daemon.id === id)?.health === 'upgrade_required') return
    const sourceDaemon = activeDaemonID
    const sourceRoute = route.kind === 'route-error' ? acceptedRoute : route
    if (sourceDaemon && sourceRoute) saveDaemonRoute(sourceDaemon, serializeRoute(sourceRoute))
    const restoredPath = loadDaemonRoute(id) ?? '/kata?view=all-open'
    const restored = parseRoute(new URL(restoredPath, window.location.origin))
    if (restored.kind === 'route-error') return

    daemonSwitching = true
    daemonError = undefined
    scheduler.stop()
    stream.stop()
    invalidations.pause()
    snapshots.clear()
    referenceGeneration += 1
    referenceAbort?.abort()
    references = undefined
    activeDaemonID = id
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

    daemonError = `Could not connect to ${id}`
    if (sourceDaemon && sourceRoute) {
      activeDaemonID = sourceDaemon
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
    const requestedRoute = route
    const baseIntent = snapshotIntentForRoute(requestedRoute)
    const intent = activeDaemonID ? { ...baseIntent, daemonID: activeDaemonID } : baseIntent
    const accepted = await snapshots.load(intent, { full })
    if (snapshots.state.authenticationRequired) {
      requireAuthentication()
      return false
    }
    if (
      snapshots.state.snapshot &&
      snapshots.state.intent &&
      uiSnapshotIntentKey(snapshots.state.intent) === uiSnapshotIntentKey(intent)
    ) {
      acceptedRoute = requestedRoute
      return accepted
    }
    return false
  }
</script>

<main aria-labelledby="kata-heading" class="kata-app" class:ready={mode === 'ready'}>
  <h1 id="kata-heading" class:visually-hidden={mode === 'ready'}>Kata</h1>
  {#if versionMismatch}
    <VersionMismatch />
  {:else if mode === 'loading'}
    {#if daemonError && !activeDaemonID && daemonInfos.length > 0}
      <section class="kata-authority-recovery" role="alert">
        <span>{daemonError}</span>
        <KataDaemonSwitcher
          daemons={daemonInfos}
          activeId={activeDaemonID}
          activeStatusLabel={daemonError}
          activeStatusTone="error"
          disabled={daemonSwitching}
          onSelect={(id) => void switchDaemon(id)}
        />
      </section>
    {:else if authority?.error && !authority.loading}
      <section class="kata-authority-recovery" role="alert">
        <span>{authority.error}</span>
        <button type="button" onclick={() => void startAuthority()}>Retry Kata snapshot</button>
        {#if daemonInfos.length > 0}
          <KataDaemonSwitcher
            daemons={daemonInfos}
            activeId={activeDaemonID}
            activeStatusLabel={daemonError}
            activeStatusTone={daemonError ? 'error' : undefined}
            disabled={daemonSwitching}
            onSelect={(id) => void switchDaemon(id)}
          />
        {/if}
      </section>
    {:else}
      <p role="status">Loading Kata…</p>
    {/if}
  {:else if route.kind === 'route-error'}
    <RouteError {route} onSearch={search} />
  {:else}
    {#if mode === 'launch'}
      <LaunchHint issueRef={undefined} />
    {:else if mode === 'login'}
      <LoginView {returnPath} onLogin={login} />
    {/if}
    {#if !authority?.snapshot && daemonInfos.length > 0 && (mode === 'launch' || mode === 'login')}
      <KataDaemonSwitcher
        daemons={daemonInfos}
        activeId={activeDaemonID}
        activeStatusLabel={daemonError}
        activeStatusTone={daemonError ? 'error' : undefined}
        disabled={daemonSwitching}
        onSelect={(id) => void switchDaemon(id)}
      />
    {/if}
    {#if authority?.snapshot && (mode === 'ready' || mode === 'launch' || mode === 'login')}
      <AppShell
        route={acceptedRoute ?? route}
        snapshot={authority.snapshot}
        loading={authority.loading}
        canMutate={authority.canMutate}
        {mutationPending}
        mutationMessage={mutationMessage(mutationState)}
        draftResetGeneration={authority.cursor}
        {preferences}
        daemons={daemonInfos}
        {activeDaemonID}
        {daemonSwitching}
        reconnecting={liveUpdatesReconnecting}
        stale={authority.stale}
        readOnly={!authority.snapshot.capabilities.writable}
        {daemonError}
        onSelectDaemon={(id) => void switchDaemon(id)}
        onPreferencesChange={updatePreferences}
        ownerOptions={(references?.owners ?? []).map((owner) => ({ name: owner, label: owner }))}
        onNavigate={navigate}
        onCreateProject={createProject}
        onDesignateInbox={designateInbox}
        onCreateIssue={createIssue}
        {searchReferences}
        onMoveIssue={moveIssue}
        onPatchMetadata={patchMetadata}
        onAddComment={addComment}
        onEditIssue={editIssue}
        onAssignOwner={assignOwner}
        onUnassignOwner={unassignOwner}
        onSetPriority={setPriority}
        onAddLabel={addLabel}
        onRemoveLabel={removeLabel}
        onCloseIssue={closeIssue}
        onReopenIssue={reopenIssue}
        onDeleteIssue={deleteIssue}
        onCreateRecurrence={createRecurrence}
        onPatchRecurrence={patchRecurrence}
        onDeleteRecurrence={deleteRecurrence}
      />
    {/if}
  {/if}
</main>
