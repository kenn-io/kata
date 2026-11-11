import { afterEach, describe, expect, it, vi } from 'vitest'

import { retryTransientRead } from './retry'

afterEach(() => vi.useRealTimers())

describe('retryTransientRead', () => {
  it('retries transient responses with bounded doubling delays', async () => {
    vi.useFakeTimers()
    const read = vi.fn().mockResolvedValue({ status: 503 })
    const result = retryTransientRead(read, new AbortController().signal)
    await vi.advanceTimersByTimeAsync(1000)
    expect(read).toHaveBeenCalledTimes(2)
    await vi.advanceTimersByTimeAsync(2000)
    expect(read).toHaveBeenCalledTimes(3)
    await vi.advanceTimersByTimeAsync(4000)
    expect(await result).toEqual({ status: 503 })
    expect(read).toHaveBeenCalledTimes(4)
  })

  it.each([408, 429, 500])('retries status %i', async (status) => {
    vi.useFakeTimers()
    const read = vi.fn().mockResolvedValueOnce({ status }).mockResolvedValueOnce({ status: 200 })
    const result = retryTransientRead(read, new AbortController().signal)
    await vi.advanceTimersByTimeAsync(1000)
    expect(await result).toEqual({ status: 200 })
    expect(read).toHaveBeenCalledTimes(2)
  })

  it.each([401, 403, 422])('returns status %i without retrying', async (status) => {
    const read = vi.fn().mockResolvedValue({ status })
    expect(await retryTransientRead(read, new AbortController().signal)).toEqual({ status })
    expect(read).toHaveBeenCalledTimes(1)
  })

  it('stops when aborted during a wait', async () => {
    vi.useFakeTimers()
    const abort = new AbortController()
    const read = vi.fn().mockResolvedValue({ status: 503 })
    const result = retryTransientRead(read, abort.signal)
    const rejection = expect(result).rejects.toMatchObject({ name: 'AbortError' })
    await vi.advanceTimersByTimeAsync(0)
    abort.abort()
    await rejection
    await vi.advanceTimersByTimeAsync(7000)
    expect(read).toHaveBeenCalledTimes(1)
  })
})
