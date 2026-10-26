import { afterEach, expect, test, vi } from 'vitest'
import { createCredentialedFetch, setGeneratedFetch } from '../api/client'
import { AuthenticationRequiredError } from '../auth/session'
import { CronClient, CronReadError } from './client'
import { CronController } from './controller'
import { defaultCronRoute } from './route'
import type { CronRun } from './types'

// The Cron page uses only core cron reads; these tests exercise HTTP behavior.
afterEach(() => vi.useRealTimers())

test('core list, detail, pagination and refresh reads never request optional enrichment', async () => {
  const upstream = vi.fn<typeof fetch>(async () =>
    Response.json({ jobs: [], runs: [], job: {}, run: {}, workflows: [], workflow: {} }),
  )
  setGeneratedFetch(createCredentialedFetch(() => ({ session: 'tab', csrf: 'csrf' }), upstream))
  const api = new CronClient()
  const signal = new AbortController().signal
  await api.jobs(1, signal, { statuses: ['failed', 'never_ran'] })
  await api.runs(1, signal, { statuses: ['failed', 'running'], beforeUID: 'cursor' })
  await api.job(1, 'job / identifier', signal)
  await api.run(1, 'run / identifier', signal)
  await api.runs(1, signal, { jobUID: 'job / identifier' })
  await api.workflows(1, signal)
  await api.workflow(1, 'workflow / identifier', signal)
  expect(upstream).toHaveBeenCalledTimes(7)
  const urls = upstream.mock.calls.map((call) => new URL(String(call[0])))
  expect(urls[0]!.searchParams.getAll('status')).toEqual(['failed', 'never_ran'])
  expect(urls[1]!.searchParams.getAll('status')).toEqual(['failed', 'running'])
  expect(urls[1]!.searchParams.get('before_uid')).toBe('cursor')
  expect(urls[4]!.searchParams.get('job_uid')).toBe('job / identifier')
  expect(decodeURIComponent(urls[2]!.pathname)).toBe(
    '/api/v1/projects/1/cron/jobs/job / identifier',
  )
  for (const [input, init] of upstream.mock.calls as unknown as [
    RequestInfo | URL,
    RequestInit,
  ][]) {
    const url = new URL(String(input))
    expect(url.searchParams.has('include_usage')).toBe(false)
    expect(url.searchParams.has('refresh_usage')).toBe(false)
    expect(new Headers(init.headers).get('X-Kata-Web-Session')).toBe('tab')
    expect(init.credentials).toBe('same-origin')
    expect(init.redirect).toBe('error')
    expect(init.signal).toBe(signal)
  }
})

function catalogClient() {
  return Object.assign(new CronClient(), {
    summary: vi.fn<CronClient['summary']>(async () => ({
      failed_runs_24h: 7,
      window_start: '',
      window_end: '',
    })),
    jobs: vi.fn<CronClient['jobs']>(async () => ({ jobs: [] })),
    runs: vi.fn<CronClient['runs']>(async () => ({ runs: [] as CronRun[] })),
    workflows: vi.fn<CronClient['workflows']>(async () => ({ workflows: [] })),
  })
}
const project = { id: 1, uid: 'project-one', name: 'example-project' }
const intent = () => ({
  projects: [project],
  route: defaultCronRoute(),
  daemonID: 'first',
  authorityGeneration: 1,
})

test('a core 401 clears authority and a project 403 stays project-local', async () => {
  const api = catalogClient()
  api.summary.mockRejectedValueOnce(new AuthenticationRequiredError('Authentication required'))
  const reject = vi.fn()
  const controller = new CronController(api, reject)
  try {
    await controller.load(intent())
    expect(reject).toHaveBeenCalledOnce()
    expect(controller.state.projects).toEqual([])
    api.summary.mockRejectedValueOnce(new CronReadError(403))
    await controller.load(intent())
    expect(controller.state.projects[0]?.error).toBe('Project not readable')
    expect(reject).toHaveBeenCalledOnce()
  } finally {
    controller.destroy()
  }
})
