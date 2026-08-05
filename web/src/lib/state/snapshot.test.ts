import { describe, expect, it, vi } from 'vitest'

import { SnapshotController, type SnapshotRequest } from './snapshot'

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
