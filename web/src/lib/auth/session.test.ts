import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  authenticationModeStorageKey,
  consumeLaunchFragment,
  exchangeLoginToken,
  loadSessionCredentials,
  openLocalSession,
  openTrustedProxySession,
  selectAuthenticationMode,
} from './session'

describe('browser session bootstrap', () => {
  beforeEach(() => {
    sessionStorage.clear()
    history.replaceState(null, '', '/kata')
  })

  it('bootstraps a direct local tab without a launch fragment', async () => {
    const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      expect(String(input)).toBe('/api/v1/ui/session/local')
      expect(init?.credentials).toBe('same-origin')
      expect(init?.redirect).toBe('error')
      expect(init?.body).toBe(JSON.stringify({ return_path: '/kata' }))
      return new Response(
        JSON.stringify({
          session: 'local-session',
          csrf: 'local-csrf',
          return_path: '/kata',
          writable: true,
          updates: 'sse',
          actor_policy: 'request',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      )
    })

    const result = await openLocalSession('/kata', fetcher, sessionStorage)

    expect(result?.kind).toBe('authenticated')
    expect(loadSessionCredentials(sessionStorage)).toEqual({
      session: 'local-session',
      csrf: 'local-csrf',
    })
  })

  it('leaves unavailable direct local bootstrap unauthenticated', async () => {
    const result = await openLocalSession(
      '/kata',
      vi.fn(async () => new Response('', { status: 404 })),
      sessionStorage,
    )

    expect(result).toBeUndefined()
    expect(loadSessionCredentials(sessionStorage)).toBeUndefined()
  })

  it('exchanges a trusted proxy principal for a browser session', async () => {
    const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      expect(String(input)).toBe('/api/v1/ui/session/proxy')
      expect(init?.credentials).toBe('same-origin')
      expect(init?.redirect).toBe('error')
      return new Response(
        JSON.stringify({
          session: 'proxy-session',
          csrf: 'proxy-csrf',
          return_path: '/kata',
          writable: true,
          updates: 'sse',
          actor_policy: 'identity',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      )
    })

    const result = await openTrustedProxySession('/kata', fetcher, sessionStorage)

    expect(result.kind).toBe('authenticated')
    expect(result.capabilities.actorPolicy).toBe('identity')
    expect(loadSessionCredentials(sessionStorage)?.session).toBe('proxy-session')
  })

  it('never persists a bearer used for interactive login', async () => {
    const fetcher = vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
      expect(init?.redirect).toBe('error')
      return new Response(
        JSON.stringify({
          session: 'login-session',
          csrf: 'login-csrf',
          return_path: '/kata?scope=01HZNQ7VFPK1XGD8R5MABCD4EX&label=ready',
          writable: true,
          updates: 'sse',
          actor_policy: 'identity',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      )
    })

    await exchangeLoginToken(
      'direct-bearer-value',
      '/kata?scope=01HZNQ7VFPK1XGD8R5MABCD4EX&label=ready',
      fetcher,
      sessionStorage,
    )

    const persisted = JSON.stringify(sessionStorage)
    expect(persisted).toContain('login-session')
    expect(persisted).not.toContain('direct-bearer-value')
    expect(fetcher).toHaveBeenCalledTimes(1)
  })

  it('preserves a login return path without putting credentials in the URL', async () => {
    history.replaceState(
      null,
      '',
      '/kata#login=1&return_path=%2Fkata%3Fscope%3D01HZNQ7VFPK1XGD8R5MABCD4EX%26owner%3Duser-a',
    )

    const result = consumeLaunchFragment(window.location, history.replaceState.bind(history))

    expect(result).toEqual({
      kind: 'login',
      returnPath: '/kata?scope=01HZNQ7VFPK1XGD8R5MABCD4EX&owner=user-a',
    })
    expect(window.location.hash).toBe('')
  })

  it('keeps the explicitly selected authentication mechanism tab-local', () => {
    expect(selectAuthenticationMode({ kind: 'login', returnPath: '/kata' }, sessionStorage)).toBe(
      'login',
    )
    expect(sessionStorage.getItem(authenticationModeStorageKey)).toBe('login')
    expect(selectAuthenticationMode({ kind: 'none', returnPath: '/kata' }, sessionStorage)).toBe(
      'login',
    )
  })
})
