import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, expect, test, vi } from 'vitest'
import * as hegel from '@hegeldev/hegel'
import * as gs from '@hegeldev/hegel/generators'
import { createCredentialedFetch, setGeneratedFetch } from '../../lib/api/client'
import { CronController, emptyCronState } from '../../lib/cron/controller'
import { defaultCronRoute } from '../../lib/cron/route'
import type { CronRun } from '../../lib/cron/types'
import CronPage from './CronPage.svelte'
import CronRunDrawer from './CronRunDrawer.svelte'

// The page loads without optional endpoints, and unknown response fields never add UI.
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

const legacyUsage = {
  primary_model: 'legacy-model',
  cost_microdollars: 420000,
  partial: true,
  models: [],
  sessions: [
    {
      agent: 'legacy-agent',
      session_id: 'legacy-session',
      url: 'https://sessions.example/legacy',
      state: 'available',
      partial: false,
      models: [],
    },
  ],
}
test.each(['jobs', 'runs'] as const)('%s table ignores unknown usage fields', (tab) => {
  const state = emptyCronState()
  const legacyRun = { ...run, usage: legacyUsage }
  state.projects = [
    {
      project: { id: 1, uid: 'project-one', name: 'example-project' },
      jobs: [{ ...job, last_run: legacyRun } as never],
      runs: [legacyRun],
      workflows: [],
      hasRecords: true,
    },
  ]
  render(CronPage, { route: { ...defaultCronRoute(), tab }, state, ...callbacks() })
  expect(screen.getByRole('button', { name: 'Inspect workspace' })).toBeTruthy()
  expect(screen.queryByRole('columnheader', { name: 'Model' })).toBeNull()
  expect(screen.queryByRole('columnheader', { name: 'Cost' })).toBeNull()
  expect(screen.queryByText('legacy-model')).toBeNull()
  expect(screen.queryByText('~$0.42+')).toBeNull()
})

test.each(['usage', 'transcripts'] as const)(
  'run drawer omits legacy %s while preserving summary and issue actions',
  async (shape) => {
    const legacyRun = {
      ...run,
      ...(shape === 'usage'
        ? { usage: legacyUsage }
        : {
            transcripts: [
              {
                agent: 'legacy-agent',
                session_id: 'legacy-session',
                url: 'https://sessions.example/legacy',
              },
            ],
            transcripts_incomplete: true,
          }),
    }
    const onOpenIssue = vi.fn()
    render(CronRunDrawer, {
      run: legacyRun,
      onOpenIssue,
      onOpenJob: vi.fn(),
      peek: {
        status: 'ready',
        issue: {
          issue: { uid: 'issue', title: 'Readable issue', short_id: 'abc4' },
          comments: [
            {
              uid: 'comment',
              created_at: '2026-10-25T11:00:00Z',
              body: 'Latest readable comment',
              author: 'example-author',
            },
          ],
        } as never,
      },
    })
    expect(screen.getByText('Recorded run summary')).toBeTruthy()
    expect(screen.getByText('Latest readable comment')).toBeTruthy()
    await fireEvent.click(screen.getByRole('button', { name: 'abc4 · Readable issue' }))
    expect(onOpenIssue).toHaveBeenCalledWith('issue')
    expect(screen.queryByText('Model', { selector: 'dt' })).toBeNull()
    expect(screen.queryByText('Cost', { selector: 'dt' })).toBeNull()
    expect(screen.queryByRole('heading', { name: 'Linked sessions' })).toBeNull()
    expect(screen.queryByRole('heading', { name: 'Models' })).toBeNull()
    expect(screen.queryByRole('link', { name: /legacy-session/ })).toBeNull()
  },
)

test(
  'legacy metadata cannot introduce run drawer sections for arbitrary recorded text',
  () =>
    hegel.test(
      (tc) => {
        // Draw the text domain first; cap only fixture materialization to keep DOM work finite.
        const text = tc.draw(gs.text({ codec: 'utf-8' })).slice(0, 256)
        const shape = tc.draw(gs.sampledFrom(['usage', 'transcripts'] as const))
        const legacyRun = {
          ...run,
          ...(shape === 'usage'
            ? { usage: { ...legacyUsage, primary_model: text } }
            : {
                transcripts: [
                  { agent: text, session_id: text, url: 'https://sessions.example/legacy' },
                ],
              }),
        }
        try {
          render(CronRunDrawer, {
            run: legacyRun,
            peek: { status: 'no-issue' },
            onOpenIssue: vi.fn(),
            onOpenJob: vi.fn(),
          })
          expect(screen.queryByText('Model', { selector: 'dt' })).toBeNull()
          expect(screen.queryByText('Cost', { selector: 'dt' })).toBeNull()
          expect(screen.queryByRole('heading', { name: 'Linked sessions' })).toBeNull()
          expect(screen.queryByRole('heading', { name: 'Models' })).toBeNull()
        } finally {
          cleanup()
        }
      },
      { testCases: 2000 },
    ),
  60000,
)
