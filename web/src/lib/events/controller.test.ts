import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  EventStreamController,
  InvalidationController,
  ReconnectBackoff,
  RefreshScheduler,
} from './controller'

describe('event invalidation control', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('coalesces event bursts and turns reset frames into full refreshes', async () => {
    const refresh = vi.fn(async (full: boolean) => {
      void full
      return true
    })
    const controller = new InvalidationController(refresh)

    controller.frame({ id: '1', event: 'issue.updated', data: '{}' })
    controller.frame({ id: '2', event: 'issue.labeled', data: '{}' })
    controller.frame({ id: '3', event: 'sync.reset_required', data: '{}' })
    expect(refresh).not.toHaveBeenCalled()
    vi.runOnlyPendingTimers()
    await Promise.resolve()

    expect(refresh).toHaveBeenCalledTimes(1)
    expect(refresh).toHaveBeenCalledWith(true)
  })

  it('keeps reset refreshes latched until an unconditional refresh succeeds', async () => {
    const refresh = vi
      .fn<(full: boolean) => Promise<boolean>>()
      .mockResolvedValueOnce(false)
      .mockResolvedValueOnce(true)
      .mockResolvedValueOnce(true)
    const controller = new InvalidationController(refresh)

    controller.frame({ id: '3', event: 'sync.reset_required', data: '{}' })
    await vi.advanceTimersByTimeAsync(0)
    expect(refresh).toHaveBeenNthCalledWith(1, true)

    await vi.advanceTimersByTimeAsync(999)
    expect(refresh).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(refresh).toHaveBeenNthCalledWith(2, true)

    controller.refresh()
    await vi.advanceTimersByTimeAsync(0)
    expect(refresh).toHaveBeenNthCalledWith(3, false)
  })

  it('keeps a reset latched across an authentication pause and reauthentication', async () => {
    const refresh = vi
      .fn<(full: boolean) => Promise<boolean>>()
      .mockImplementationOnce(async () => {
        controller.pause()
        return false
      })
      .mockResolvedValueOnce(true)
      .mockResolvedValueOnce(true)
    const controller = new InvalidationController(refresh)

    controller.frame({ id: '3', event: 'sync.reset_required', data: '{}' })
    await vi.advanceTimersByTimeAsync(0)
    expect(refresh).toHaveBeenNthCalledWith(1, true)

    await controller.resume()
    expect(refresh).toHaveBeenNthCalledWith(2, true)

    controller.refresh()
    await vi.advanceTimersByTimeAsync(0)
    expect(refresh).toHaveBeenNthCalledWith(3, false)
  })

  it('backs off 1/2/4 seconds through a 30-second cap and resets only after work', () => {
    const backoff = new ReconnectBackoff()
    expect([
      backoff.afterDisconnect(false),
      backoff.afterDisconnect(false),
      backoff.afterDisconnect(false),
      backoff.afterDisconnect(false),
      backoff.afterDisconnect(false),
      backoff.afterDisconnect(false),
    ]).toEqual([1000, 2000, 4000, 8000, 16000, 30000])
    expect(backoff.afterDisconnect(false)).toBe(30000)
    expect(backoff.afterDisconnect(true)).toBe(1000)
    expect(backoff.afterDisconnect(false)).toBe(1000)
  })

  it('reports a productive stream reconnect without exposing failure details', async () => {
    const states: string[] = []
    const controller = new EventStreamController({
      connect: async function* () {
        yield { id: '13', event: 'issue.updated', data: '{}' }
      },
      onFrame: vi.fn(),
      onState: (state) => states.push(state),
      wait: async () => {
        throw new DOMException('Stopped', 'AbortError')
      },
    })

    controller.start(12)
    await vi.waitFor(() => expect(states).toEqual(['connecting', 'online', 'reconnecting']))
    controller.stop()
  })

  it('stops reconnecting when the browser session is no longer authorized', async () => {
    const onAuthenticationRequired = vi.fn()
    const wait = vi.fn(async () => undefined)
    const options = {
      connect: async function* () {
        yield* []
        const error = new Error('Event stream unavailable')
        error.name = 'AuthenticationRequiredError'
        throw error
      },
      onFrame: vi.fn(),
      onAuthenticationRequired,
      wait,
    }
    const controller = new EventStreamController(options)

    controller.start(12)
    await vi.waitFor(() => expect(onAuthenticationRequired).toHaveBeenCalledOnce())

    expect(wait).not.toHaveBeenCalled()
    controller.stop()
  })
})

describe('snapshot refresh scheduling', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('recomputes intent before polls and never opens SSE in polling mode', async () => {
    let intent = 'first'
    const seen: string[] = []
    const openEvents = vi.fn()
    const scheduler = new RefreshScheduler({
      refresh: async () => {
        seen.push(intent)
      },
      openEvents,
      now: () => new Date('2026-08-01T12:00:00'),
    })
    scheduler.start('poll')

    intent = 'second'
    vi.advanceTimersByTime(15_000)
    await Promise.resolve()
    expect(seen).toEqual(['second'])
    expect(openEvents).not.toHaveBeenCalled()
    scheduler.stop()
  })

  it('refreshes on visibility, focus, timezone change, and local midnight', async () => {
    const refresh = vi.fn(async () => undefined)
    let zone = 'America/Chicago'
    const scheduler = new RefreshScheduler({
      refresh,
      openEvents: vi.fn(),
      now: () => new Date('2026-08-01T23:59:59.500'),
      timeZone: () => zone,
    })
    scheduler.start('poll')

    scheduler.visibilityChanged(false)
    vi.advanceTimersByTime(15_000)
    await Promise.resolve()
    expect(refresh).not.toHaveBeenCalled()
    scheduler.visibilityChanged(true)
    scheduler.focused()
    zone = 'America/New_York'
    scheduler.environmentChanged()
    vi.advanceTimersByTime(501)
    await Promise.resolve()

    expect(refresh).toHaveBeenCalledTimes(4)
    scheduler.stop()
  })
})
