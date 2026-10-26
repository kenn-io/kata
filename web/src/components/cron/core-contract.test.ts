import { cleanup, render, screen } from '@testing-library/svelte'
import { afterEach, expect, test, vi } from 'vitest'
import { createCredentialedFetch, setGeneratedFetch } from '../../lib/api/client'
import { CronController } from '../../lib/cron/controller'
import { defaultCronRoute } from '../../lib/cron/route'
import type { CronRun } from '../../lib/cron/types'
import CronPage from './CronPage.svelte'

// The page loads when every optional capability endpoint is absent.
afterEach(() => {
  cleanup()
  vi.useRealTimers()
})
const run = {
  uid: 'run',
  job_uid: 'job',
  issue_uid: 'issue',
  status: 'failed',
  created_at: '2026-10-25T10:00:00Z',
  summary: { version: 1, message: 'Recorded run summary' },
} as CronRun
const job = {
  uid: 'job',
  name: 'Inspect workspace',
  last_run: run,
  definition: { enabled: true, kind: 'issue', trigger: { kind: 'manual' }, options: {} },
}
const callbacks = () => ({
  onNavigate: vi.fn(),
  onLoadMore: vi.fn(),
  onRefresh: vi.fn(),
  onOpenIssue: vi.fn(),
})

test.each(['jobs', 'runs', 'workflows'] as const)(
  'entire %s page loads when every optional capability endpoint is absent',
  async (tab) => {
    const urls: URL[] = []
    const upstream = vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input))
      urls.push(url)
      const payloads: Record<string, unknown> = {
        '/api/v1/projects/1/cron/summary': {
          failed_runs_24h: 7,
          window_start: '2026-10-24T10:00:00Z',
          window_end: '2026-10-25T10:00:00Z',
        },
        '/api/v1/projects/1/cron/jobs': { jobs: [job] },
        '/api/v1/projects/1/cron/runs': { runs: [run] },
        '/api/v1/projects/1/cron/workflows': {
          workflows: [{ uid: 'workflow', name: 'Routine checks', definition: { steps: [] } }],
        },
      }
      return url.pathname in payloads
        ? Response.json(payloads[url.pathname])
        : new Response('Endpoint absent', { status: 404 })
    })
    setGeneratedFetch(createCredentialedFetch(() => ({ session: 'tab', csrf: 'csrf' }), upstream))
    const controller = new CronController()
    const route = { ...defaultCronRoute(), tab }
    try {
      await controller.load({
        projects: [{ id: 1, uid: 'project-one', name: 'example-project' }],
        route,
        daemonID: undefined,
        authorityGeneration: 1,
      })
      render(CronPage, { route, state: controller.state, ...callbacks() })
      expect(screen.getByRole('heading', { name: 'Cron' })).toBeTruthy()
      expect(controller.state.projects[0]?.error).toBeUndefined()
      expect(controller.state.failedRuns24h).toBe(7)
      expect(controller.state.badgePartial).toBe(false)
      expect(screen.queryByText('No cron records in these projects.')).toBeNull()
      expect(
        screen.getByRole('button', {
          name: tab === 'workflows' ? 'Routine checks' : 'Inspect workspace',
        }),
      ).toBeTruthy()
      expect(urls.map((url) => url.pathname).sort()).toEqual([
        '/api/v1/projects/1/cron/jobs',
        '/api/v1/projects/1/cron/runs',
        '/api/v1/projects/1/cron/summary',
        '/api/v1/projects/1/cron/workflows',
      ])
    } finally {
      controller.destroy()
    }
  },
)
