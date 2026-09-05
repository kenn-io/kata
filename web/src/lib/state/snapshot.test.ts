import { describe, expect, it, vi } from 'vitest'

import { parseRoute } from '../router'
import {
  createUISnapshotRequest,
  SnapshotController,
  snapshotIntentForRoute,
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
    expect(controller.state.error).toBe('Snapshot unavailable')
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
