import { describe, expect, it, vi } from 'vitest'

import { parseRoute } from '../router'
import {
  createUISnapshotRequest,
  projectUISnapshot,
  SnapshotController,
  snapshotIntentForRoute,
  snapshotRequestIntent,
  type SnapshotRequest,
  type UISnapshot,
} from './snapshot'

interface TestIntent {
  key: string
}

interface TestSnapshot {
  cursor: number
  capabilities: { writable: boolean; updates: 'sse' | 'poll' }
  value: string
}

describe('SnapshotController', () => {
  it('aborts the previous generation and rejects a slow old route', async () => {
    const pending: Array<ReturnType<typeof deferredSnapshot>> = []
    const request: SnapshotRequest<TestIntent, TestSnapshot> = (intent, options) => {
      const deferred = deferredSnapshot()
      deferred.intent = intent
      deferred.signal = options.signal
      pending.push(deferred)
      return deferred.promise
    }
    const controller = new SnapshotController(request, (intent) => intent.key)

    const oldLoad = controller.load({ key: 'old-route' })
    const newLoad = controller.load({ key: 'new-route' })
    expect(pending[0]?.signal?.aborted).toBe(true)
    pending[1]?.resolve({
      status: 200,
      etag: '"new"',
      snapshot: snapshot('new-route', 2),
    })
    await newLoad
    pending[0]?.resolve({
      status: 200,
      etag: '"old"',
      snapshot: snapshot('old-route', 1),
    })
    await oldLoad

    expect(controller.state.intent).toEqual({ key: 'new-route' })
    expect(controller.state.snapshot?.value).toBe('new-route')
    expect(controller.state.cursor).toBe(2)
  })

  it('retains accepted authority on 304 and hands its cursor to subscribers', async () => {
    const requests: Array<{ etag?: string }> = []
    const accepted: number[] = []
    const request: SnapshotRequest<TestIntent, TestSnapshot> = async (_intent, options) => {
      requests.push(options.etag ? { etag: options.etag } : {})
      if (requests.length === 1) {
        return { status: 200, etag: '"first"', snapshot: snapshot('first', 17) }
      }
      return { status: 304, etag: '"first"' }
    }
    const controller = new SnapshotController(request, (intent) => intent.key)
    controller.subscribe((state) => {
      if (state.snapshot && !state.loading) accepted.push(state.cursor)
    })

    await controller.load({ key: 'same-route' })
    const first = controller.state.snapshot
    await controller.load({ key: 'same-route' })

    expect(requests[1]?.etag).toBe('"first"')
    expect(controller.state.snapshot).toBe(first)
    expect(controller.state.stale).toBe(false)
    expect(accepted.at(-1)).toBe(17)
  })

  it('requests retries only before the first snapshot is accepted', async () => {
    const request = vi
      .fn<SnapshotRequest<TestIntent, TestSnapshot>>()
      .mockResolvedValue({ status: 200, etag: '"ok"', snapshot: snapshot('safe', 4) })
    const controller = new SnapshotController(request, (intent) => intent.key)
    await controller.load({ key: 'same-route' })
    await controller.load({ key: 'same-route' })
    expect(request.mock.calls.map(([, options]) => options.retry)).toEqual([true, false])
  })

  it('retains but stale-marks authority after a safe generic failure', async () => {
    const request = vi
      .fn<SnapshotRequest<TestIntent, TestSnapshot>>()
      .mockResolvedValueOnce({ status: 200, etag: '"ok"', snapshot: snapshot('safe', 4) })
      .mockRejectedValueOnce(new Error('request carried a credential-shaped value'))
    const controller = new SnapshotController(request, (intent) => intent.key)

    expect(await controller.load({ key: 'same-route' })).toBe(true)
    expect(await controller.load({ key: 'same-route' })).toBe(false)

    expect(controller.state.snapshot?.value).toBe('safe')
    expect(controller.state.stale).toBe(true)
    expect(controller.state.canMutate).toBe(false)
    expect(controller.state.error).toBe("Couldn't load workspace")
  })

  it('distinguishes expired browser authority from a transient snapshot failure', async () => {
    const fetcher = vi.fn(async () => new Response('', { status: 401 }))
    const request: SnapshotRequest<TestIntent, TestSnapshot> = async (_intent, options) => {
      const response = await fetcher()
      if (options.signal.aborted) throw new DOMException('Aborted', 'AbortError')
      if (!response.ok) {
        const error = new Error('Snapshot unavailable')
        error.name = response.status === 401 ? 'AuthenticationRequiredError' : 'Error'
        throw error
      }
      throw new Error('unexpected response')
    }
    const controller = new SnapshotController(request, (intent) => intent.key)

    await controller.load({ key: 'same-route' })

    expect(controller.state).toMatchObject({
      loading: false,
      stale: false,
      canMutate: false,
      authenticationRequired: true,
    })
  })

  it('revalidates the accepted intent without withdrawing write authority', async () => {
    const pending: Array<ReturnType<typeof deferredSnapshot>> = []
    const request: SnapshotRequest<TestIntent, TestSnapshot> = () => {
      const deferred = deferredSnapshot()
      pending.push(deferred)
      return deferred.promise
    }
    const controller = new SnapshotController(request, (intent) => intent.key)
    const first = controller.load({ key: 'same-route' })
    pending[0]?.resolve({ status: 200, etag: '"first"', snapshot: snapshot('first', 3) })
    await first

    const revalidation = controller.load({ key: 'same-route' })

    expect(controller.state).toMatchObject({ loading: false, canMutate: true })
    pending[1]?.resolve({ status: 200, etag: '"second"', snapshot: snapshot('second', 4) })
    await revalidation
    expect(controller.state.snapshot?.value).toBe('second')
    expect(controller.state.canMutate).toBe(true)
  })

  it('withdraws write authority while a full reload or another intent loads', async () => {
    const pending: Array<ReturnType<typeof deferredSnapshot>> = []
    const request: SnapshotRequest<TestIntent, TestSnapshot> = () => {
      const deferred = deferredSnapshot()
      pending.push(deferred)
      return deferred.promise
    }
    const controller = new SnapshotController(request, (intent) => intent.key)
    const first = controller.load({ key: 'same-route' })
    pending[0]?.resolve({ status: 200, etag: '"first"', snapshot: snapshot('first', 3) })
    await first

    void controller.load({ key: 'same-route' }, { full: true })
    expect(controller.state).toMatchObject({ loading: true, canMutate: false })
    void controller.load({ key: 'other-route' })
    expect(controller.state).toMatchObject({ loading: true, canMutate: false })
  })

  it('clears accepted authority before changing daemon ownership', async () => {
    const controller = new SnapshotController<TestIntent, TestSnapshot>(
      async () => ({ status: 200, etag: '"first"', snapshot: snapshot('first', 9) }),
      (intent) => intent.key,
    )
    await controller.load({ key: 'example-local' })

    controller.clear()

    expect(controller.state).toMatchObject({
      intent: undefined,
      snapshot: undefined,
      etag: undefined,
      cursor: 0,
      loading: false,
      canMutate: false,
    })
  })
})

describe('snapshotIntentForRoute', () => {
  it('includes the browser timezone for non-calendar collections', () => {
    const route = parseRoute(new URL('https://daemon.example/kata?view=all-open'))
    if (route.kind === 'route-error') throw new Error('expected a Kata route')

    const intent = snapshotIntentForRoute(
      route,
      new Date('2026-09-01T00:30:00Z'),
      'America/Los_Angeles',
    )

    expect(intent.timeZone).toBe('America/Los_Angeles')
    expect(intent.localDate).toBeUndefined()
  })

  it('loads Inbox, Scheduled, and the landing route from the all-open collection', () => {
    for (const url of [
      'https://daemon.example/kata',
      'https://daemon.example/kata?view=inbox',
      'https://daemon.example/kata?view=scheduled',
    ]) {
      const route = parseRoute(new URL(url))
      if (route.kind === 'route-error') throw new Error('expected a Kata route')

      const intent = snapshotIntentForRoute(route, new Date('2026-09-01T00:30:00Z'), 'UTC')

      expect(intent.view, url).toBe('all-open')
      expect(intent.localDate, url).toBeUndefined()
      expect(intent.timeZone, url).toBe('UTC')
    }
  })

  it('keeps Today on the calendar collection with the browser date', () => {
    const route = parseRoute(new URL('https://daemon.example/kata?view=today'))
    if (route.kind === 'route-error') throw new Error('expected a Kata route')

    const intent = snapshotIntentForRoute(
      route,
      new Date('2026-09-01T00:30:00Z'),
      'America/Los_Angeles',
    )

    expect(intent.view).toBe('today')
    expect(intent.localDate).toBe('2026-08-31')
  })

  it.each(['credentials', 'settings'])(
    'maps the %s screen to an unselected ordinary authority snapshot',
    (view) => {
      const route = parseRoute(
        new URL(
          `https://daemon.example/kata?view=${view}&issue=01HZNQ7VFPK1XGD8R5MABCD4EX&owner=private-actor`,
        ),
      )
      if (route.kind === 'route-error') throw new Error('expected credentials route')

      expect(snapshotIntentForRoute(route, new Date('2026-09-15T12:00:00Z'), 'UTC')).toEqual({
        view: 'all-open',
        statuses: [],
        owners: [],
        labels: [],
        relationships: [],
        includeGraph: false,
        includeHistory: false,
        timeZone: 'UTC',
      })
    },
  )
})

describe('snapshotRequestIntent', () => {
  it('requests every project, since the project list is filtered from it', () => {
    expect(
      snapshotRequestIntent({
        view: 'all-open',
        projectUID: '01J00000000000000000000002',
        statuses: ['open'],
        owners: ['user-a'],
        labels: [],
        relationships: [],
        text: 'deploy',
        includeGraph: false,
        includeHistory: false,
        timeZone: 'UTC',
      }),
    ).toEqual({
      view: 'all-open',
      statuses: ['open'],
      owners: ['user-a'],
      labels: [],
      relationships: [],
      text: 'deploy',
      includeGraph: false,
      includeHistory: false,
      timeZone: 'UTC',
    })
  })
})

describe('projectUISnapshot', () => {
  it('keeps one project and the links that touch it, like the daemon project view', () => {
    const issue = (uid: string, projectUID: string) => ({
      uid,
      project_uid: projectUID,
      title: uid,
    })
    const link = (from: string, to: string) => ({
      from_issue_uid: from,
      to_issue_uid: to,
      type: 'blocks',
    })
    const all = {
      contract_version: '2',
      cursor: 41,
      capabilities: { writable: true, updates: 'sse' },
      origin: 'https://daemon.example',
      origin_stable: true,
      catalog: [{ project: { uid: 'project-a' } }, { project: { uid: 'project-b' } }],
      collection: [
        issue('issue-a1', 'project-a'),
        issue('issue-b1', 'project-b'),
        issue('issue-a2', 'project-a'),
      ],
      collection_links: [
        link('issue-a1', 'issue-b1'),
        link('issue-b1', 'issue-x9'),
        link('issue-x8', 'issue-a2'),
      ],
    } as unknown as UISnapshot

    const projected = projectUISnapshot(all, 'project-a')

    expect(projected.cursor).toBe(41)
    expect(projected.catalog).toBe(all.catalog)
    expect(projected.collection.map(({ uid }) => uid)).toEqual(['issue-a1', 'issue-a2'])
    expect(projected.collection_links).toEqual([
      link('issue-a1', 'issue-b1'),
      link('issue-x8', 'issue-a2'),
    ])
  })
})

describe('createUISnapshotRequest', () => {
  it('delegates snapshot transport and query serialization to the generated client', async () => {
    const generatedRequest = vi.fn(async () => ({
      status: 200 as const,
      headers: new Headers({ ETag: '"snapshot"' }),
      data: {
        cursor: 9,
        capabilities: { writable: true, updates: 'poll' },
      } as UISnapshot,
    }))
    const request = createUISnapshotRequest(generatedRequest)
    const signal = new AbortController().signal

    const response = await request(
      {
        view: 'all-open',
        statuses: ['open', 'closed'],
        owners: ['agent-a'],
        labels: ['urgent'],
        relationships: ['blocks'],
        includeGraph: true,
        includeHistory: false,
        timeZone: 'America/New_York',
      },
      { signal, etag: '"previous"', full: false },
    )

    expect(generatedRequest).toHaveBeenCalledWith(
      {
        view: 'all-open',
        status: ['open', 'closed'],
        owner: ['agent-a'],
        label: ['urgent'],
        relationship: ['blocks'],
        include_graph: true,
        include_history: false,
        time_zone: 'America/New_York',
      },
      { signal, headers: { 'If-None-Match': '"previous"' } },
    )
    expect(response).toEqual({
      status: 200,
      etag: '"snapshot"',
      snapshot: expect.objectContaining({ cursor: 9 }),
    })
  })

  it.each([
    {
      label: 'unknown scope kind',
      capabilities: {
        writable: true,
        updates: 'poll',
        actor_policy: 'token',
        scope: {
          kind: 'project',
          project_uid: '01HZNQ7VFPK1XGD8R5MABCD4EX',
          root_issue_uid: '01HZNQ7VFPK1XGD8R5MABCD5YZ',
        },
        expires_at: '2099-01-01T00:00:00Z',
        allowed_actions: ['issue.read'],
        close_requires_evidence: true,
      },
    },
    {
      label: 'expired scope',
      capabilities: {
        writable: true,
        updates: 'poll',
        actor_policy: 'token',
        scope: {
          kind: 'issue_subtree',
          project_uid: '01HZNQ7VFPK1XGD8R5MABCD4EX',
          root_issue_uid: '01HZNQ7VFPK1XGD8R5MABCD5YZ',
        },
        expires_at: '2000-01-01T00:00:00Z',
        allowed_actions: ['issue.read'],
        close_requires_evidence: true,
      },
    },
    {
      label: 'partial scope capabilities',
      capabilities: {
        writable: true,
        updates: 'poll',
        actor_policy: 'token',
        scope: {
          kind: 'issue_subtree',
          project_uid: '01HZNQ7VFPK1XGD8R5MABCD4EX',
          root_issue_uid: '01HZNQ7VFPK1XGD8R5MABCD5YZ',
        },
        expires_at: '2099-01-01T00:00:00Z',
      },
    },
  ])('rejects $label before accepting browser authority', async ({ capabilities }) => {
    const generatedRequest = vi.fn(async () => ({
      status: 200 as const,
      headers: new Headers({ ETag: '"snapshot"' }),
      data: {
        contract_version: '2',
        cursor: 9,
        capabilities,
        catalog: [],
        collection: [],
        collection_links: [],
        origin: 'https://daemon.example',
        origin_stable: true,
      } as unknown as UISnapshot,
    }))
    const request = createUISnapshotRequest(generatedRequest)

    await expect(
      request(
        {
          view: 'all-open',
          statuses: [],
          owners: [],
          labels: [],
          relationships: [],
          includeGraph: false,
          includeHistory: false,
        },
        { signal: new AbortController().signal, full: false },
      ),
    ).rejects.toThrow('Invalid issue-scoped authority')
  })
})

function snapshot(value: string, cursor: number): TestSnapshot {
  return { cursor, value, capabilities: { writable: true, updates: 'sse' } }
}

function deferredSnapshot() {
  let resolve!: (value: { status: 200; etag: string; snapshot: TestSnapshot }) => void
  const promise = new Promise<{
    status: 200
    etag: string
    snapshot: TestSnapshot
  }>((done) => {
    resolve = done
  })
  return {
    promise,
    resolve,
    intent: undefined as TestIntent | undefined,
    signal: undefined as AbortSignal | undefined,
  }
}
