import { describe, expect, it } from 'vitest'

import { retryPortBind } from './port-bind-retry'

describe('retryPortBind', () => {
  it('allocates a fresh port after the daemon loses the bind race', async () => {
    const allocated: number[] = []
    const started: number[] = []
    let nextPort = 41000

    const result = await retryPortBind(
      async () => {
        const port = nextPort++
        allocated.push(port)
        return port
      },
      async (port) => {
        started.push(port)
        if (started.length === 1) {
          throw new Error(`listen tcp 127.0.0.1:${port}: bind: address already in use`)
        }
        return { port }
      },
    )

    expect(allocated).toEqual([41000, 41001])
    expect(started).toEqual([41000, 41001])
    expect(result).toEqual({ port: 41001 })
  })

  it('does not retry unrelated daemon startup errors', async () => {
    const failure = new Error('database initialization failed')
    let allocations = 0

    await expect(
      retryPortBind(
        async () => {
          allocations += 1
          return 41000
        },
        async () => {
          throw failure
        },
      ),
    ).rejects.toBe(failure)

    expect(allocations).toBe(1)
  })

  it('stops after four consecutive bind collisions', async () => {
    const failure = new Error('listen tcp 127.0.0.1:41000: bind: address already in use')
    let allocations = 0

    await expect(
      retryPortBind(
        async () => {
          allocations += 1
          return 41000 + allocations
        },
        async () => {
          throw failure
        },
      ),
    ).rejects.toBe(failure)

    expect(allocations).toBe(4)
  })
})
