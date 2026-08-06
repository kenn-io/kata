import type { components } from '../api/schema'
import { createCredentialedFetch } from '../api/client'
import { AuthenticationRequiredError, isAuthenticationRequiredError } from '../auth/session'
import type { KataRoute } from '../router'

export interface SnapshotAuthority {
  cursor: number
  capabilities: {
    writable: boolean
    updates: 'sse' | 'poll'
  }
}

export interface SnapshotRequestOptions {
  signal: AbortSignal
  etag?: string
  full: boolean
}

export type SnapshotResponse<TSnapshot> =
  | { status: 200; etag: string; snapshot: TSnapshot }
  | { status: 304; etag?: string }

export type SnapshotRequest<TIntent, TSnapshot> = (
  intent: TIntent,
  options: SnapshotRequestOptions,
) => Promise<SnapshotResponse<TSnapshot>>

export interface SnapshotState<TIntent, TSnapshot extends SnapshotAuthority> {
  generation: number
  intent: TIntent | undefined
  snapshot: TSnapshot | undefined
  etag: string | undefined
  cursor: number
  loading: boolean
  stale: boolean
  error: string | undefined
  canMutate: boolean
  authenticationRequired: boolean
}

type StateListener<TIntent, TSnapshot extends SnapshotAuthority> = (
  state: Readonly<SnapshotState<TIntent, TSnapshot>>,
) => void

export class SnapshotController<TIntent, TSnapshot extends SnapshotAuthority> {
  readonly #request: SnapshotRequest<TIntent, TSnapshot>
  readonly #key: (intent: TIntent) => string
  readonly #listeners = new Set<StateListener<TIntent, TSnapshot>>()
  #abort: AbortController | undefined
  #acceptedKey = ''

  state: SnapshotState<TIntent, TSnapshot> = {
    generation: 0,
    intent: undefined,
    snapshot: undefined,
    etag: undefined,
    cursor: 0,
    loading: false,
    stale: false,
    error: undefined,
    canMutate: false,
    authenticationRequired: false,
  }

  constructor(request: SnapshotRequest<TIntent, TSnapshot>, key: (intent: TIntent) => string) {
    this.#request = request
    this.#key = key
  }

  subscribe(listener: StateListener<TIntent, TSnapshot>): () => void {
    this.#listeners.add(listener)
    return () => this.#listeners.delete(listener)
  }

  async load(intent: TIntent, options: { full?: boolean } = {}): Promise<boolean> {
    const generation = this.state.generation + 1
    const requestedKey = this.#key(intent)
    this.#abort?.abort()
    const abort = new AbortController()
    this.#abort = abort
    const sameAuthority = requestedKey === this.#acceptedKey
    this.#setState({
      ...this.state,
      generation,
      loading: true,
      error: undefined,
      canMutate: false,
      authenticationRequired: false,
    })

    try {
      const requestOptions: SnapshotRequestOptions = {
        signal: abort.signal,
        full: options.full === true,
      }
      if (sameAuthority && this.state.etag && !requestOptions.full) {
        requestOptions.etag = this.state.etag
      }
      const response = await this.#request(intent, requestOptions)
      if (abort.signal.aborted || generation !== this.state.generation) return false

      if (response.status === 304) {
        if (requestOptions.full || !sameAuthority || !this.state.snapshot) {
          this.#setFailure(generation)
          return false
        }
        this.#setState({
          ...this.state,
          loading: false,
          stale: false,
          error: undefined,
          etag: response.etag ?? this.state.etag,
          canMutate: this.state.snapshot.capabilities.writable,
          authenticationRequired: false,
        })
        return true
      }

      this.#acceptedKey = requestedKey
      const snapshot = Object.freeze(response.snapshot)
      this.#setState({
        generation,
        intent,
        snapshot,
        etag: response.etag,
        cursor: snapshot.cursor,
        loading: false,
        stale: false,
        error: undefined,
        canMutate: snapshot.capabilities.writable,
        authenticationRequired: false,
      })
      return true
    } catch (error) {
      if (abort.signal.aborted || generation !== this.state.generation) return false
      if (isAuthenticationRequiredError(error)) {
        this.#setAuthenticationRequired(generation)
        return false
      }
      this.#setFailure(generation)
      return false
    }
  }

  abort(): void {
    this.#abort?.abort()
  }

  clear(): void {
    this.#abort?.abort()
    this.#abort = undefined
    this.#acceptedKey = ''
    this.#setState({
      generation: this.state.generation + 1,
      intent: undefined,
      snapshot: undefined,
      etag: undefined,
      cursor: 0,
      loading: false,
      stale: false,
      error: undefined,
      canMutate: false,
      authenticationRequired: false,
    })
  }

  markAuthenticationRequired(): void {
    this.#abort?.abort()
    this.#setAuthenticationRequired(this.state.generation)
  }

  #setFailure(generation: number): void {
    this.#setState({
      ...this.state,
      generation,
      loading: false,
      stale: this.state.snapshot !== undefined,
      error: 'Snapshot unavailable',
      canMutate: false,
      authenticationRequired: false,
    })
  }

  #setAuthenticationRequired(generation: number): void {
    this.#setState({
      ...this.state,
      generation,
      loading: false,
      stale: this.state.snapshot !== undefined,
      error: 'Authentication required',
      canMutate: false,
      authenticationRequired: true,
    })
  }

  #setState(state: SnapshotState<TIntent, TSnapshot>): void {
    this.state = state
    for (const listener of this.#listeners) listener(state)
  }
}

export type UISnapshot = components['schemas']['UISnapshotResponseBody']

export interface UISnapshotIntent {
  daemonID?: string
  view: string
  projectUID?: string
  statuses: string[]
  owners: string[]
  labels: string[]
  relationships: string[]
  text?: string
  selectedIssueUID?: string
  includeGraph: boolean
  includeHistory: boolean
  localDate?: string
  timeZone?: string
}

export function snapshotIntentForRoute(
  route: Exclude<KataRoute, { kind: 'route-error' }>,
  now: Date = new Date(),
  timeZone: string = Intl.DateTimeFormat().resolvedOptions().timeZone,
): UISnapshotIntent {
  const view = route.view ?? (route.projectUID || route.issueUID ? 'all-open' : 'inbox')
  const intent: UISnapshotIntent = {
    view,
    statuses: [...route.filters.status],
    owners: [...route.filters.owner],
    labels: [...route.filters.label],
    relationships: [...route.filters.relationship],
    includeGraph: Boolean(route.issueUID) && route.graph,
    includeHistory: Boolean(route.issueUID),
  }
  if (route.projectUID) intent.projectUID = route.projectUID
  if (route.issueUID) intent.selectedIssueUID = route.issueUID
  if (route.filters.text) intent.text = route.filters.text
  if (['today', 'upcoming', 'deadlines'].includes(intent.view)) {
    intent.localDate = localDate(now, timeZone)
    intent.timeZone = timeZone
  }
  return intent
}

export function uiSnapshotIntentKey(intent: UISnapshotIntent): string {
  return JSON.stringify(intent)
}

export function createUISnapshotRequest(
  fetcher: typeof fetch = createCredentialedFetch(),
): SnapshotRequest<UISnapshotIntent, UISnapshot> {
  return async (intent, options) => {
    const query = new URLSearchParams({
      view: intent.view,
      include_graph: String(intent.includeGraph),
      include_history: String(intent.includeHistory),
    })
    appendValues(query, 'status', intent.statuses)
    appendValues(query, 'owner', intent.owners)
    appendValues(query, 'label', intent.labels)
    appendValues(query, 'relationship', intent.relationships)
    if (intent.projectUID) query.set('project_uid', intent.projectUID)
    if (intent.text) query.set('text', intent.text)
    if (intent.selectedIssueUID) query.set('selected_issue_uid', intent.selectedIssueUID)
    if (intent.localDate) query.set('local_date', intent.localDate)
    if (intent.timeZone) query.set('time_zone', intent.timeZone)
    const headers = new Headers()
    if (options.etag) headers.set('If-None-Match', options.etag)
    const response = await fetcher(`/api/v1/ui/snapshot?${query}`, {
      method: 'GET',
      headers,
      signal: options.signal,
    })
    if (response.status === 304) {
      const etag = response.headers.get('ETag')
      return etag ? { status: 304, etag } : { status: 304 }
    }
    if (response.status === 401) throw new AuthenticationRequiredError('Snapshot unavailable')
    if (!response.ok) throw new Error('Snapshot unavailable')
    return {
      status: 200,
      etag: response.headers.get('ETag') ?? '',
      snapshot: (await response.json()) as UISnapshot,
    }
  }
}

function appendValues(query: URLSearchParams, key: string, values: readonly string[]): void {
  for (const value of values) query.append(key, value)
}

function localDate(now: Date, timeZone: string): string {
  const parts = new Intl.DateTimeFormat('en', {
    timeZone,
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
  }).formatToParts(now)
  const value = (type: Intl.DateTimeFormatPartTypes) =>
    parts.find((part) => part.type === type)?.value ?? ''
  return `${value('year')}-${value('month')}-${value('day')}`
}
