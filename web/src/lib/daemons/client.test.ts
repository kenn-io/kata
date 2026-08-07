import { describe, expect, test, vi } from 'vitest'

import { createDaemonFetch, fetchWebDaemons } from './client'

describe('web daemon transport', () => {
  test('pins ordinary API requests to the selected daemon proxy', async () => {
    const upstream = vi.fn<typeof fetch>(async () => new Response('{}'))
    const fetcher = createDaemonFetch(() => 'example-remote', upstream)

    await fetcher('/api/v1/ui/snapshot?view=all-open', {
      headers: { 'If-None-Match': '"snapshot-1"' },
    })

    const [input, init] = upstream.mock.calls[0]!
    const request = new Request(input, init)
    expect(new URL(request.url).pathname).toBe('/api/v1/ui/proxy/api/v1/ui/snapshot')
    expect(new URL(request.url).search).toBe('?view=all-open')
    expect(request.headers.get('X-Kata-Web-Daemon')).toBe('example-remote')
    expect(request.headers.get('If-None-Match')).toBe('"snapshot-1"')
  })

  test('forwards mutation bodies through the selected daemon proxy', async () => {
    const upstream = vi.fn<typeof fetch>(async () => new Response('{}'))
    const fetcher = createDaemonFetch(() => 'example-local', upstream)

    await fetcher('/api/v1/projects', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: 'example-project' }),
    })

    const [input, init] = upstream.mock.calls[0]!
    const request = new Request(input, init)
    expect(request.method).toBe('POST')
    expect(request.headers.get('X-Kata-Web-Daemon')).toBe('example-local')
    await expect(request.json()).resolves.toEqual({ name: 'example-project' })
  })

  test('leaves gateway session and roster requests on the browser origin', async () => {
    const upstream = vi.fn<typeof fetch>(async () => new Response('{}'))
    const fetcher = createDaemonFetch(() => 'example-remote', upstream)

    await fetcher('/api/v1/ui/daemons')
    await fetcher('/api/v1/ui/session/local', { method: 'POST' })

    const paths = upstream.mock.calls.map(
      ([request]) =>
        new URL(request instanceof Request ? request.url : String(request), window.location.origin)
          .pathname,
    )
    expect(paths).toEqual(['/api/v1/ui/daemons', '/api/v1/ui/session/local'])
  })

  test('accepts only a sanitized daemon roster', async () => {
    const fetcher = vi.fn(async () =>
      Response.json({
        daemons: [
          {
            id: 'example-local',
            url: '',
            default: true,
            auth: 'none',
            health: 'connected',
          },
          {
            id: 'example-remote',
            url: 'https://daemon.example',
            default: false,
            auth: 'token',
            health: 'auth_required',
            hint: 'check credentials',
          },
          {
            id: 'example-legacy',
            url: 'https://daemon.example',
            default: false,
            auth: 'none',
            health: 'upgrade_required',
            hint: 'daemon does not support the Kata web UI',
          },
        ],
      }),
    )

    await expect(fetchWebDaemons(fetcher)).resolves.toEqual([
      expect.objectContaining({ id: 'example-local', default: true, health: 'connected' }),
      expect.objectContaining({ id: 'example-remote', auth: 'token', health: 'auth_required' }),
      expect.objectContaining({ id: 'example-legacy', health: 'upgrade_required' }),
    ])
  })
})
