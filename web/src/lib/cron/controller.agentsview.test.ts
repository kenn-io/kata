import { afterEach, beforeEach, expect, test, vi } from 'vitest'
import { AuthenticationRequiredError } from '../auth/session'
import type { KataTaskEvent } from '../kata/types'
import { CronClient, CronReadError } from './client'
import { CronController } from './controller'
import { defaultCronRoute } from './route'
import type { CronRun, ShowIssueResponseBody } from './types'

const project = { id: 7, uid: 'project-one', name: 'example-project' }
const other = { id: 8, uid: 'project-two', name: 'second-project' }
const locator = 'https://agentsview.example/sessions/abc'
const run = (uid: string, issueUID: string | undefined = `issue-${uid}`) =>
  ({
    uid,
    issue_uid: issueUID,
    status: 'failed',
    created_at: '2026-10-25T10:00:00Z',
    summary: { version: 1, message: `Run ${uid}` },
  }) as CronRun
const closed = (projectUID: string, issueUID: string, url = locator) =>
  ({
    event_id: 12,
    type: 'issue.closed',
    project_uid: projectUID,
    issue_uid: issueUID,
    payload: { transcript: { url } },
  }) as unknown as KataTaskEvent
const issue = (uid: string, projectID = project.id) =>
  ({
    issue: { uid, project_id: projectID, title: 'Readable issue' },
    comments: [{ uid: 'c', body: 'Latest', author: 'a', created_at: '2026-10-25T11:00:00Z' }],
  }) as unknown as ShowIssueResponseBody

function client(enabled = true) {
  return Object.assign(new CronClient(), {
    summary: vi.fn<CronClient['summary']>(async () => ({
      failed_runs_24h: 0,
      window_start: '',
      window_end: '',
    })),
    jobs: vi.fn<CronClient['jobs']>(async () => ({ jobs: [] })),
    runs: vi.fn<CronClient['runs']>(async () => ({ runs: [] })),
    workflows: vi.fn<CronClient['workflows']>(async () => ({ workflows: [] })),
    run: vi.fn<CronClient['run']>(async (_project, uid) => run(uid)),
    issue: vi.fn<CronClient['issue']>(async (projectID, uid) => issue(uid, projectID)),
    capabilities: vi.fn<CronClient['capabilities']>(async () => ({
      agentsviewEnabled: enabled,
    })),
    issueHistory: vi.fn<CronClient['issueHistory']>(async (projectUID, issueUID) => [
      closed(projectUID, issueUID),
    ]),
  })
}
const intent = (selectionUID = 'a', projects = [project], daemonID = 'local') => ({
  projects,
  daemonID,
  authorityGeneration: 1,
  route: {
    ...defaultCronRoute(),
    tab: 'runs' as const,
    selection: { kind: 'run' as const, projectUID: projects[0]!.uid, uid: selectionUID },
  },
})
async function settle() {
  await vi.advanceTimersByTimeAsync(200)
}
beforeEach(() => vi.useFakeTimers())
afterEach(() => vi.useRealTimers())

test('AgentsViewCron enabled capability exposes the recorded locator for the run issue', async () => {
  const api = client(true)
  const controller = new CronController(api)
  await controller.load(intent())
  await settle()
  expect(controller.state.relatedSessionURL).toBe(locator)
  expect(api.capabilities).toHaveBeenCalledWith(project.id, expect.any(AbortSignal))
  expect(api.issueHistory).toHaveBeenCalledWith(project.uid, 'issue-a', expect.any(AbortSignal))
  expect(controller.state.peek.status).toBe('ready')
  controller.destroy()
})

test('AgentsViewCron disabled capability makes zero optional history reads', async () => {
  const api = client(false)
  const controller = new CronController(api)
  await controller.load(intent())
  await settle()
  expect(api.capabilities).toHaveBeenCalledOnce()
  expect(api.issueHistory).not.toHaveBeenCalled()
  expect(controller.state.relatedSessionURL).toBeUndefined()
  expect(controller.state.peek.status).toBe('ready')
  controller.destroy()
})

test('AgentsViewCron runs without an issue make no optional reads', async () => {
  const api = client(true)
  api.run.mockImplementation(async (_project, uid) => {
    const withoutIssue = run(uid)
    delete withoutIssue.issue_uid
    return withoutIssue
  })
  const controller = new CronController(api)
  await controller.load(intent())
  await settle()
  expect(api.capabilities).not.toHaveBeenCalled()
  expect(api.issueHistory).not.toHaveBeenCalled()
  controller.destroy()
})

test.each([
  ['capability 404', 'capability', () => Promise.reject(new CronReadError(404))],
  ['capability timeout', 'capability', () => new Promise<never>(() => {})],
  ['history 403', 'history', () => Promise.reject(new CronReadError(403))],
  ['history failure', 'history', () => Promise.reject(new Error('Snapshot unavailable'))],
  ['history timeout', 'history', () => new Promise<never>(() => {})],
] as const)('AgentsViewCron %s leaves ordinary reads intact', async (_name, target, outcome) => {
  const api = client(true)
  const reject = vi.fn()
  if (target === 'capability') api.capabilities.mockImplementation(outcome)
  else api.issueHistory.mockImplementation(outcome)
  const controller = new CronController(api, reject)
  await controller.load(intent())
  await settle()
  expect(controller.state.detail).toMatchObject({ uid: 'a' })
  expect(controller.state.peek.status).toBe('ready')
  expect(controller.state.relatedSessionURL).toBeUndefined()
  expect(controller.state.projects).toHaveLength(1)
  expect(reject).not.toHaveBeenCalled()
  controller.destroy()
})

test.each(['capability', 'history'] as const)(
  'AgentsViewCron %s 401 keeps the existing credential rejection and fence',
  async (target) => {
    const api = client(true)
    const reject = vi.fn()
    const rejection = () =>
      Promise.reject(new AuthenticationRequiredError('Authentication required'))
    if (target === 'capability') api.capabilities.mockImplementation(rejection)
    else api.issueHistory.mockImplementation(rejection)
    const controller = new CronController(api, reject)
    await controller.load(intent())
    await settle()
    expect(reject).toHaveBeenCalledOnce()
    expect(controller.state.projects).toEqual([])
    expect(controller.state.detail).toBeUndefined()
    expect(controller.state.relatedSessionURL).toBeUndefined()
    controller.destroy()
  },
)

test('AgentsViewCron selection change discards a late locator from the previous run', async () => {
  const api = client(true)
  let release!: (events: KataTaskEvent[]) => void
  api.issueHistory.mockImplementationOnce(
    (projectUID, issueUID) =>
      new Promise((resolve) => (release = () => resolve([closed(projectUID, issueUID)]))),
  )
  api.issueHistory.mockImplementationOnce(async () => [])
  const controller = new CronController(api)
  await controller.load(intent('a'))
  await settle()
  await controller.select({ kind: 'run', projectUID: project.uid, uid: 'b' })
  await settle()
  release([])
  await settle()
  expect(controller.state.selected?.uid).toBe('b')
  expect(controller.state.relatedSessionURL).toBeUndefined()
  controller.destroy()
})

test('AgentsViewCron daemon or project switch discards stale capability and history', async () => {
  const api = client(true)
  let releaseCapability!: () => void
  api.capabilities.mockImplementationOnce(
    () =>
      new Promise((resolve) => {
        releaseCapability = () => resolve({ agentsviewEnabled: true })
      }),
  )
  api.capabilities.mockImplementationOnce(async () => ({ agentsviewEnabled: false }))
  const controller = new CronController(api)
  await controller.load(intent('a'))
  await settle()
  await controller.load({ ...intent('b', [other], 'remote'), authorityGeneration: 2 })
  await settle()
  releaseCapability()
  await settle()
  expect(controller.state.selected).toMatchObject({ projectUID: other.uid, uid: 'b' })
  expect(api.issueHistory).not.toHaveBeenCalled()
  expect(controller.state.relatedSessionURL).toBeUndefined()
  controller.destroy()
})

test('AgentsViewCron disable then re-enable restores only the current locator', async () => {
  const api = client(true)
  const controller = new CronController(api)
  await controller.load(intent())
  await settle()
  expect(controller.state.relatedSessionURL).toBe(locator)
  api.capabilities.mockImplementation(async () => ({ agentsviewEnabled: false }))
  const reads = api.issueHistory.mock.calls.length
  await controller.refresh()
  await settle()
  expect(controller.state.relatedSessionURL).toBeUndefined()
  expect(api.issueHistory.mock.calls.length).toBe(reads)
  api.capabilities.mockImplementation(async () => ({ agentsviewEnabled: true }))
  await controller.refresh()
  await settle()
  expect(controller.state.relatedSessionURL).toBe(locator)
  controller.destroy()
})

test('AgentsViewCron reads a moved issue history from its current project', async () => {
  const api = client(true)
  api.issue.mockImplementation(async (_project, uid) => issue(uid, other.id))
  const controller = new CronController(api)
  await controller.load(intent('a', [project, other]))
  await settle()
  expect(api.capabilities).toHaveBeenCalledWith(project.id, expect.any(AbortSignal))
  expect(api.issueHistory).toHaveBeenCalledWith(other.uid, 'issue-a', expect.any(AbortSignal))
  expect(controller.state.relatedSessionURL).toBe(locator)
  controller.destroy()
})

test('AgentsViewCron makes no history read when the issue is unreadable or moved out of reach', async () => {
  for (const outcome of [
    () => Promise.reject(new CronReadError(404)),
    async (_project: number, uid: string) => issue(uid, 99),
  ]) {
    const api = client(true)
    api.issue.mockImplementation(outcome)
    const controller = new CronController(api)
    await controller.load(intent())
    await settle()
    expect(api.issueHistory).not.toHaveBeenCalled()
    expect(controller.state.relatedSessionURL).toBeUndefined()
    controller.destroy()
  }
})
