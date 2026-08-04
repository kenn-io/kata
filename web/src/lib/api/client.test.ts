import { describe, expect, it, vi } from 'vitest'

import { createCredentialedFetch } from './client'

describe('Kata browser API credentials', () => {
  it('attaches the tab session to reads and adds CSRF only to mutations', async () => {
    const calls: Array<[RequestInfo | URL, RequestInit | undefined]> = []
    const upstream = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push([input, init])
      return new Response('{}', { status: 200 })
    })
    const fetcher = createCredentialedFetch(
      () => ({ session: 'tab-session', csrf: 'tab-csrf' }),
      upstream as typeof fetch,
    )

    await fetcher('/api/v1/ui/snapshot', { method: 'GET' })
    await fetcher('/api/v1/projects/1/issues', { method: 'POST', body: '{}' })

    const read = calls[0]?.[1]
    const mutation = calls[1]?.[1]
    expect(read?.credentials).toBe('same-origin')
    expect(read?.redirect).toBe('error')
    expect(new Headers(read?.headers).get('X-Kata-Web-Session')).toBe('tab-session')
    expect(new Headers(read?.headers).has('X-Kata-CSRF')).toBe(false)
    expect(mutation?.credentials).toBe('same-origin')
    expect(mutation?.redirect).toBe('error')
    expect(new Headers(mutation?.headers).get('X-Kata-Web-Session')).toBe('tab-session')
    expect(new Headers(mutation?.headers).get('X-Kata-CSRF')).toBe('tab-csrf')
  })
})
