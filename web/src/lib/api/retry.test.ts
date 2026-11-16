import { afterEach, describe, expect, it, vi } from 'vitest'

import { retryTransientRead } from './retry'

afterEach(() => vi.useRealTimers())

describe('retryTransientRead', () => {
  it('retries transient responses with bounded doubling delays', async () => {
    vi.useFakeTimers()
    const read = vi.fn(async () => new Response('', { status: 503 }))
    const result = retryTransientRead(read, new AbortController().signal, async () => undefined)
    await vi.advanceTimersByTimeAsync(1000)
    expect(read).toHaveBeenCalledTimes(2)
    await vi.advanceTimersByTimeAsync(2000)
    expect(read).toHaveBeenCalledTimes(3)
    await vi.advanceTimersByTimeAsync(4000)
    expect((await result).response.status).toBe(503)
    expect(read).toHaveBeenCalledTimes(4)
  })

  it.each([408, 429, 500])('retries status %i', async (status) => {
    vi.useFakeTimers()
    const read = vi
      .fn()
      .mockResolvedValueOnce(new Response('', { status }))
      .mockResolvedValueOnce(new Response('', { status: 200 }))
    const result = retryTransientRead(read, new AbortController().signal, async () => undefined)
    await vi.advanceTimersByTimeAsync(1000)
    expect((await result).response.status).toBe(200)
    expect(read).toHaveBeenCalledTimes(2)
  })

  it.each([401, 403, 422])('returns status %i without retrying', async (status) => {
    const read = vi.fn(async () => new Response('', { status }))
    expect(
      (await retryTransientRead(read, new AbortController().signal, async () => undefined)).response
        .status,
    ).toBe(status)
    expect(read).toHaveBeenCalledTimes(1)
  })

  it('stops when aborted during a wait', async () => {
    vi.useFakeTimers()
    const abort = new AbortController()
    const read = vi.fn(async () => new Response('', { status: 503 }))
    const result = retryTransientRead(read, abort.signal, async () => undefined)
    const rejection = expect(result).rejects.toMatchObject({ name: 'AbortError' })
    await vi.advanceTimersByTimeAsync(0)
    abort.abort()
    await rejection
    await vi.advanceTimersByTimeAsync(7000)
    expect(read).toHaveBeenCalledTimes(1)
  })
})

describe('Retry-After', () => {
  it.each([
    ['date', 'Thu, 01 Jan 1970 00:00:10 GMT', 10000],
    ['invalid', 'invalid', 1000],
  ])('uses %s hints', async (_case, hint, delay) => {
    vi.useFakeTimers()
    vi.setSystemTime(0)
    const read = vi
      .fn()
      .mockResolvedValueOnce(new Response('', { status: 503, headers: { 'Retry-After': hint } }))
      .mockResolvedValueOnce(new Response('', { status: 200 }))
    const result = retryTransientRead(read, new AbortController().signal, async () => undefined)
    await vi.advanceTimersByTimeAsync(delay - 1)
    expect(read).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1)
    expect((await result).response.status).toBe(200)
  })
  it('returns immediately when the delay exceeds the wait budget', async () => {
    const read = vi.fn(
      async () => new Response('', { status: 429, headers: { 'Retry-After': '120' } }),
    )
    expect(
      (await retryTransientRead(read, new AbortController().signal, async () => undefined)).response
        .status,
    ).toBe(429)
    expect(read).toHaveBeenCalledTimes(1)
  })
})
