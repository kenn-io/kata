import { describe, expect, it, vi } from 'vitest'

import {
  createCredentialedFetch,
  orvalFetch,
  responseTriggeredAuthenticationTransition,
} from './client'

describe('Kata browser API credentials', () => {
  it('returns the JSON body from generated API requests', async () => {
    const upstream = vi.fn(async () =>
      Response.json({ issues: [{ title: 'Generated client request' }] }),
    )

    const result = await orvalFetch<{
      data: { issues: Array<{ title: string }> }
      status: number
      headers: Headers
    }>('/api/v1/ui/references', { method: 'GET' }, upstream as typeof fetch)

    expect(result.data.issues[0]?.title).toBe('Generated client request')
  })

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

  it('fences browser authority when a credentialed request returns 401', async () => {
    const onAuthenticationRequired = vi.fn(() => true)
    const fetcher = createCredentialedFetch(
      () => ({ session: 'expired-session', csrf: 'expired-csrf' }),
      vi.fn(async () => new Response('', { status: 401 })) as typeof fetch,
      onAuthenticationRequired,
    )

    const response = await fetcher('/api/v1/ui/references')

    expect(response.status).toBe(401)
    expect(onAuthenticationRequired).toHaveBeenCalledTimes(1)
    expect(responseTriggeredAuthenticationTransition(response)).toBe(true)
  })

  it('ignores a delayed 401 from a superseded browser session', async () => {
    let credentials = { session: 'expired-session', csrf: 'expired-csrf' }
    let release!: (response: Response) => void
    const response = new Promise<Response>((resolve) => {
      release = resolve
    })
    const onAuthenticationRequired = vi.fn(() => true)
    const fetcher = createCredentialedFetch(
      () => credentials,
      vi.fn(async () => response) as typeof fetch,
      onAuthenticationRequired,
    )

    const pending = fetcher('/api/v1/ui/references')
    credentials = { session: 'renewed-session', csrf: 'renewed-csrf' }
    release(new Response('', { status: 401 }))

    const result = await pending
    expect(result.status).toBe(401)
    expect(onAuthenticationRequired).not.toHaveBeenCalled()
    expect(responseTriggeredAuthenticationTransition(result)).toBe(false)
  })
})
