// @vitest-environment-options { "url": "http://127.0.0.2/kata" }
import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { tick } from 'svelte'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import App from './App.svelte'
import { CronController } from './lib/cron/controller'
import { SnapshotController } from './lib/state/snapshot'

const projectUID = '01J00000000000000000000002'
const runUIDs = [
  '01J00000000000000000000011',
  '01J00000000000000000000012',
  '01J00000000000000000000013',
]
const jobUIDs = [
  '01J00000000000000000000021',
  '01J00000000000000000000022',
  '01J00000000000000000000023',
]
const names = ['First page job', 'Page two job', 'Newly observed job'] as const
beforeEach(() => {
  sessionStorage.clear()
  localStorage.clear()
  history.replaceState(null, '', '/kata?view=cron&cron_tab=runs#direct=1')
  sessionStorage.setItem(
    'kata.web.session.v1',
    JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
  )
  vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
  vi.useFakeTimers()
})
afterEach(() => {
  cleanup()
  vi.useRealTimers()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

function mount() {
  let changed = false,
    summaryCount = 2
  const snapshotStatuses: number[] = []
  const snapshotLoads = vi.spyOn(SnapshotController.prototype, 'load'),
    catalogLoads = vi.spyOn(CronController.prototype, 'load')
  const runPages: string[] = [],
    runDetails: string[] = []
  const run = (index: number) => ({
    id: index + 1,
    uid: runUIDs[index],
    project_id: 7,
    revision: 1,
    actor: 'reader',
    job_uid: jobUIDs[index],
    created_at: `2026-10-25T10:00:0${index === 2 ? 2 : 1 - index}Z`,
    updated_at: '2026-10-25T10:00:02Z',
    status: changed && index === 0 ? 'succeeded' : 'failed',
    summary: { version: 1, message: `Detail ${index}` },
  })
  const jobs = jobUIDs.map((uid, index) => ({
    uid,
    name: names[index],
    last_run: null,
    definition: {
      version: 1,
      kind: 'job',
      enabled: true,
      trigger: { kind: 'interval', interval_seconds: 60 },
      action: { kind: 'execute', prompt: 'Inspect' },
      overlap: 'forbid',
      catchup: 'all',
    },
  }))
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request =
          input instanceof Request
            ? input
            : new Request(new URL(String(input), window.location.origin), init),
        url = new URL(request.url)
      request.signal.throwIfAborted()
      if (url.pathname === '/api/v1/ui/telemetry')
        return Response.json({ status: 'disabled' }, { status: 202 })
      if (url.pathname === '/api/v1/ui/references')
        return Response.json({ issues: [], labels: [], owners: [], projects: [] })
      if (url.pathname === '/api/v1/ui/snapshot') {
        if (request.headers.has('If-None-Match') && !changed) {
          snapshotStatuses.push(304)
          return new Response(null, { status: 304, headers: { ETag: '"cron-1"' } })
        }
        snapshotStatuses.push(200)
        return Response.json(
          {
            contract_version: '2',
            cursor: changed ? 13 : 12,
            capabilities: {
              writable: false,
              updates: 'poll',
              actor_policy: 'readonly',
              token_audit_read: false,
            },
            origin: 'https://daemon.example',
            origin_stable: true,
            catalog: [
              {
                project: {
                  id: 7,
                  uid: projectUID,
                  name: 'example-project',
                  active: true,
                  metadata: {},
                  revision: 1,
                  created_at: '2026-10-25T00:00:00Z',
                },
                stats: { Open: 0, Closed: 0, LastEventAt: '2026-10-25T00:00:00Z' },
              },
            ],
            collection: [],
            collection_links: [],
          },
          { headers: { ETag: changed ? '"cron-2"' : '"cron-1"' } },
        )
      }
      if (url.pathname.endsWith('/cron/summary'))
        return Response.json({
          failed_runs_24h: summaryCount,
          window_start: '2026-10-24T10:00:00Z',
          window_end: '2026-10-25T10:00:00Z',
        })
      if (url.pathname.endsWith('/cron/jobs')) return Response.json({ jobs })
      if (url.pathname.endsWith('/cron/workflows')) return Response.json({ workflows: [] })
      if (url.pathname.endsWith('/cron/runs')) {
        const before = url.searchParams.get('before_uid') ?? ''
        runPages.push(before)
        const index = before === runUIDs[2] ? 0 : before === runUIDs[0] ? 1 : changed ? 2 : 0
        return Response.json({ runs: [run(index)], next_before_uid: runUIDs[index] })
      }
      if (url.pathname.includes('/cron/runs/')) {
        const uid = url.pathname.split('/').at(-1)!
        runDetails.push(uid)
        return Response.json({ run: run(runUIDs.indexOf(uid)) })
      }
      throw new Error(`Unexpected fixture route ${url.pathname}`)
    }),
  )
  render(App)
  return {
    snapshotLoads,
    catalogLoads,
    runPages,
    runDetails,
    snapshotStatuses,
    setSummary: (count: number) => {
      summaryCount = count
    },
    change: () => {
      changed = true
    },
  }
}
async function settle(task: ReturnType<typeof mount>) {
  await task.snapshotLoads.mock.results.at(-1)?.value
  await task.catalogLoads.mock.results.at(-1)?.value
  await tick()
}
async function pageTwo(task: ReturnType<typeof mount>) {
  await vi.waitFor(async () => {
    await tick()
    expect(screen.queryByRole('button', { name: names[0] })).not.toBeNull()
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Load more · example-project' }))
  await vi.waitFor(async () => {
    await tick()
    expect(screen.queryByRole('button', { name: names[1] })).not.toBeNull()
  })
  expect(task.runPages).toEqual(['', runUIDs[0]])
}

test('App poll accepts a 304 without discarding page two or fetching its catalogs again', async () => {
  const task = mount()
  await pageTwo(task)
  const snapshots = task.snapshotLoads.mock.calls.length
  await vi.advanceTimersByTimeAsync(15000)
  await vi.waitFor(() => expect(task.snapshotLoads.mock.calls.length).toBeGreaterThan(snapshots))
  await settle(task)
  expect(screen.queryByRole('button', { name: names[1] })).not.toBeNull()
  expect(task.runPages).toEqual(['', runUIDs[0]])
})
test('App opens and closes a page-two drawer through selection-only navigation', async () => {
  const task = mount()
  await pageTwo(task)
  const snapshots = task.snapshotLoads.mock.calls.length,
    catalogs = task.catalogLoads.mock.calls.length
  await fireEvent.click(screen.getByRole('button', { name: names[1] }))
  await vi.waitFor(async () => {
    await tick()
    expect(screen.queryByText('Detail 1')).not.toBeNull()
  })
  await settle(task)
  expect(task.runPages).toEqual(['', runUIDs[0]])
  expect(task.catalogLoads).toHaveBeenCalledTimes(catalogs)
  expect(task.snapshotLoads).toHaveBeenCalledTimes(snapshots)
  expect(location.search).toContain(`cron_uid=${runUIDs[1]}`)
  await fireEvent.keyDown(window, { key: 'Escape' })
  await vi.advanceTimersByTimeAsync(0)
  await settle(task)
  expect(screen.queryByRole('dialog')).toBeNull()
  expect(screen.queryByRole('button', { name: names[1] })).not.toBeNull()
  expect(task.catalogLoads).toHaveBeenCalledTimes(catalogs)
  expect(task.snapshotLoads).toHaveBeenCalledTimes(snapshots)
  expect(location.search).not.toContain('cron_uid=')
  history.replaceState(
    null,
    '',
    `/kata?view=cron&cron_tab=runs&cron_kind=run&cron_scope=${projectUID}&cron_uid=${runUIDs[1]}#direct=1`,
  )
  window.dispatchEvent(new PopStateEvent('popstate'))
  await vi.waitFor(async () => {
    await tick()
    expect(screen.queryByText('Detail 1')).not.toBeNull()
  })
  expect(task.catalogLoads).toHaveBeenCalledTimes(catalogs)
  expect(task.snapshotLoads).toHaveBeenCalledTimes(snapshots)
})
test('App reconciles a changed snapshot through the loaded boundary and retains the drawer', async () => {
  const task = mount()
  await pageTwo(task)
  await fireEvent.click(screen.getByRole('button', { name: names[1] }))
  await vi.waitFor(async () => {
    await tick()
    expect(screen.queryByText('Detail 1')).not.toBeNull()
  })
  await settle(task)
  const drawer = screen.getByRole('dialog', { name: 'Run detail' }),
    snapshots = task.snapshotLoads.mock.calls.length
  task.change()
  window.dispatchEvent(new Event('focus'))
  await vi.advanceTimersByTimeAsync(0)
  await vi.waitFor(() => expect(task.snapshotLoads.mock.calls.length).toBeGreaterThan(snapshots))
  await settle(task)
  expect(screen.queryByRole('button', { name: names[2] })).not.toBeNull()
  expect(screen.queryByRole('button', { name: names[1] })).not.toBeNull()
  expect(screen.getByRole('dialog', { name: 'Run detail' })).toBe(drawer)
  expect(screen.queryByRole('row', { name: /succeeded.*First page job/ })).not.toBeNull()
  expect(task.runPages.slice(-3)).toEqual(['', runUIDs[2], runUIDs[0]])
})

test.each(['pagination', 'summary'] as const)(
  'App grouping navigation followed by 304 keeps %s requests live',
  async (operation) => {
    const task = mount()
    await vi.waitFor(async () => {
      await tick()
      expect(screen.queryByRole('button', { name: names[0] })).not.toBeNull()
    })
    await settle(task)
    const catalogs = task.catalogLoads.mock.calls.length
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Group by project' }))
    await settle(task)
    expect(location.search).toContain('cron_group=project')
    expect(task.snapshotStatuses).toEqual([200, 304])
    expect(task.catalogLoads).toHaveBeenCalledTimes(catalogs)
    if (operation === 'pagination') {
      await pageTwo(task)
    } else {
      task.setSummary(9)
      await vi.advanceTimersByTimeAsync(60_000)
      await vi.waitFor(async () => {
        await tick()
        expect(screen.queryByRole('button', { name: 'Cron 9' })).not.toBeNull()
      })
    }
  },
)
