import { describe, expect, test, vi } from 'vitest'
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
  // Revalidating rows already on screen is not a loading state.
  expect(c.state.loading).toBe(false)
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

test.each(['without', 'with'] as const)(
  'changed snapshots during a catalog load %s a selection wait for it, then refresh once',
  async (selected) => {
    const api = client()
    api.run = vi.fn(async () => ({ uid: 'r', created_at: '', summary: { version: 1 } }) as CronRun)
    const held: (() => void)[] = []
    api.summary.mockImplementation(
      () =>
        new Promise((resolve) =>
          held.push(() => resolve({ failed_runs_24h: 7, window_start: '', window_end: '' })),
        ),
    )
    const c = new CronController(api)
    const intent = {
      projects: projects.slice(0, 2),
      route: {
        ...defaultCronRoute(),
        ...(selected === 'with'
          ? { selection: { kind: 'run' as const, projectUID: 'p1', uid: 'r' } }
          : {}),
      },
      daemonID: undefined,
      authorityGeneration: 1,
    }
    try {
      const first = c.sync(intent, false)
      await vi.waitFor(() => expect(held).toHaveLength(2))
      const overlapping = [c.sync(intent, false), c.sync(intent, false), c.sync(intent, false)]
      expect(api.summary).toHaveBeenCalledTimes(2)
      for (const release of held.splice(0)) release()
      await vi.waitFor(() => expect(held).toHaveLength(2))
      expect(c.state.projects.map((row) => row.project.uid).sort()).toEqual(['p1', 'p2'])
      expect(c.state.failedRuns24h).toBe(14)
      for (const release of held.splice(0)) release()
      await Promise.all([first, ...overlapping])
      expect(api.summary).toHaveBeenCalledTimes(4)
      expect(c.state.loading).toBe(false)
    } finally {
      c.destroy()
    }
  },
)

test('an authority change during a catalog load still aborts it', async () => {
  const api = client()
  let release!: () => void
  api.summary.mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        release = () => resolve({ failed_runs_24h: 99, window_start: '', window_end: '' })
      }),
  )
  const c = new CronController(api)
  const intent = {
    projects: projects.slice(0, 1),
    route: defaultCronRoute(),
    daemonID: undefined,
    authorityGeneration: 1,
  }
  try {
    const first = c.sync(intent, false)
    await vi.waitFor(() => expect(release).toBeTypeOf('function'))
    await c.sync({ ...intent, authorityGeneration: 2 }, false)
    expect(c.state.failedRuns24h).toBe(7)
    release()
    await first
    expect(c.state.failedRuns24h).toBe(7)
    expect(api.summary).toHaveBeenCalledTimes(2)
  } finally {
    c.destroy()
  }
})

describe('overlapping events', () => {
  const one = projects.slice(0, 1)
  const base = { projects: one, daemonID: undefined, authorityGeneration: 1 }
  const withRun = (uid = 'r') => ({
    ...base,
    route: { ...defaultCronRoute(), selection: { kind: 'run' as const, projectUID: 'p1', uid } },
  })
  function held<T>() {
    const calls: { signal: AbortSignal; resolve: (value: T) => void }[] = []
    const fn = (_project: number, _uid: string, signal: AbortSignal) =>
      new Promise<T>((resolve) => calls.push({ signal, resolve }))
    return { calls, fn }
  }
  const run = (uid: string, issueUID?: string) =>
    ({ uid, created_at: '', summary: { version: 1 }, issue_uid: issueUID }) as CronRun

  test('changed snapshots keep an open detail read and revalidate it once afterwards', async () => {
    const api = client()
    const detail = held<CronRun>()
    api.run = vi.fn(detail.fn)
    const c = new CronController(api)
    try {
      await c.load({ ...base, route: defaultCronRoute() })
      const opened = c.select(withRun().route.selection)
      expect(detail.calls).toHaveLength(1)
      await Promise.all([c.sync(withRun(), false), c.sync(withRun(), false)])
      expect(detail.calls[0]!.signal.aborted).toBe(false)
      expect(detail.calls).toHaveLength(1)
      detail.calls[0]!.resolve(run('r'))
      await opened
      expect(c.state.detail?.uid).toBe('r')
      await vi.waitFor(() => expect(detail.calls).toHaveLength(2))
      detail.calls[1]!.resolve(run('r'))
      void c.sync(withRun(), false)
      await vi.waitFor(() => expect(detail.calls).toHaveLength(3))
      expect(c.state.detailLoading).toBe(false)
    } finally {
      c.destroy()
    }
  })

  test('catalog revalidates the open drawer before a queued sweep starts', async () => {
    const api = client()
    const events: string[] = []
    api.run = vi.fn(async (_project, uid) => {
      events.push('drawer')
      return run(uid)
    })
    const sweeps: (() => void)[] = []
    let sweepCount = 0
    const c = new CronController(api)
    const selected = withRun()
    const selection = selected.route.selection!
    const pending: Promise<void>[] = []
    try {
      await c.load({ ...base, route: defaultCronRoute() })
      await c.select(selection)
      events.length = 0
      api.summary.mockImplementation(
        () =>
          new Promise((resolve) => {
            events.push(`catalog:${++sweepCount}`)
            sweeps.push(() => resolve({ failed_runs_24h: 7, window_start: '', window_end: '' }))
          }),
      )

      pending.push(c.sync(selected, false))
      await vi.waitFor(() => expect(sweeps).toHaveLength(1))
      pending.push(c.sync(selected, false))
      sweeps.shift()!()

      await vi.waitFor(() =>
        expect(events.slice(0, 3)).toEqual(['catalog:1', 'drawer', 'catalog:2']),
      )
      sweeps.shift()!()
      await Promise.all(pending)
    } finally {
      sweeps.splice(0).forEach((release) => release())
      await Promise.all(pending)
      c.destroy()
    }
  })

  test('a successful queued detail retry clears its earlier error', async () => {
    const api = client()
    const c = new CronController(api)
    let rejectFirst!: (error: Error) => void
    api.run = vi
      .fn<CronClient['run']>()
      .mockImplementationOnce(
        () =>
          new Promise((_resolve, reject) => {
            rejectFirst = reject
          }),
      )
      .mockResolvedValueOnce(run('r'))
    const selection = { kind: 'run' as const, projectUID: 'p1', uid: 'r' }
    let pending: Promise<void> | undefined
    try {
      await c.load({ ...base, route: defaultCronRoute() })
      pending = c.select(selection)
      await vi.waitFor(() => expect(rejectFirst).toBeTypeOf('function'))
      await c.refresh()
      rejectFirst(new CronReadError(503))

      await vi.waitFor(() => expect(api.run).toHaveBeenCalledTimes(2))
      await vi.waitFor(() => expect(c.state.detail?.uid).toBe('r'))
      expect(c.state.detailError).toBeUndefined()
      await pending
    } finally {
      await pending
      c.destroy()
    }
  })

  test('changed snapshots keep an open issue peek', async () => {
    vi.useFakeTimers()
    const api = client()
    api.run = vi.fn(async () => run('r', 'issue'))
    const peek = held<ShowIssueResponseBody>()
    api.issue = vi.fn(peek.fn)
    const c = new CronController(api)
    try {
      await c.load(withRun())
      await vi.advanceTimersByTimeAsync(150)
      expect(peek.calls).toHaveLength(1)
      await c.sync(withRun(), false)
      expect(peek.calls[0]!.signal.aborted).toBe(false)
      peek.calls[0]!.resolve(issueWithComment('issue'))
      await vi.advanceTimersByTimeAsync(0)
      expect(c.state.peek.status).toBe('ready')
    } finally {
      c.destroy()
      vi.useRealTimers()
    }
  })

  test('a coalesced catalog refresh follows the latest drawer selection', async () => {
    const api = client()
    api.run = vi.fn(async (_project, uid) => run(uid))
    let release!: () => void
    api.summary.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          release = () => resolve({ failed_runs_24h: 7, window_start: '', window_end: '' })
        }),
    )
    const c = new CronController(api)
    try {
      const first = c.sync(withRun('a'), false)
      await vi.waitFor(() => expect(release).toBeTypeOf('function'))
      const queued = c.sync(withRun('a'), false)
      await c.select(undefined)
      release()
      await Promise.all([first, queued])
      expect(c.state.selected).toBeUndefined()
      expect(c.state.detail).toBeUndefined()
      await c.select({ kind: 'run', projectUID: 'p1', uid: 'b' })
      expect(c.state.detail?.uid).toBe('b')
    } finally {
      c.destroy()
    }
  })

  test('a failed job filter refresh never shows jobs from another filter', async () => {
    const api = client()
    const ran = { uid: 'job', name: 'Inspect workspace', last_run: run('r') } as never
    api.jobs.mockResolvedValueOnce({ jobs: [ran] })
    const c = new CronController(api)
    try {
      await c.load({ ...base, route: { ...defaultCronRoute(), tab: 'jobs' } })
      expect(c.state.projects[0]?.jobs).toHaveLength(1)
      api.jobs.mockRejectedValueOnce(new CronReadError(503))
      await c.load({
        ...base,
        route: { ...defaultCronRoute(), tab: 'jobs', statuses: ['never_ran'] },
      })
      const row = c.state.projects[0]!
      expect(row.jobs).toEqual([])
      expect(row.jobsError).toBe('Cron unavailable')
      expect(row.error).toBeUndefined()
    } finally {
      c.destroy()
    }
  })

  test('badge sweeps finish under repeated snapshots and refresh once after changes', async () => {
    const api = client()
    const sweeps: (() => void)[] = []
    api.summary.mockImplementation(
      () =>
        new Promise((resolve) =>
          sweeps.push(() => resolve({ failed_runs_24h: 7, window_start: '', window_end: '' })),
        ),
    )
    const c = new CronController(api)
    const intent = { ...base, projects: projects.slice(0, 2), route: defaultCronRoute() }
    try {
      const first = c.loadBadge(intent, false)
      await vi.waitFor(() => expect(sweeps).toHaveLength(2))
      const repeats = [
        c.loadBadge(intent, true),
        c.loadBadge(intent, false),
        c.loadBadge(intent, true),
      ]
      expect(api.summary).toHaveBeenCalledTimes(2)
      for (const release of sweeps.splice(0)) release()
      await vi.waitFor(() => expect(c.state.failedRuns24h).toBe(14))
      await vi.waitFor(() => expect(sweeps).toHaveLength(2))
      for (const release of sweeps.splice(0)) release()
      await Promise.all([first, ...repeats])
      expect(api.summary).toHaveBeenCalledTimes(4)
      await c.loadBadge(intent, true)
      expect(api.summary).toHaveBeenCalledTimes(4)
    } finally {
      c.destroy()
    }
  })
})

test('a destroyed controller ignores requests from an unmounted page', async () => {
  const api = client()
  api.run = vi.fn(async () => ({ uid: 'r', created_at: '', summary: { version: 1 } }) as CronRun)
  const c = new CronController(api)
  c.destroy()
  const intent = {
    projects: projects.slice(0, 1),
    route: {
      ...defaultCronRoute(),
      selection: { kind: 'run' as const, projectUID: 'p1', uid: 'r' },
    },
    daemonID: undefined,
    authorityGeneration: 1,
  }
  await c.sync(intent, false)
  await c.loadBadge(intent, false)
  await c.select(intent.route.selection)
  await c.refresh()
  await c.refreshSummaries()
  for (const read of [api.summary, api.jobs, api.runs, api.workflows, api.run])
    expect(read).not.toHaveBeenCalled()
})

test('older history loads during a background sweep and survives it', async () => {
  const api = client()
  const at = (uid: string, time: string) => ({ uid, created_at: time }) as CronRun
  api.runs.mockImplementation(async (_project, _signal, query) =>
    query?.beforeUID === 'cursor-1'
      ? { runs: [at('older', '2026-10-23T00:00:00Z')], next_before_uid: 'cursor-2' }
      : { runs: [at('newest', '2026-10-25T00:00:00Z')], next_before_uid: 'cursor-1' },
  )
  const c = new CronController(api)
  const intent = {
    projects: projects.slice(0, 1),
    route: defaultCronRoute(),
    daemonID: undefined,
    authorityGeneration: 1,
  }
  try {
    await c.load(intent)
    let release!: () => void
    api.summary.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          release = () => resolve({ failed_runs_24h: 7, window_start: '', window_end: '' })
        }),
    )
    const sweep = c.sync(intent, false)
    await vi.waitFor(() => expect(release).toBeTypeOf('function'))
    expect(c.state.loading).toBe(false)
    await c.loadMore('p1')
    expect(c.state.projects[0]?.runs.map((run) => run.uid)).toEqual(['newest', 'older'])
    release()
    await sweep
    expect(c.state.projects[0]?.runs.map((run) => run.uid)).toEqual(['newest', 'older'])
    expect(c.state.projects[0]?.nextBeforeUID).toBe('cursor-2')
  } finally {
    c.destroy()
  }
})

test('a run that leaves the selected status disappears from refreshed history', async () => {
  const api = client()
  const at = (uid: string, time: string) =>
    ({ uid, status: 'running', created_at: time }) as CronRun
  api.runs.mockResolvedValue({
    runs: [at('newer', '2026-10-25T00:00:00Z'), at('older', '2026-10-24T00:00:00Z')],
  })
  const c = new CronController(api)
  const intent = {
    projects: projects.slice(0, 1),
    route: { ...defaultCronRoute(), tab: 'runs' as const, statuses: ['running'] },
    daemonID: undefined,
    authorityGeneration: 1,
  }
  try {
    await c.load(intent)
    api.runs.mockResolvedValue({ runs: [at('newer', '2026-10-25T00:00:00Z')] })
    await c.sync(intent, false)
    expect(c.state.projects[0]?.runs.map((run) => run.uid)).toEqual(['newer'])
  } finally {
    c.destroy()
  }
})

test('catalog updates finish while a drawer revalidation is delayed', async () => {
  const api = client()
  const held: (() => void)[] = []
  let calls = 0
  api.run = vi.fn((_project, uid) => {
    calls++
    const value = { uid, created_at: '', summary: { version: 1 } } as CronRun
    if (calls === 1) return Promise.resolve(value)
    return new Promise<CronRun>((resolve) => held.push(() => resolve(value)))
  })
  const c = new CronController(api)
  const intent = {
    projects: projects.slice(0, 1),
    route: {
      ...defaultCronRoute(),
      selection: { kind: 'run' as const, projectUID: 'p1', uid: 'r' },
    },
    daemonID: undefined,
    authorityGeneration: 1,
  }
  try {
    await c.load(intent)
    const summaries = api.summary.mock.calls.length
    await c.sync(intent, false)
    await vi.waitFor(() => expect(held).toHaveLength(1))
    await c.sync(intent, false)
    expect(api.summary.mock.calls.length).toBe(summaries + 2)
    expect(c.state.loading).toBe(false)
    for (const release of held.splice(0)) release()
  } finally {
    c.destroy()
  }
})
