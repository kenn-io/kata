import { expect, test, vi } from 'vitest'
import * as hegel from '@hegeldev/hegel'
import * as gs from '@hegeldev/hegel/generators'
import { CronController, mergedRuns } from './controller'
import { CronClient, CronReadError } from './client'
import { defaultCronRoute } from './route'
import type { CronRun, CronSummary, ShowIssueResponseBody } from './types'
const projects = [
  { id: 1, uid: 'p1', name: 'project-one' },
  { id: 2, uid: 'p2', name: 'project-two' },
  { id: 3, uid: 'p3', name: 'project-three' },
]
function client() {
  return Object.assign(new CronClient(), {
    summary: vi.fn<CronClient['summary']>(async () => ({
      failed_runs_24h: 7,
      window_start: '',
      window_end: '',
    })),
    jobs: vi.fn<CronClient['jobs']>(async () => ({ jobs: [] })),
    runs: vi.fn<CronClient['runs']>(async () => ({ runs: [], next_before_uid: '' })),
    workflows: vi.fn<CronClient['workflows']>(async () => ({ workflows: [] })),
  })
}
test('denied project preserves readable catalogs and summary counts', async () => {
  const api = client()
  api.summary.mockImplementation(async (id) => {
    if (id === 2) throw new CronReadError(403)
    return { failed_runs_24h: 7, window_start: '', window_end: '' }
  })
  const c = new CronController(api)
  await c.load({ projects, route: defaultCronRoute(), daemonID: undefined, authorityGeneration: 1 })
  expect(c.state.failedRuns24h).toBe(14)
  expect(c.state.badgePartial).toBe(true)
  expect(c.state.projects.filter((p) => !p.error)).toHaveLength(2)
  c.destroy()
})
test('fence discards late authority responses', async () => {
  const api = client()
  let resolve!: (v: CronSummary) => void
  api.summary.mockImplementation(() => new Promise((r) => (resolve = r)))
  const c = new CronController(api)
  const pending = c.load({
    projects: projects.slice(0, 1),
    route: defaultCronRoute(),
    authorityGeneration: 1,
    daemonID: undefined,
  })
  c.fence()
  resolve({ failed_runs_24h: 7, window_start: '', window_end: '' })
  await pending
  expect(c.state.projects).toEqual([])
  expect(c.state.failedRuns24h).toBe(0)
  c.destroy()
})
test('selection generation rejects an old issue response', async () => {
  vi.useFakeTimers()
  const api = client()
  api.run = vi.fn(
    async (_p, uid) =>
      ({
        uid,
        issue_uid: uid,
        summary: { version: 1 },
        created_at: '',
        status: 'failed',
      }) as CronRun,
  )
  let resolve!: (v: unknown) => void
  api.issue = vi.fn(() => new Promise((r) => (resolve = r))) as typeof api.issue
  const c = new CronController(api)
  await c.load({ projects, route: defaultCronRoute(), authorityGeneration: 1, daemonID: undefined })
  const first = c.select({ kind: 'run', projectUID: 'p1', uid: 'a' })
  await vi.advanceTimersByTimeAsync(150)
  await first
  const second = c.select({ kind: 'run', projectUID: 'p2', uid: 'b' })
  await second
  resolve({ issue: { title: 'old' }, comments: [] })
  await Promise.resolve()
  expect(c.state.selected?.uid).toBe('b')
  expect(c.state.peek.issue).toBeUndefined()
  c.destroy()
  vi.useRealTimers()
})

test('badge-only refresh never fetches usage or catalogs', async () => {
  const api = client()
  const c = new CronController(api)
  await c.loadBadge({
    projects,
    route: defaultCronRoute(),
    authorityGeneration: 1,
    daemonID: undefined,
  })
  expect(c.state.failedRuns24h).toBe(21)
  expect(api.runs).not.toHaveBeenCalled()
  expect(api.jobs).not.toHaveBeenCalled()
  c.destroy()
})
test('per-project cursors and creation order are independent', async () => {
  const api = client()
  const seen: string[] = []
  api.runs = vi.fn(async (id, _signal, q) => {
    seen.push(`${id}:${q?.beforeUID ?? ''}`)
    return {
      runs: [
        {
          uid: q?.beforeUID ? 'next' : 'first',
          created_at: id === 1 ? '2026-01-01' : '2026-01-02',
        } as CronRun,
      ],
      next_before_uid: q?.beforeUID ? '' : 'cursor-' + id,
    }
  })
  const c = new CronController(api)
  await c.load({
    projects: projects.slice(0, 2),
    route: defaultCronRoute(),
    authorityGeneration: 1,
    daemonID: undefined,
  })
  await c.loadMore('p1')
  expect(seen).toContain('1:cursor-1')
  expect(seen).not.toContain('2:cursor-1')
  expect(c.state.projects.find((p) => p.project.uid === 'p1')?.runs).toHaveLength(2)
  c.destroy()
})

test('summary outage preserves readable core records', async () => {
  const api = client()
  api.summary.mockRejectedValue(new CronReadError(503))
  api.runs.mockResolvedValue({ runs: [{ uid: 'r', created_at: '2026-01-01' } as CronRun] })
  const c = new CronController(api)
  await c.load({
    projects: projects.slice(0, 1),
    route: defaultCronRoute(),
    authorityGeneration: 1,
    daemonID: undefined,
  })
  expect(c.state.projects[0]?.runs).toHaveLength(1)
  expect(c.state.projects[0]?.error).toBeUndefined()
  expect(c.state.badgePartial).toBe(true)
  c.destroy()
})
test('pagination errors retain previously loaded history', async () => {
  const api = client()
  api.runs
    .mockResolvedValueOnce({
      runs: [{ uid: 'r', created_at: '2026-01-01' } as CronRun],
      next_before_uid: 'cursor',
    })
    .mockRejectedValueOnce(new CronReadError(503))
  const c = new CronController(api)
  await c.load({
    projects: projects.slice(0, 1),
    route: defaultCronRoute(),
    authorityGeneration: 1,
    daemonID: undefined,
  })
  await c.loadMore('p1')
  expect(c.state.projects[0]?.runs).toHaveLength(1)
  expect(c.state.projects[0]?.error).toBeUndefined()
  expect(c.state.projects[0]?.historyError).toBe('More history unavailable')
  c.destroy()
})

test('a global 401 fences protected results and requests authentication', async () => {
  const { AuthenticationRequiredError } = await import('../auth/session')
  const api = client()
  api.summary.mockRejectedValue(new AuthenticationRequiredError('Authentication required'))
  const reject = vi.fn()
  const c = new CronController(api, reject)
  await c.load({ projects, route: defaultCronRoute(), authorityGeneration: 1, daemonID: undefined })
  expect(c.state.projects).toEqual([])
  expect(c.state.failedRuns24h).toBe(0)
  expect(reject).toHaveBeenCalledOnce()
  c.destroy()
})
test('a replacement daemon generation discards old catalog answers', async () => {
  const api = client()
  let resolve!: (v: CronSummary) => void
  api.summary.mockImplementationOnce(() => new Promise((r) => (resolve = r)))
  const c = new CronController(api)
  const first = c.load({
    projects: projects.slice(0, 1),
    route: defaultCronRoute(),
    authorityGeneration: 1,
    daemonID: 'first',
  })
  await c.load({
    projects: projects.slice(1, 2),
    route: defaultCronRoute(),
    authorityGeneration: 2,
    daemonID: 'second',
  })
  resolve({ failed_runs_24h: 0, window_start: '', window_end: '' })
  await first
  expect(c.state.projects.map((p) => p.project.uid)).toEqual(['p2'])
  expect(c.state.failedRuns24h).toBe(7)
  c.destroy()
})

test('tab and filter loads retain the authoritative badge while requests are pending', async () => {
  const api = client()
  const c = new CronController(api)
  const intent = {
    projects: projects.slice(0, 1),
    route: defaultCronRoute(),
    authorityGeneration: 1,
    daemonID: undefined,
  }
  await c.load(intent)
  let resolve!: (v: CronSummary) => void
  api.summary.mockImplementationOnce(() => new Promise((r) => (resolve = r)))
  const next = c.load({ ...intent, route: { ...intent.route, tab: 'runs', statuses: ['failed'] } })
  expect(c.state.failedRuns24h).toBe(7)
  resolve({ failed_runs_24h: 7, window_start: '', window_end: '' })
  await next
  c.destroy()
})

test('fence clears selected protected detail as well as catalogs', async () => {
  const api = client()
  api.run = vi.fn(
    async () =>
      ({ uid: 'r', status: 'failed', summary: { version: 1 }, created_at: '' }) as CronRun,
  )
  const c = new CronController(api)
  await c.load({ projects, route: defaultCronRoute(), authorityGeneration: 1, daemonID: undefined })
  await c.select({ kind: 'run', projectUID: 'p1', uid: 'r' })
  expect(c.state.detail?.uid).toBe('r')
  c.fence()
  expect(c.state.selected).toBeUndefined()
  expect(c.state.detail).toBeUndefined()
  c.destroy()
})

test('merged histories order variable timestamp fractions and retain UID/project ties', () => {
  const row = (projectUID: string, runs: { uid: string; created_at: string }[]) => ({
    project: { id: 1, uid: projectUID, name: 'example-project' },
    jobs: [],
    workflows: [],
    hasRecords: true,
    runs: runs as CronRun[],
  })
  const stamp = '2026-10-25T10:00:00'
  const result = mergedRuns([
    row('b', [
      { uid: 'whole', created_at: stamp + 'Z' },
      { uid: 'short', created_at: stamp + '.12Z' },
      { uid: 'z', created_at: stamp + '.121Z' },
    ]),
    row('a', [
      { uid: 'a', created_at: stamp + '.121000000Z' },
      { uid: 'z', created_at: stamp + '.121000000Z' },
    ]),
  ])
  expect(result.map(({ project, run }) => `${project.uid}:${run.uid}`)).toEqual([
    'a:z',
    'b:z',
    'a:a',
    'b:short',
    'b:whole',
  ])
})

test.each(['runs', 'jobs'] as const)(
  'the %s tab keeps job-name queries distinct from historical run statuses',
  async (tab) => {
    const api = client(),
      latest = { uid: 'latest', status: 'succeeded' } as CronRun
    api.jobs.mockImplementation(async (_id, _signal, query) => ({
      jobs: query?.statuses?.includes('failed')
        ? []
        : [{ uid: 'job', name: 'Inspect workspace', last_run: latest } as never],
    }))
    api.runs.mockResolvedValue({
      runs: [
        {
          uid: 'historical',
          job_uid: 'job',
          status: 'failed',
          created_at: '2026-10-24T10:00:00Z',
        } as CronRun,
      ],
    })
    const c = new CronController(api)
    await c.load({
      projects: projects.slice(0, 1),
      route: { ...defaultCronRoute(), tab, statuses: ['failed'] },
      authorityGeneration: 1,
      daemonID: undefined,
    })
    expect(api.jobs).toHaveBeenCalledWith(
      1,
      expect.any(AbortSignal),
      expect.objectContaining({ statuses: tab === 'jobs' ? ['failed'] : [] }),
    )
    expect(c.state.projects[0]?.jobs.map((job) => job.name)).toEqual(
      tab === 'runs' ? ['Inspect workspace'] : [],
    )
    expect(c.state.projects[0]?.runs[0]?.status).toBe('failed')
    c.destroy()
  },
)

test('cancelled reconciliation reloads before an unchanged snapshot can reuse the catalog', async () => {
  const api = client(),
    c = new CronController(api)
  const intent = {
    projects: projects.slice(0, 1),
    route: defaultCronRoute(),
    daemonID: undefined,
    authorityGeneration: 1,
  }
  await c.load(intent)
  api.runs.mockResolvedValue({
    runs: [{ uid: 'refreshed', created_at: '2026-10-25T00:00:00Z' } as CronRun],
  })
  let resolve!: (value: CronSummary) => void
  api.summary.mockImplementationOnce(
    () =>
      new Promise((r) => {
        resolve = r
      }),
  )
  const pending = c.invalidate()
  expect(c.state.loading).toBe(true)
  c.cancelNavigation()
  await c.sync({ ...intent, route: { ...intent.route, groupByProject: true } }, true)
  expect(api.summary).toHaveBeenCalledTimes(3)
  resolve({ failed_runs_24h: 0, window_start: '', window_end: '' })
  await pending
  expect(c.state.failedRuns24h).toBe(7)
  expect(c.state.projects[0]?.runs.map((run) => run.uid)).toEqual(['refreshed'])
  c.destroy()
})

test('unchanged snapshots reuse completed catalogs with denied projects without loading flashes', () =>
  hegel.testAsync(
    async (tc) => {
      // Bound materialized work while drawing arbitrary project and polling sequences.
      const denied = [false, ...tc.draw(gs.arrays(gs.booleans())).slice(0, 8), true]
      const groups = [false, true, ...tc.draw(gs.arrays(gs.booleans())).slice(0, 6)]
      const api = client(),
        c = new CronController(api)
      api.summary.mockImplementation(async (id) => {
        if (denied[id - 1]) throw new CronReadError(403)
        return { failed_runs_24h: 7, window_start: '', window_end: '' }
      })
      api.runs.mockImplementation(async (id) => ({
        runs: [{ uid: `run-${id}`, created_at: '2026-10-25T00:00:00Z' } as CronRun],
      }))
      const intent = {
        projects: denied.map((_, index) => ({
          id: index + 1,
          uid: `project-${index + 1}`,
          name: `Project ${index + 1}`,
        })),
        route: defaultCronRoute(),
        daemonID: undefined,
        authorityGeneration: 1,
      }
      try {
        await c.load(intent)
        const accepted = c.state
        const reads = [api.summary, api.jobs, api.workflows, api.runs]
        const counts = reads.map((read) => read.mock.calls.length)
        const loading: boolean[] = []
        c.subscribe((state) => loading.push(state.loading))
        for (const groupByProject of groups) {
          await c.sync({ ...intent, route: { ...intent.route, groupByProject } }, true)
          expect(c.state.projects).toEqual(accepted.projects)
          expect(c.state.failedRuns24h).toBe(7 * denied.filter((value) => !value).length)
          expect(c.state.badgePartial).toBe(true)
          expect(reads.map((read) => read.mock.calls.length)).toEqual(counts)
          expect(loading).not.toContain(true)
        }
      } finally {
        c.destroy()
      }
    },
    { testCases: 2000 },
  ))

test.each(['refresh', 'authority', 'snapshot'] as const)(
  '%s can recover a completed project denial',
  async (recovery) => {
    const api = client(),
      c = new CronController(api)
    let denied = true
    api.summary.mockImplementation(async (id) => {
      if (id === 2 && denied) throw new CronReadError(403)
      return { failed_runs_24h: 7, window_start: '', window_end: '' }
    })
    const intent = {
      projects: projects.slice(0, 2),
      route: defaultCronRoute(),
      daemonID: undefined,
      authorityGeneration: 1,
    }
    try {
      await c.load(intent)
      expect(c.state.projects.find((row) => row.project.uid === 'p2')?.error).toBe(
        'Project not readable',
      )
      denied = false
      if (recovery === 'refresh') await c.refresh()
      else if (recovery === 'authority') await c.sync({ ...intent, authorityGeneration: 2 }, true)
      else await c.sync(intent, false)
      expect(c.state.projects.map((row) => row.error)).toEqual([undefined, undefined])
      expect(c.state.failedRuns24h).toBe(14)
      expect(c.state.badgePartial).toBe(false)
      expect(api.summary).toHaveBeenCalledTimes(4)
    } finally {
      c.destroy()
    }
  },
)

test.each(['initial', 'refresh'] as const)(
  'unchanged snapshots retry a transient %s catalog failure',
  async (failure) => {
    const api = client(),
      c = new CronController(api)
    const intent = {
      projects: projects.slice(0, 1),
      route: defaultCronRoute(),
      daemonID: undefined,
      authorityGeneration: 1,
    }
    try {
      if (failure === 'refresh') await c.load(intent)
      api.runs.mockRejectedValueOnce(new CronReadError(503))
      await c.load(intent)
      expect(
        failure === 'initial' ? c.state.projects[0]?.error : c.state.projects[0]?.historyError,
      ).toBe(failure === 'initial' ? 'Cron unavailable' : 'History refresh unavailable')
      api.runs.mockResolvedValue({
        runs: [{ uid: 'recovered', created_at: '2026-10-25T00:00:00Z' } as CronRun],
      })
      await c.sync(intent, true)
      expect(c.state.projects[0]?.runs.map((run) => run.uid)).toEqual(['recovered'])
      expect(c.state.projects[0]?.error).toBeUndefined()
      expect(c.state.projects[0]?.historyError).toBeUndefined()
    } finally {
      c.destroy()
    }
  },
)

function issueWithComment(uid: string): ShowIssueResponseBody {
  return {
    issue: {
      id: 1,
      uid,
      short_id: 'abc4',
      project_id: 1,
      title: 'Inspect workspace',
      body: '',
      author: 'example-user',
      status: 'open',
      revision: 1,
      created_at: '2026-10-25T00:00:00Z',
      updated_at: '2026-10-25T00:00:00Z',
      labels: [],
      metadata: {},
    },
    comments: [
      {
        id: 1,
        uid: 'comment-one',
        issue_id: 1,
        author: 'example-user',
        body: 'Workspace inspection complete',
        created_at: '2026-10-25T00:00:00Z',
      },
    ],
    labels: [],
    links: [],
  }
}

test('background run reload keeps the readable issue peek', async () => {
  vi.useFakeTimers()
  const api = client(),
    c = new CronController(api)
  const previous = issueWithComment('issue-before')
  const initialRun: CronRun = {
    id: 1,
    uid: 'selected-run',
    project_id: 1,
    actor: 'example-user',
    status: 'succeeded',
    revision: 1,
    summary: { version: 1 },
    issue_uid: previous.issue.uid,
    created_at: '2026-10-25T00:00:00Z',
    updated_at: '2026-10-25T00:00:00Z',
  }
  const run = vi.fn<CronClient['run']>().mockResolvedValueOnce(initialRun)
  const issue = vi.fn<CronClient['issue']>().mockResolvedValueOnce(previous)
  api.run = run
  api.issue = issue
  const intent = {
    projects: projects.slice(0, 1),
    route: {
      ...defaultCronRoute(),
      selection: { kind: 'run' as const, projectUID: 'p1', uid: initialRun.uid },
    },
    daemonID: undefined,
    authorityGeneration: 1,
  }
  try {
    await c.load(intent)
    await vi.advanceTimersByTimeAsync(150)
    expect(c.state.peek).toEqual({ status: 'ready', issue: previous })
    let resolveRun!: (value: CronRun) => void
    let resolveIssue!: (value: ShowIssueResponseBody) => void
    let detailStarted!: () => void
    const detailRequest = new Promise<void>((resolve) => (detailStarted = resolve))
    run.mockImplementationOnce(() => {
      detailStarted()
      return new Promise((resolve) => (resolveRun = resolve))
    })
    issue.mockImplementationOnce(() => new Promise((resolve) => (resolveIssue = resolve)))
    const peeks: string[] = []
    c.subscribe((state) => peeks.push(state.peek.status))
    const pending = c.sync(intent, false)
    await detailRequest
    expect(c.state.peek).toEqual({ status: 'ready', issue: previous })
    resolveRun(initialRun)
    await pending
    await vi.advanceTimersByTimeAsync(150)
    expect(issue).toHaveBeenLastCalledWith(1, previous.issue.uid, expect.any(AbortSignal))
    expect(c.state.peek).toEqual({ status: 'ready', issue: previous })
    expect(peeks.every((status) => status === 'ready')).toBe(true)
    c.fence()
    resolveIssue(issueWithComment(previous.issue.uid))
    await Promise.resolve()
    expect(c.state.peek).toEqual({ status: 'no-issue' })
  } finally {
    c.destroy()
    vi.useRealTimers()
  }
})

test('explicit refresh rereads the selected job and its recent runs', async () => {
  const api = client()
  const job = vi.fn<CronClient['job']>(async () => ({ uid: 'job', name: 'Before' }) as never)
  api.job = job
  const recent = (uid: string) => ({ uid, created_at: '2026-10-25T00:00:00Z' }) as CronRun
  api.runs.mockImplementation(async (_project, _signal, query) => ({
    runs: query?.jobUID ? [recent('run-before')] : [],
  }))
  const c = new CronController(api)
  try {
    await c.load({
      projects: projects.slice(0, 1),
      route: {
        ...defaultCronRoute(),
        selection: { kind: 'job', projectUID: 'p1', uid: 'job' },
      },
      daemonID: undefined,
      authorityGeneration: 1,
    })
    expect(c.state.recentRuns.map((run) => run.uid)).toEqual(['run-before'])
    job.mockResolvedValue({ uid: 'job', name: 'After' } as never)
    api.runs.mockImplementation(async (_project, _signal, query) => ({
      runs: query?.jobUID ? [recent('run-after')] : [],
    }))
    await c.refresh()
    expect((c.state.detail as { name?: string } | undefined)?.name).toBe('After')
    expect(c.state.recentRuns.map((run) => run.uid)).toEqual(['run-after'])
  } finally {
    c.destroy()
  }
})
