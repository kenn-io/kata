import { isAuthenticationRequiredError } from '../auth/session'

export interface MutationAuthority {
  canMutate: boolean
  actorPolicy: string
}

export interface MutationContext {
  headers: Headers
  body<T extends object>(body: T, requestActor?: string): T | (T & { actor: string })
}

export interface MutationResult {
  data?: unknown
  status: number
  headers: Headers
}

export interface MutationOptions<TDraft = unknown> {
  revision?: string
  createKey?: string
  draft?: TDraft
}

export type MutationState<TDraft = unknown> =
  | { kind: 'idle' }
  | { kind: 'pending'; draft?: TDraft }
  | { kind: 'blocked'; draft?: TDraft }
  | { kind: 'revision-conflict'; code: string; detail: string; draft?: TDraft }
  | { kind: 'domain-error'; code: string; detail: string; draft?: TDraft }
  | { kind: 'uncertain'; draft?: TDraft }

interface ControllerOptions {
  authority: () => MutationAuthority
  refresh: () => Promise<boolean>
  onAuthenticationRequired?: () => void
}

interface MutationError {
  code: string
  detail: string
}

export class MutationController {
  readonly #authority: () => MutationAuthority
  readonly #refresh: () => Promise<boolean>
  readonly #onAuthenticationRequired: () => void
  readonly #committedCreates = new Map<string, unknown>()

  state: MutationState = { kind: 'idle' }

  constructor(options: ControllerOptions) {
    this.#authority = options.authority
    this.#refresh = options.refresh
    this.#onAuthenticationRequired = options.onAuthenticationRequired ?? (() => undefined)
  }

  async execute<T, TDraft = unknown>(
    options: MutationOptions<TDraft>,
    mutate: (context: MutationContext) => Promise<MutationResult>,
  ): Promise<T | false> {
    if (options.createKey && this.#committedCreates.has(options.createKey)) {
      return this.#committedCreates.get(options.createKey) as T
    }

    const authority = this.#authority()
    if (!authority.canMutate) {
      this.state = withDraft({ kind: 'blocked' }, options.draft)
      return false
    }

    this.state = withDraft({ kind: 'pending' }, options.draft)
    const headers = new Headers()
    if (options.revision) headers.set('If-Match', options.revision)
    const context: MutationContext = {
      headers,
      body: (body, requestActor) =>
        authority.actorPolicy === 'identity' || !requestActor
          ? body
          : { ...body, actor: requestActor },
    }

    let result: MutationResult
    try {
      result = await mutate(context)
    } catch (error) {
      if (isAuthenticationRequiredError(error)) {
        this.#onAuthenticationRequired()
        this.state = withDraft({ kind: 'blocked' }, options.draft)
        return false
      }
      await this.#refreshAfterUncertainWrite()
      this.state = withDraft({ kind: 'uncertain' }, options.draft)
      return false
    }

    if (result.status < 200 || result.status >= 300 || result.data === undefined) {
      const error = mutationError(result.data, result.status)
      if (result.status === 401) {
        this.#onAuthenticationRequired()
        this.state = withDraft({ kind: 'blocked' }, options.draft)
      } else if (result.status === 412 || error.code === 'revision_conflict') {
        await this.#refreshAfterUncertainWrite()
        this.state = withDraft(
          { kind: 'revision-conflict', code: error.code, detail: error.detail },
          options.draft,
        )
      } else if (result.status >= 400 && result.status < 500) {
        this.state = withDraft(
          { kind: 'domain-error', code: error.code, detail: error.detail },
          options.draft,
        )
      } else {
        await this.#refreshAfterUncertainWrite()
        this.state = withDraft({ kind: 'uncertain' }, options.draft)
      }
      return false
    }

    if (options.createKey) this.#committedCreates.set(options.createKey, result.data)
    this.state = { kind: 'idle' }
    await this.#refreshAfterUncertainWrite()
    return result.data as T
  }

  async #refreshAfterUncertainWrite(): Promise<void> {
    try {
      await this.#refresh()
    } catch {
      // The caller retains the accepted snapshot and exposes its stale state.
    }
  }
}

function mutationError(error: unknown, status: number): MutationError {
  const source = objectValue(error, 'error') ?? asObject(error)
  return {
    code: stringValue(source, 'code') ?? `http_${status}`,
    detail:
      stringValue(source, 'detail') ??
      stringValue(source, 'message') ??
      stringValue(source, 'title') ??
      `HTTP ${status}`,
  }
}

function withDraft<T extends object, TDraft>(
  state: T,
  draft: TDraft | undefined,
): T & { draft?: TDraft } {
  return draft === undefined ? state : { ...state, draft }
}

function asObject(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined
}

function objectValue(value: unknown, key: string): Record<string, unknown> | undefined {
  return asObject(asObject(value)?.[key])
}

function stringValue(value: Record<string, unknown> | undefined, key: string): string | undefined {
  const candidate = value?.[key]
  return typeof candidate === 'string' ? candidate : undefined
}
