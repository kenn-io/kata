import { CronClient, CronReadError, type CronDefinitionRecord } from './client'
import { serializeCronJobDraft, type CronDefinitionDraft } from './jobDraft'
import {
  parseCronDocument,
  rawCronValue,
  sameCronDefinition,
  type RawCronDocument,
} from './document'
import { rawApiFetch } from '../api/client'
import { AuthenticationRequiredError, isAuthenticationRequiredError } from '../auth/session'
import type { MutationContext, MutationResult } from '../mutations/controller'

export interface CronAttempt {
  kind: 'job' | 'workflow'
  operation: 'save' | 'delete'
  projectID: number
  uid: string
  body: string
  expectedEventUID: string
  creating: boolean
}
export interface CronMutationOutcome {
  kind: 'saved' | 'deleted' | 'retryable' | 'conflict' | 'uncertain' | 'blocked'
  record?: CronDefinitionRecord
}
export interface CronMutationOptions {
  onMutation: (
    draft: CronAttempt,
    mutate: (context: MutationContext) => Promise<MutationResult>,
  ) => Promise<boolean>
  actor?: () => string | undefined
  /** Stable authority identity; changing it permanently fences existing attempts. */
  scope?: () => unknown
}

/** Call after resolving references; the returned attempt is never rebuilt for retry. */
export function freezeCronAttempt(
  draft: CronDefinitionDraft,
  operation: 'save' | 'delete',
  document?: RawCronDocument,
): CronAttempt {
  if (operation === 'delete' && (draft.kind !== 'job' || !draft.definitionEventUID))
    throw new Error('Only an existing job can be deleted')
  const guard = draft.definitionEventUID
    ? `"expected_event_uid":${JSON.stringify(draft.definitionEventUID)}`
    : ''
  let body: string
  if (operation === 'delete') body = `{${guard}}`
  else {
    if (!document && draft.kind === 'workflow')
      throw new Error('A serialized workflow document is required')
    const intended = document ?? serializeCronJobDraft(draft)
    // Validate the raw value, then splice it verbatim into the request envelope.
    parseCronDocument(intended.source)
    // Go strings.TrimSpace uses Unicode White_Space, including NEL but excluding BOM.
    const name = (draft.values.name ?? draft.name).replace(
      /^\p{White_Space}+|\p{White_Space}+$/gu,
      '',
    )
    body = `{"name":${JSON.stringify(name)},"definition":${intended.source}${guard ? ',' + guard : ''}}`
  }
  return Object.freeze({
    kind: draft.kind,
    operation,
    projectID: draft.projectID,
    uid: draft.uid,
    body,
    expectedEventUID: draft.definitionEventUID,
    creating: !draft.definitionEventUID,
  })
}

interface AttemptState {
  scope: unknown
  body?: string
  outcome?: CronMutationOutcome
  sent: boolean
}

/** Reconciliation never sends a write. Only a later explicit execute may retry. */
export class CronMutations {
  readonly #options: CronMutationOptions
  readonly #reader = new CronClient()
  readonly #attempts = new WeakMap<CronAttempt, AttemptState>()
  #pending = false

  constructor(options: CronMutationOptions) {
    this.#options = options
  }

  execute(attempt: CronAttempt, signal: AbortSignal): Promise<CronMutationOutcome> {
    return this.#run(attempt, signal, false)
  }

  reconcile(attempt: CronAttempt, signal: AbortSignal): Promise<CronMutationOutcome> {
    return this.#run(attempt, signal, true)
  }

  async #run(
    attempt: CronAttempt,
    signal: AbortSignal,
    readOnly: boolean,
  ): Promise<CronMutationOutcome> {
    if (this.#pending || signal.aborted) return { kind: 'blocked' }
    const scope = this.#options.scope?.()
    let state = this.#attempts.get(attempt)
    if (!state) {
      state = { scope, sent: false }
      this.#attempts.set(attempt, state)
    }
    if (state.scope !== scope) return { kind: 'blocked' }
    const retained = state
    let outcome: CronMutationOutcome = { kind: 'blocked' }
    let activeSignal = signal
    const fenced = () => activeSignal.aborted || retained.scope !== this.#options.scope?.()
    this.#pending = true
    try {
      const accepted = await this.#options.onMutation(attempt, async (context) => {
        activeSignal = context.signal ? AbortSignal.any([signal, context.signal]) : signal
        if (fenced()) return responseFor(outcome)
        // Actor selection uses the common authority policy exactly once. Only new
        // envelope metadata is encoded; original definition values stay raw.
        if (retained.body === undefined) {
          const metadata = JSON.stringify(context.body({}, this.#options.actor?.()))
          retained.body =
            metadata === '{}' ? attempt.body : attempt.body.slice(0, -1) + ',' + metadata.slice(1)
        }
        try {
          if (
            retained.outcome &&
            ['saved', 'deleted', 'conflict'].includes(retained.outcome.kind)
          ) {
            outcome = retained.outcome
          } else if (readOnly || retained.sent) {
            outcome = await this.#readBack(attempt, activeSignal)
            if (!readOnly && outcome.kind === 'retryable' && !fenced())
              outcome = await this.#send(attempt, retained, context, activeSignal)
          } else {
            outcome = await this.#send(attempt, retained, context, activeSignal)
          }
        } catch (error) {
          if (error instanceof CronDomainError) {
            outcome = { kind: 'blocked' }
            return { status: error.status, data: { error: error.error }, headers: new Headers() }
          }
          if (isAuthenticationRequiredError(error)) {
            outcome = { kind: 'blocked' }
            throw error // Existing controller owns credential rejection.
          }
          outcome = { kind: fenced() ? 'blocked' : 'uncertain' }
        }
        if (fenced()) outcome = { kind: 'blocked' }
        retained.outcome = outcome
        return responseFor(outcome)
      })
      // The shared controller refreshes authority after the callback. Keep its
      // combined cancellation fence live until that final await has settled.
      if (fenced() || (!accepted && (outcome.kind === 'saved' || outcome.kind === 'deleted'))) {
        outcome = { kind: 'blocked' }
        retained.outcome = outcome
      }
      return outcome
    } finally {
      this.#pending = false
    }
  }

  async #send(
    attempt: CronAttempt,
    state: AttemptState,
    context: MutationContext,
    signal: AbortSignal,
  ): Promise<CronMutationOutcome> {
    state.sent = true
    let response: Response
    try {
      const headers = new Headers(context.headers)
      headers.delete('If-Match')
      headers.set('Content-Type', 'application/json')
      response = await rawApiFetch(
        `/api/v1/projects/${attempt.projectID}/cron/${attempt.kind}s/${encodeURIComponent(attempt.uid)}`,
        {
          method: attempt.operation === 'delete' ? 'DELETE' : 'PUT',
          body: state.body!,
          headers,
          signal,
        },
      )
    } catch (error) {
      if (isAuthenticationRequiredError(error) || signal.aborted) throw error
      return this.#readBack(attempt, signal)
    }
    if (signal.aborted) return { kind: 'blocked' }
    if (response.status === 401) {
      // Keep the common controller's authentication transition for raw fetch
      // implementations that return a 401 instead of rejecting it.
      throw new AuthenticationRequiredError('Authentication required')
    }
    if (response.ok) {
      try {
        const record = readRecord(attempt.kind, await response.text())
        return { kind: attempt.operation === 'delete' ? 'deleted' : 'saved', record }
      } catch {
        return this.#readBack(attempt, signal)
      }
    }
    let text: string
    try {
      text = await response.text()
    } catch {
      return this.#readBack(attempt, signal)
    }
    const error: { code: string; detail: string } = {
      code: `http_${response.status}`,
      detail: `HTTP ${response.status}`,
    }
    try {
      const doc = parseCronDocument(text)
      for (const key of ['code', 'detail'] as const) {
        const raw =
          rawCronValue(doc, ['error', key]) ??
          rawCronValue(doc, [key]) ??
          (key === 'detail'
            ? (rawCronValue(doc, ['error', 'message']) ?? rawCronValue(doc, ['message']))
            : undefined)
        if (raw !== undefined) {
          const value: unknown = JSON.parse(raw)
          if (typeof value === 'string') error[key] = value
        }
      }
    } catch {
      /* A malformed failure response cannot establish a committed write. */
    }
    if (response.status === 409 && error.code === 'cron_conflict') {
      try {
        const doc = parseCronDocument(text)
        const raw = rawCronValue(doc, ['error', 'data', 'current'])
        if (raw)
          return {
            kind: 'conflict',
            record: readRecord(attempt.kind, `{"${attempt.kind}":${raw}}`),
          }
      } catch {
        /* Fall back to a fresh read when no usable current record was supplied. */
      }
      const current = await this.#readBack(attempt, signal)
      return { kind: 'conflict', ...(current.record ? { record: current.record } : {}) }
    }
    if (response.status >= 400 && response.status < 500) {
      // Preserve unrelated domain errors through the controller unchanged.
      state.outcome = { kind: 'blocked' }
      throw new CronDomainError(response.status, error)
    }
    return this.#readBack(attempt, signal)
  }

  async #readBack(attempt: CronAttempt, signal: AbortSignal): Promise<CronMutationOutcome> {
    try {
      const record = await this.#reader.readDefinition(
        attempt.kind,
        attempt.projectID,
        attempt.uid,
        signal,
      )
      if (signal.aborted) return { kind: 'blocked' }
      if (attempt.operation === 'delete') {
        if (record.deletedAt) return { kind: 'deleted', record }
      } else if (!record.deletedAt) {
        const envelope = parseCronDocument(attempt.body)
        const name = rawCronValue(envelope, ['name'])!
        const definition = parseCronDocument(rawCronValue(envelope, ['definition'])!)
        if (
          record.name === JSON.parse(name) &&
          sameCronDefinition(attempt.kind, definition, record.document)
        )
          return { kind: 'saved', record }
      }
      return {
        kind:
          !record.deletedAt &&
          !attempt.creating &&
          record.definitionEventUID === attempt.expectedEventUID
            ? 'retryable'
            : 'conflict',
        record,
      }
    } catch (error) {
      if (isAuthenticationRequiredError(error)) throw error
      if (signal.aborted) return { kind: 'blocked' }
      if (error instanceof CronReadError && error.status === 404)
        return { kind: attempt.creating ? 'retryable' : 'conflict' }
      return { kind: 'uncertain' }
    }
  }
}

class CronDomainError extends Error {
  constructor(
    readonly status: number,
    readonly error: { code: string; detail: string },
  ) {
    super(error.detail)
  }
}
function responseFor(outcome: CronMutationOutcome): MutationResult {
  const status =
    outcome.kind === 'conflict'
      ? 409
      : outcome.kind === 'uncertain' || outcome.kind === 'retryable'
        ? 503
        : outcome.kind === 'blocked'
          ? 403
          : 200
  return {
    status,
    headers: new Headers(),
    data:
      outcome.kind === 'conflict'
        ? { error: { code: 'cron_conflict', detail: 'The cron definition changed.' } }
        : outcome,
  }
}
function readRecord(kind: 'job' | 'workflow', source: string): CronDefinitionRecord {
  const envelope = parseCronDocument(source)
  const scalar = (key: string) => {
    const raw = rawCronValue(envelope, [kind, key])
    const value: unknown = raw === undefined ? undefined : JSON.parse(raw)
    if (typeof value !== 'string') throw new Error('Malformed cron response')
    return value
  }
  const definition = rawCronValue(envelope, [kind, 'definition'])
  if (!definition) throw new Error('Malformed cron response')
  const deleted = rawCronValue(envelope, [kind, 'deleted_at'])
  return {
    uid: scalar('uid'),
    name: scalar('name'),
    definitionEventUID: scalar('definition_event_uid'),
    document: parseCronDocument(definition),
    ...(deleted && deleted !== 'null' ? { deletedAt: scalar('deleted_at') } : {}),
  }
}

/** The App owns this gate and fences it when credentials or daemon change. */
interface CronMutationGateOptions {
  pending: () => boolean
  canMutate: () => boolean
  runMutation: CronMutationOptions['onMutation']
}
export class CronMutationGate {
  readonly #options: CronMutationGateOptions
  readonly #generations = new WeakMap<CronAttempt, number>()
  #generation = 0
  #abort = new AbortController()

  constructor(options: CronMutationGateOptions) {
    this.#options = options
  }

  fence(): void {
    this.#generation += 1
    this.#abort.abort()
    this.#abort = new AbortController()
  }

  async run(
    draft: CronAttempt,
    mutate: (context: MutationContext) => Promise<MutationResult>,
  ): Promise<boolean> {
    if (this.#options.pending() || !this.#options.canMutate()) return false
    const generation = this.#generation
    const prior = this.#generations.get(draft)
    if (prior !== undefined && prior !== generation) return false
    this.#generations.set(draft, generation)
    const signal = this.#abort.signal
    const result = await this.#options.runMutation(draft, (context) =>
      mutate({ ...context, signal }),
    )
    return !signal.aborted && generation === this.#generation && result
  }
}
