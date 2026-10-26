import { parseCronDocument, rawCronValue, type RawCronDocument } from './document'
import { orvalFetch, rawApiFetch } from '../api/client'
import { AuthenticationRequiredError } from '../auth/session'
import type { CronRun, CronJob, CronWorkflow, CronSummary, ShowIssueResponseBody } from './types'
import type { CronCapabilitiesResponseBody } from '../api/generated'
import { createUISnapshotRequest } from '../state/snapshot'
import { normalizeEvent } from '../kata/projection'
import type { KataTaskEvent } from '../kata/types'
export class CronReadError extends Error {
  constructor(readonly status: number) {
    super(status === 403 ? 'Project not readable' : 'Cron unavailable')
  }
}
interface Query {
  statuses?: string[]
  beforeUID?: string
  jobUID?: string
}
export interface CronDefinitionRecord {
  uid: string
  name: string
  definitionEventUID: string
  deletedAt?: string
  document: RawCronDocument
}
export class CronClient {
  async readDefinition(
    kind: 'job' | 'workflow',
    projectID: number,
    uid: string,
    signal: AbortSignal,
  ): Promise<CronDefinitionRecord> {
    const response = await rawApiFetch(
      `/api/v1/projects/${projectID}/cron/${kind}s/${encodeURIComponent(uid)}`,
      { method: 'GET', signal },
    )
    if (response.status === 401) throw new AuthenticationRequiredError('Authentication required')
    if (response.status !== 200) throw new CronReadError(response.status)
    const envelope = parseCronDocument(await response.text())
    const raw = rawCronValue(envelope, [kind, 'definition'])
    if (raw === undefined) throw new Error('Cron definition is missing')
    // Extract before decoding even the envelope metadata.
    const document = parseCronDocument(raw)
    const string = (key: string): string => {
      const value = rawCronValue(envelope, [kind, key])
      const decoded: unknown = value === undefined ? undefined : JSON.parse(value)
      if (typeof decoded !== 'string') throw new Error(`Cron ${key} is missing`)
      return decoded
    }
    const deleted = rawCronValue(envelope, [kind, 'deleted_at'])
    return {
      uid: string('uid'),
      name: string('name'),
      definitionEventUID: string('definition_event_uid'),
      ...(deleted && deleted !== 'null' ? { deletedAt: string('deleted_at') } : {}),
      document,
    }
  }
  async read<T>(project: number, path: string, signal: AbortSignal, query: Query = {}): Promise<T> {
    const q = new URLSearchParams()
    for (const status of query.statuses ?? []) q.append('status', status)
    if (query.beforeUID) q.set('before_uid', query.beforeUID)
    if (query.jobUID) q.set('job_uid', query.jobUID)
    return this.#get<T>(`/api/v1/projects/${project}/${path}${q.size ? '?' + q : ''}`, signal)
  }
  async #get<T>(url: string, signal: AbortSignal): Promise<T> {
    const response = await orvalFetch<{ data: T; status: number }>(url, { method: 'GET', signal })
    if (response.status === 401) throw new AuthenticationRequiredError('Authentication required')
    if (response.status !== 200) throw new CronReadError(response.status)
    return response.data
  }
  summary(project: number, signal: AbortSignal) {
    return this.read<CronSummary>(project, 'cron/summary', signal)
  }
  jobs(project: number, signal: AbortSignal, query: Query = {}) {
    return this.read<{ jobs: CronJob[] }>(project, 'cron/jobs', signal, query)
  }
  runs(project: number, signal: AbortSignal, query: Query = {}) {
    return this.read<{ runs: CronRun[]; next_before_uid?: string }>(
      project,
      'cron/runs',
      signal,
      query,
    )
  }
  workflows(project: number, signal: AbortSignal) {
    return this.read<{ workflows: CronWorkflow[] }>(project, 'cron/workflows', signal)
  }
  async job(project: number, uid: string, signal: AbortSignal) {
    return (
      await this.read<{ job: CronJob }>(project, 'cron/jobs/' + encodeURIComponent(uid), signal)
    ).job
  }
  async run(project: number, uid: string, signal: AbortSignal) {
    return (
      await this.read<{ run: CronRun }>(project, 'cron/runs/' + encodeURIComponent(uid), signal)
    ).run
  }
  async workflow(project: number, uid: string, signal: AbortSignal) {
    return (
      await this.read<{ workflow: CronWorkflow }>(
        project,
        'cron/workflows/' + encodeURIComponent(uid),
        signal,
      )
    ).workflow
  }
  // An issue keeps its UID when it moves projects. The run's project answers
  // for unmoved issues, including under project-scoped authority; a moved one
  // falls back to the UID read.
  async issue(project: number, uid: string, signal: AbortSignal) {
    try {
      return await this.read<ShowIssueResponseBody>(
        project,
        'issues/' + encodeURIComponent(uid),
        signal,
      )
    } catch (error) {
      if (!(error instanceof CronReadError && error.status === 404)) throw error
      return this.#get<ShowIssueResponseBody>('/api/v1/issues/' + encodeURIComponent(uid), signal)
    }
  }
  // Daemons predating the field omit agentsview_enabled, which means disabled.
  async capabilities(project: number, signal: AbortSignal) {
    const body = await this.read<Partial<CronCapabilitiesResponseBody>>(
      project,
      'cron/capabilities',
      signal,
    )
    return { agentsviewEnabled: body.agentsview_enabled === true }
  }
  // The selected-issue snapshot carries that issue's newest 500 history events.
  async issueHistory(
    projectUID: string,
    issueUID: string,
    signal: AbortSignal,
  ): Promise<readonly KataTaskEvent[]> {
    const response = await createUISnapshotRequest()(
      {
        view: 'all-open',
        projectUID,
        selectedIssueUID: issueUID,
        statuses: [],
        owners: [],
        labels: [],
        relationships: [],
        includeGraph: false,
        includeHistory: true,
      },
      { signal, full: true },
    )
    if (response.status !== 200) throw new Error('Snapshot unavailable')
    return (response.snapshot.selected?.history ?? []).map(normalizeEvent)
  }
}
