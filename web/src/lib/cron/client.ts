import { orvalFetch } from '../api/client'
import { AuthenticationRequiredError } from '../auth/session'
import type { CronRun, CronJob, CronWorkflow, CronSummary, ShowIssueResponseBody } from './types'
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
export class CronClient {
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
}
