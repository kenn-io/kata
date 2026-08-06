// @vitest-environment-options { "url": "http://127.0.0.2/kata" }

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import App from './App.svelte'
import { preferencesStorageKey } from './lib/state/preferences'

describe('App', () => {
  beforeEach(() => {
    sessionStorage.clear()
    localStorage.clear()
    document.documentElement.classList.remove('dark')
    history.replaceState(null, '', '/kata')
  })

  afterEach(() => {
    cleanup()
    document.documentElement.classList.remove('dark')
    vi.unstubAllGlobals()
  })

  it('renders the Kata application shell while loading', () => {
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
    )
    render(App)

    expect(screen.getByRole('main', { name: 'Kata' })).not.toBeNull()
    expect(screen.getByRole('status').textContent).toBe('Loading Kata…')
  })

  it('opens a direct local tab with a transparent browser session', async () => {
    const paths: string[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const target = new URL(
          input instanceof Request ? input.url : String(input),
          window.location.origin,
        )
        paths.push(target.pathname)
        if (target.pathname === '/api/v1/ui/session/local') {
          return new Response(
            JSON.stringify({
              session: 'local-session',
              csrf: 'local-csrf',
              return_path: '/kata',
              writable: true,
              updates: 'poll',
              actor_policy: 'request',
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } },
          )
        }
        if (target.pathname === '/api/v1/ui/daemons') return Response.json(daemonRoster())
        expect(
          input instanceof Request
            ? input.headers.get('X-Kata-Web-Session')
            : new Headers(init?.headers).get('X-Kata-Web-Session'),
        ).toBe('local-session')
        return new Response(JSON.stringify(snapshot()), {
          status: 200,
          headers: { 'Content-Type': 'application/json', ETag: '"snapshot-1"' },
        })
      }),
    )

    render(App)

    expect(await screen.findByRole('region', { name: 'Kata workspace' })).not.toBeNull()
    expect(paths.slice(0, 3)).toEqual([
      '/api/v1/ui/session/local',
      '/api/v1/ui/daemons',
      '/api/v1/ui/proxy/api/v1/ui/snapshot',
    ])
    expect(sessionStorage.getItem('kata.web.session.v1')).toContain('local-session')
  })

  it('honors an explicitly selected gateway daemon on first authority load', async () => {
    history.replaceState(null, '', '/kata#daemon=example-remote')
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
    )
    const selected: Array<string | null> = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request =
          input instanceof Request
            ? input
            : new Request(new URL(String(input), window.location.origin), init)
        const path = new URL(request.url).pathname
        if (path === '/api/v1/ui/daemons') {
          return Response.json({
            daemons: [
              ...daemonRoster().daemons,
              {
                id: 'example-remote',
                url: 'https://daemon.example',
                default: false,
                auth: 'token',
                health: 'connected',
              },
            ],
          })
        }
        if (path.endsWith('/api/v1/ui/references')) {
          return Response.json({ issues: [], labels: [], owners: [], projects: [] })
        }
        selected.push(request.headers.get('X-Kata-Web-Daemon'))
        return Response.json(snapshot(), { headers: { ETag: '"snapshot-1"' } })
      }),
    )

    render(App)

    expect(await screen.findByRole('region', { name: 'Kata workspace' })).not.toBeNull()
    expect(selected[0]).toBe('example-remote')
  })

  it('opens a trusted-proxy tab without presenting token login', async () => {
    const paths: string[] = []
    let authorized = false
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const target = new URL(String(input), window.location.origin)
        paths.push(target.pathname)
        if (target.pathname === '/api/v1/ui/session/local') {
          return new Response('', { status: 404 })
        }
        if (target.pathname === '/api/v1/ui/session/proxy') {
          authorized = true
          return Response.json({
            session: 'proxy-session',
            csrf: 'proxy-csrf',
            return_path: '/kata',
            writable: true,
            updates: 'poll',
            actor_policy: 'identity',
          })
        }
        if (!authorized) {
          return new Response('', {
            status: 401,
            headers: { 'X-Kata-Web-Authentication': 'proxy' },
          })
        }
        return Response.json(snapshot(), { headers: { ETag: '"snapshot-1"' } })
      }),
    )

    render(App)

    expect(await screen.findByRole('region', { name: 'Kata workspace' })).not.toBeNull()
    expect(paths).toContain('/api/v1/ui/session/proxy')
    expect(screen.queryByRole('heading', { name: 'Connect to Kata' })).toBeNull()
  })

  it('tells a local tab to use the CLI when transparent authorization is unavailable', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const target = new URL(String(input), window.location.origin)
        if (target.pathname === '/api/v1/ui/daemons') return Response.json(daemonRoster())
        if (target.pathname.endsWith('/api/v1/ui/references')) {
          return Response.json({ issues: [], labels: [], owners: [], projects: [] })
        }
        if (target.pathname === '/api/v1/ui/session/local') {
          return new Response('', { status: 404 })
        }
        return new Response('', {
          status: 401,
          headers: { 'X-Kata-Web-Authentication': 'loopback' },
        })
      }),
    )

    render(App)

    expect(await screen.findByText(/kata ui/)).not.toBeNull()
  })

  it('leaves loading and clears stale tab credentials when snapshot authority expires', async () => {
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'expired-session', csrf: 'expired-csrf' }),
    )
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response('', {
            status: 401,
            headers: { 'X-Kata-Web-Authentication': 'loopback' },
          }),
      ),
    )

    render(App)

    expect(await screen.findByRole('heading', { name: 'Launch Kata securely' })).not.toBeNull()
    expect(sessionStorage.getItem('kata.web.session.v1')).toBeNull()
    expect(screen.queryByText('Loading Kata…')).toBeNull()
  })

  it('renews an expired session on a server-advertised custom loopback origin', async () => {
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'expired-session', csrf: 'expired-csrf' }),
    )
    let snapshotReads = 0
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const target = new URL(String(input), window.location.origin)
        if (target.pathname === '/api/v1/ui/daemons') return Response.json(daemonRoster())
        if (target.pathname === '/api/v1/ui/session/local') {
          return new Response(
            JSON.stringify({
              session: 'renewed-session',
              csrf: 'renewed-csrf',
              return_path: '/kata',
              writable: true,
              updates: 'poll',
              actor_policy: 'request',
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } },
          )
        }
        if (target.pathname.endsWith('/api/v1/ui/references')) {
          return Response.json({ issues: [], labels: [], owners: [], projects: [] })
        }
        snapshotReads += 1
        if (snapshotReads === 1)
          return new Response('', {
            status: 401,
            headers: { 'X-Kata-Web-Authentication': 'loopback' },
          })
        return new Response(JSON.stringify(snapshot()), {
          status: 200,
          headers: { 'Content-Type': 'application/json', ETag: '"snapshot-1"' },
        })
      }),
    )

    render(App)

    expect(await screen.findByRole('region', { name: 'Kata workspace' })).not.toBeNull()
    await waitFor(() =>
      expect(sessionStorage.getItem('kata.web.session.v1')).toContain('renewed-session'),
    )
  })

  it('reloads the configured daemon roster after transparent reauthentication', async () => {
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'expired-session', csrf: 'expired-csrf' }),
    )
    let rosterReads = 0
    const snapshotRequests: Request[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request =
          input instanceof Request
            ? input
            : new Request(new URL(String(input), window.location.origin), init)
        const target = new URL(request.url)
        if (target.pathname === '/api/v1/ui/daemons') {
          rosterReads += 1
          if (rosterReads === 1) {
            return new Response('', {
              status: 401,
              headers: { 'X-Kata-Web-Authentication': 'loopback' },
            })
          }
          return Response.json({
            daemons: [
              { id: 'example-local', url: '', default: false, auth: 'none', health: 'connected' },
              {
                id: 'example-remote',
                url: 'https://daemon.example',
                default: true,
                auth: 'token',
                health: 'connected',
              },
            ],
          })
        }
        if (target.pathname === '/api/v1/ui/session/local') {
          return Response.json({
            session: 'renewed-session',
            csrf: 'renewed-csrf',
            return_path: '/kata',
            writable: true,
            updates: 'poll',
            actor_policy: 'request',
          })
        }
        if (target.pathname.endsWith('/api/v1/ui/references')) {
          return Response.json({ issues: [], labels: [], owners: [], projects: [] })
        }
        if (target.pathname.endsWith('/api/v1/ui/snapshot')) {
          snapshotRequests.push(request)
          return Response.json(snapshot(), { headers: { ETag: '"snapshot-1"' } })
        }
        throw new Error(`Unexpected request: ${request.method} ${target.pathname}`)
      }),
    )

    render(App)

    expect(await screen.findByRole('region', { name: 'Kata workspace' })).not.toBeNull()
    await waitFor(() => expect(rosterReads).toBe(2))
    await waitFor(() =>
      expect(snapshotRequests.at(-1)?.headers.get('X-Kata-Web-Daemon')).toBe('example-remote'),
    )
  })

  it('does not resume snapshot authority after the application unmounts', async () => {
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
    )
    let releaseRoster!: (response: Response) => void
    const roster = new Promise<Response>((resolve) => {
      releaseRoster = resolve
    })
    const paths: string[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const target = new URL(String(input), window.location.origin)
        paths.push(target.pathname)
        if (target.pathname === '/api/v1/ui/daemons') return roster
        return Response.json(snapshot(), { headers: { ETag: '"snapshot-1"' } })
      }),
    )

    render(App)
    await waitFor(() => expect(paths).toEqual(['/api/v1/ui/daemons']))
    cleanup()
    releaseRoster(Response.json(daemonRoster()))
    await new Promise((resolve) => setTimeout(resolve, 0))

    expect(paths).toEqual(['/api/v1/ui/daemons'])
  })

  it('recovers an initial snapshot failure through an in-app retry', async () => {
    history.replaceState(null, '', '/kata?view=all-open')
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
    )
    let snapshotReads = 0
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const target = new URL(String(input), window.location.origin)
        if (target.pathname !== '/api/v1/ui/snapshot') {
          return new Response('{}', {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        }
        snapshotReads += 1
        if (snapshotReads === 1) return new Response('', { status: 503 })
        return new Response(JSON.stringify(snapshot()), {
          status: 200,
          headers: { 'Content-Type': 'application/json', ETag: '"snapshot-1"' },
        })
      }),
    )

    render(App)

    const retry = await screen.findByRole('button', { name: 'Retry Kata snapshot' })
    expect(screen.getByRole('alert').textContent).toContain('Snapshot unavailable')
    await fireEvent.click(retry)

    expect(await screen.findByRole('button', { name: /Example issue/ })).not.toBeNull()
    expect(snapshotReads).toBe(2)
    expect(screen.queryByRole('button', { name: 'Retry Kata snapshot' })).toBeNull()
  })

  it('fences browser authority when project creation rejects the session', async () => {
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'revoked-session', csrf: 'revoked-csrf' }),
    )
    let revoked = false
    const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const method = input instanceof Request ? input.method : (init?.method ?? 'GET')
      const target = new URL(
        input instanceof Request ? input.url : String(input),
        window.location.origin,
      )
      if (method === 'POST' && target.pathname === '/api/v1/projects') {
        revoked = true
        return new Response('', {
          status: 401,
          headers: { 'X-Kata-Web-Authentication': 'loopback' },
        })
      }
      if (method === 'POST' && target.pathname === '/api/v1/ui/session/local') {
        return new Response('', { status: 404 })
      }
      if (revoked && target.pathname === '/api/v1/ui/snapshot') {
        return new Response('', {
          status: 401,
          headers: { 'X-Kata-Web-Authentication': 'loopback' },
        })
      }
      return new Response(JSON.stringify(snapshot()), {
        status: 200,
        headers: { 'Content-Type': 'application/json', ETag: '"snapshot-1"' },
      })
    })
    vi.stubGlobal('fetch', fetcher)

    render(App)
    await fireEvent.click(await screen.findByRole('button', { name: 'New project' }))
    const input = screen.getByLabelText('New project name')
    await fireEvent.input(input, { target: { value: 'example-workspace' } })
    await fireEvent.keyDown(input, { key: 'Enter' })

    expect(await screen.findByRole('heading', { name: 'Launch Kata securely' })).not.toBeNull()
    expect(sessionStorage.getItem('kata.web.session.v1')).toBeNull()
    expect(
      fetcher.mock.calls.some(
        ([value, options]) =>
          (value instanceof Request ? value.method : options?.method) === 'POST',
      ),
    ).toBe(true)
  })

  it('fences browser authority when a reference request rejects the session', async () => {
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'revoked-session', csrf: 'revoked-csrf' }),
    )
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const target = new URL(
          input instanceof Request ? input.url : String(input),
          window.location.origin,
        )
        if (target.pathname === '/api/v1/ui/references') {
          return new Response('', {
            status: 401,
            headers: { 'X-Kata-Web-Authentication': 'login' },
          })
        }
        return new Response(JSON.stringify(snapshot()), {
          status: 200,
          headers: { 'Content-Type': 'application/json', ETag: '"snapshot-1"' },
        })
      }),
    )

    render(App)

    expect(await screen.findByRole('form', { name: 'Log in to Kata' })).not.toBeNull()
    expect(sessionStorage.getItem('kata.web.session.v1')).toBeNull()
    expect(
      (screen.getByRole('button', { name: 'New project' }) as HTMLButtonElement).disabled,
    ).toBe(true)
  })

  it('shows configured-origin login without probing authority or losing the requested route', () => {
    const fetcher = vi.fn()
    vi.stubGlobal('fetch', fetcher)
    history.replaceState(
      null,
      '',
      '/kata#login=1&return_path=%2Fkata%3Fscope%3D01HZNQ7VFPK1XGD8R5MABCD4EX%26label%3Dready',
    )
    render(App)

    expect(screen.getByRole('form', { name: 'Log in to Kata' })).not.toBeNull()
    expect(fetcher).not.toHaveBeenCalled()
    expect(window.location.hash).toBe('')
  })

  it('recovers a short issue route through complete issue authority', async () => {
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
    )
    history.replaceState(null, '', '/kata?issue=abc4')
    render(App)

    const search = screen.getAllByRole('button', { name: 'Search for abc4' })
    expect(search).toHaveLength(1)
    expect(window.location.pathname + window.location.search).toBe('/kata?issue=abc4')
    await fireEvent.click(search[0]!)
    expect(window.location.pathname).toBe('/kata')
    expect(new URLSearchParams(window.location.search).getAll('status')).toEqual(['all'])
    expect(new URLSearchParams(window.location.search).get('text')).toBe('abc4')
  })

  it('renders the ported collection and navigates through canonical routes after authority loads', async () => {
    history.replaceState(null, '', '/kata?view=all-open')
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
    )
    let snapshotReads = 0
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const target = new URL(
          input instanceof Request ? input.url : String(input),
          window.location.origin,
        )
        if (target.pathname === '/api/v1/ui/daemons') return Response.json(daemonRoster())
        snapshotReads += 1
        if (snapshotReads > 1) return new Promise<Response>(() => {})
        return new Response(JSON.stringify(snapshot()), {
          status: 200,
          headers: { 'Content-Type': 'application/json', ETag: '"snapshot-1"' },
        })
      }),
    )

    render(App)

    expect(await screen.findByRole('button', { name: /Example issue/ })).not.toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: 'Today' }))
    await waitFor(() => expect(window.location.search).toBe('?view=today'))
    expect(screen.getByRole('heading', { name: 'All Open' })).not.toBeNull()
  })

  it('switches configured daemons in place and restores each daemon route', async () => {
    history.replaceState(null, '', '/kata?view=all-open')
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
    )
    const local = snapshot()
    const remote = structuredClone(local)
    remote.collection[0]!.title = 'Remote issue'
    const snapshotRequests: Request[] = []
    const referenceRequests: Request[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request =
          input instanceof Request
            ? input
            : new Request(new URL(String(input), window.location.origin), init)
        const path = new URL(request.url).pathname
        if (path === '/api/v1/ui/daemons') {
          return Response.json({
            daemons: [
              ...daemonRoster().daemons,
              {
                id: 'example-remote',
                url: 'https://daemon.example',
                default: false,
                auth: 'token',
                health: 'connected',
              },
            ],
          })
        }
        if (path.endsWith('/api/v1/ui/references')) {
          referenceRequests.push(request)
          return Response.json({ issues: [], labels: [], owners: [], projects: [] })
        }
        if (path.endsWith('/api/v1/ui/snapshot')) {
          snapshotRequests.push(request)
          const daemon = request.headers.get('X-Kata-Web-Daemon')
          return Response.json(daemon === 'example-remote' ? remote : local, {
            headers: { ETag: `"${daemon ?? 'default'}-${snapshotRequests.length}"` },
          })
        }
        throw new Error(`Unexpected request: ${request.method} ${path}`)
      }),
    )

    render(App)
    expect(await screen.findByRole('button', { name: /Example issue/ })).not.toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: 'Today' }))
    await waitFor(() => expect(window.location.search).toBe('?view=today'))
    await fireEvent.click(screen.getByRole('button', { name: 'Switch Kata daemon: example-local' }))
    await fireEvent.click(screen.getByRole('menuitemradio', { name: /example-remote/ }))
    expect(await screen.findByRole('button', { name: /Remote issue/ })).not.toBeNull()
    expect(window.location.search).toBe('?view=all-open')
    expect(
      referenceRequests.find(
        (request) => request.headers.get('X-Kata-Web-Daemon') === 'example-local',
      )?.signal.aborted,
    ).toBe(true)

    await fireEvent.click(
      screen.getByRole('button', { name: 'Switch Kata daemon: example-remote' }),
    )
    await fireEvent.click(screen.getByRole('menuitemradio', { name: /example-local/ }))
    await waitFor(() => expect(window.location.search).toBe('?view=today'))
    expect(snapshotRequests.map((request) => request.headers.get('X-Kata-Web-Daemon'))).toEqual(
      expect.arrayContaining(['example-local', 'example-remote']),
    )
  })

  it('keeps the requested daemon selected across transparent authentication recovery', async () => {
    history.replaceState(null, '', '/kata?view=all-open')
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'expired-session', csrf: 'expired-csrf' }),
    )
    const local = snapshot()
    const remote = structuredClone(local)
    remote.collection[0]!.title = 'Remote issue'
    let remoteRejected = false
    const snapshotDaemons: Array<string | null> = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request =
          input instanceof Request
            ? input
            : new Request(new URL(String(input), window.location.origin), init)
        const path = new URL(request.url).pathname
        if (path === '/api/v1/ui/daemons') {
          return Response.json({
            daemons: [
              ...daemonRoster().daemons,
              {
                id: 'example-remote',
                url: 'https://daemon.example',
                default: false,
                auth: 'token',
                health: 'connected',
              },
            ],
          })
        }
        if (path === '/api/v1/ui/session/local') {
          return Response.json({
            session: 'renewed-session',
            csrf: 'renewed-csrf',
            return_path: '/kata?view=all-open',
            writable: true,
            updates: 'poll',
            actor_policy: 'identity',
          })
        }
        if (path.endsWith('/api/v1/ui/references')) {
          return Response.json({ issues: [], labels: [], owners: [], projects: [] })
        }
        if (path.endsWith('/api/v1/ui/snapshot')) {
          const daemon = request.headers.get('X-Kata-Web-Daemon')
          snapshotDaemons.push(daemon)
          if (daemon === 'example-remote' && !remoteRejected) {
            remoteRejected = true
            return new Response('', {
              status: 401,
              headers: { 'X-Kata-Web-Authentication': 'loopback' },
            })
          }
          return Response.json(daemon === 'example-remote' ? remote : local, {
            headers: { ETag: `"${daemon ?? 'default'}-${snapshotDaemons.length}"` },
          })
        }
        throw new Error(`Unexpected request: ${request.method} ${path}`)
      }),
    )

    render(App)
    expect(await screen.findByRole('button', { name: /Example issue/ })).not.toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: 'Switch Kata daemon: example-local' }))
    await fireEvent.click(screen.getByRole('menuitemradio', { name: /example-remote/ }))

    expect(await screen.findByRole('button', { name: /Remote issue/ })).not.toBeNull()
    expect(
      screen.getByRole('button', { name: 'Switch Kata daemon: example-remote' }),
    ).not.toBeNull()
    expect(snapshotDaemons.at(-1)).toBe('example-remote')
  })

  it('offers daemon switching when the default snapshot is unavailable', async () => {
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
    )
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request =
          input instanceof Request
            ? input
            : new Request(new URL(String(input), window.location.origin), init)
        const path = new URL(request.url).pathname
        if (path === '/api/v1/ui/daemons') {
          return Response.json({
            daemons: [
              {
                id: 'example-local',
                url: '',
                default: true,
                auth: 'none',
                health: 'down',
              },
              {
                id: 'example-remote',
                url: 'https://daemon.example',
                default: false,
                auth: 'token',
                health: 'connected',
              },
            ],
          })
        }
        if (path.endsWith('/api/v1/ui/references')) {
          return Response.json({ issues: [], labels: [], owners: [], projects: [] })
        }
        if (path.endsWith('/api/v1/ui/snapshot')) {
          if (request.headers.get('X-Kata-Web-Daemon') === 'example-remote') {
            return Response.json(snapshot(), { headers: { ETag: '"remote-snapshot"' } })
          }
          return new Response('{"error":{"code":"daemon_unreachable"}}', { status: 502 })
        }
        throw new Error(`Unexpected request: ${request.method} ${path}`)
      }),
    )

    render(App)

    expect((await screen.findByRole('alert')).textContent).toContain('Snapshot unavailable')
    await fireEvent.click(screen.getByRole('button', { name: 'Switch Kata daemon: example-local' }))
    await fireEvent.click(screen.getByRole('menuitemradio', { name: /example-remote/ }))
    expect(await screen.findByRole('region', { name: 'Kata workspace' })).not.toBeNull()
  })

  it('designates an Inbox project from a fresh catalog', async () => {
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
    )
    const accepted = snapshot()
    const requests: Request[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request =
          input instanceof Request
            ? input
            : new Request(new URL(String(input), window.location.origin), init)
        requests.push(request)
        if (request.method === 'POST' && request.url.endsWith('/api/v1/projects/7/metadata')) {
          accepted.catalog[0]!.project.metadata.role = 'inbox'
          return new Response(
            JSON.stringify({
              changed: true,
              project: accepted.catalog[0]!.project,
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } },
          )
        }
        return new Response(JSON.stringify(accepted), {
          status: 200,
          headers: { 'Content-Type': 'application/json', ETag: `"snapshot-${requests.length}"` },
        })
      }),
    )

    render(App)

    await fireEvent.click(
      await screen.findByRole('button', { name: 'Inbox project: Choose a project' }),
    )
    await fireEvent.mouseDown(screen.getByRole('option', { name: /example-project/ }))

    await waitFor(() => {
      const mutation = requests.find(
        (request) =>
          request.method === 'POST' && request.url.endsWith('/api/v1/projects/7/metadata'),
      )
      expect(mutation).toBeDefined()
      expect((screen.getByRole('button', { name: 'New task' }) as HTMLButtonElement).disabled).toBe(
        false,
      )
    })
    const mutation = requests.find(
      (request) => request.method === 'POST' && request.url.endsWith('/api/v1/projects/7/metadata'),
    )!
    expect(await mutation.json()).toEqual({ actor: 'kata-web', patch: { role: 'inbox' } })
  })

  it('reassigns Inbox through one atomic designation request', async () => {
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
    )
    const accepted = snapshot()
    accepted.catalog[0]!.project.metadata.role = 'inbox'
    accepted.catalog.push({
      project: {
        id: 8,
        uid: '01J00000000000000000000008',
        name: 'example-workspace',
        metadata: { area: 'Work' },
        revision: 1,
        created_at: '2026-08-01T09:00:00.000Z',
      },
      stats: { Open: 0, Closed: 0, LastEventAt: '2026-08-01T12:00:00.000Z' },
    })
    const patches: Array<{ projectID: number; body: unknown }> = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request =
          input instanceof Request
            ? input
            : new Request(new URL(String(input), window.location.origin), init)
        const match = request.url.match(/\/api\/v1\/projects\/(\d+)\/metadata$/)
        if (request.method === 'POST' && match) {
          const projectID = Number(match[1])
          const body = await request.json()
          patches.push({ projectID, body })
          const project = accepted.catalog.find((entry) => entry.project.id === projectID)!.project
          for (const entry of accepted.catalog) {
            delete entry.project.metadata.role
          }
          project.metadata.role = 'inbox'
          return new Response(JSON.stringify({ changed: true, project }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        }
        return new Response(JSON.stringify(accepted), {
          status: 200,
          headers: { 'Content-Type': 'application/json', ETag: `"snapshot-${patches.length}"` },
        })
      }),
    )

    render(App)

    await fireEvent.click(
      await screen.findByRole('button', { name: 'Inbox project: example-project' }),
    )
    await fireEvent.mouseDown(screen.getByRole('option', { name: /example-workspace/ }))

    await waitFor(() => expect(patches).toHaveLength(1))
    expect(patches).toEqual([
      { projectID: 8, body: { actor: 'kata-web', patch: { role: 'inbox' } } },
    ])
  })

  it('loads, applies, and persists presentation preferences', async () => {
    localStorage.setItem(
      preferencesStorageKey,
      JSON.stringify({
        theme: 'dark',
        columns: ['status', 'title'],
        splitDirection: 'horizontal',
        splitSize: 520,
        collapsedGroups: [],
      }),
    )
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
    )
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(JSON.stringify(snapshot()), {
            status: 200,
            headers: { 'Content-Type': 'application/json', ETag: '"snapshot-1"' },
          }),
      ),
    )

    render(App)

    expect(document.documentElement.classList.contains('dark')).toBe(true)
    await fireEvent.click(await screen.findByRole('button', { name: 'Theme: Dark' }))
    expect(JSON.parse(localStorage.getItem(preferencesStorageKey) ?? '{}')).toEqual(
      expect.objectContaining({ theme: 'system', splitDirection: 'horizontal', splitSize: 520 }),
    )
  })

  it('sends the visible recurrence revision when deleting', async () => {
    history.replaceState(null, '', '/kata?issue=01J00000000000000000000001')
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
    )
    const base = snapshot()
    const accepted = {
      ...base,
      selected: {
        state: 'available',
        issue: { ...base.collection[0], revision: 3 },
        comments: [],
        labels: [],
        links: [],
        recurrences: [
          {
            id: 5,
            uid: '01J00000000000000000000005',
            project_id: 7,
            rrule: 'FREQ=WEEKLY',
            dtstart: '2026-08-01',
            timezone: 'UTC',
            template_title: 'Weekly example',
            template_body: '',
            template_labels: [],
            template_metadata: {},
            author: 'user-a',
            revision: 4,
            created_at: '2026-08-01T09:00:00.000Z',
            updated_at: '2026-08-01T12:00:00.000Z',
          },
        ],
        history: [],
      },
    }
    const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request =
        input instanceof Request
          ? input
          : new Request(new URL(String(input), window.location.origin), init)
      if (request.method === 'DELETE') return new Response(null, { status: 204 })
      return new Response(JSON.stringify(accepted), {
        status: 200,
        headers: { 'Content-Type': 'application/json', ETag: '"snapshot-1"' },
      })
    })
    vi.stubGlobal('fetch', fetcher)

    render(App)
    await fireEvent.click(await screen.findByRole('button', { name: 'Delete recurrence' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Delete' }))

    await waitFor(() => {
      const deletion = fetcher.mock.calls
        .map(([input, init]) =>
          input instanceof Request
            ? input
            : new Request(new URL(String(input), window.location.origin), init),
        )
        .find((request) => request.method === 'DELETE')
      expect(deletion?.headers.get('If-Match')).toBe('"rev-4"')
    })
  })

  it('reuses the comment idempotency key after an uncertain response', async () => {
    history.replaceState(null, '', '/kata?issue=01J00000000000000000000001')
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
    )
    const base = snapshot()
    const accepted = {
      ...base,
      selected: {
        state: 'available',
        issue: base.collection[0],
        comments: [],
        labels: [],
        links: [],
        recurrences: [],
        history: [],
      },
    }
    const commentRequests: Request[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request =
          input instanceof Request
            ? input
            : new Request(new URL(String(input), window.location.origin), init)
        const target = new URL(request.url)
        if (target.pathname.endsWith('/comments') && request.method === 'POST') {
          commentRequests.push(request)
          if (commentRequests.length === 1) return new Response('', { status: 502 })
          return Response.json({
            issue: base.collection[0],
            comment: {
              id: 9,
              uid: '01J00000000000000000000009',
              issue_id: 1,
              author: 'user-a',
              body: 'Retry-safe comment',
              created_at: '2026-08-01T12:00:00.000Z',
            },
            event: null,
            changed: false,
          })
        }
        return Response.json(accepted, { headers: { ETag: '"snapshot-1"' } })
      }),
    )

    render(App)
    const editor = await screen.findByRole('textbox', { name: 'Comment' })
    await fireEvent.input(editor, { target: { value: 'Retry-safe comment' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Add comment' }))
    await waitFor(() => expect(commentRequests).toHaveLength(1))
    const addComment = screen.getByRole('button', { name: 'Add comment' }) as HTMLButtonElement
    await waitFor(() => expect(addComment.disabled).toBe(false))
    await fireEvent.click(addComment)
    await waitFor(() => expect(commentRequests).toHaveLength(2))

    const keys = commentRequests.map((request) => request.headers.get('Idempotency-Key'))
    expect(keys[0]).toBeTruthy()
    expect(keys[1]).toBe(keys[0])
  })

  it('preserves a comment draft during transparent session renewal', async () => {
    history.replaceState(null, '', '/kata?issue=01J00000000000000000000001')
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'expired-session', csrf: 'expired-csrf' }),
    )
    const base = snapshot()
    const accepted = {
      ...base,
      selected: {
        state: 'available',
        issue: base.collection[0],
        comments: [],
        labels: [],
        links: [],
        recurrences: [],
        history: [],
      },
    }
    let commentRejected = false
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request =
          input instanceof Request
            ? input
            : new Request(new URL(String(input), window.location.origin), init)
        const target = new URL(request.url)
        if (target.pathname.endsWith('/comments') && request.method === 'POST') {
          commentRejected = true
          return new Response('', {
            status: 401,
            headers: { 'X-Kata-Web-Authentication': 'loopback' },
          })
        }
        if (target.pathname === '/api/v1/ui/session/local') {
          return Response.json({
            session: 'renewed-session',
            csrf: 'renewed-csrf',
            return_path: '/kata?issue=01J00000000000000000000001',
            writable: true,
            updates: 'poll',
            actor_policy: 'identity',
          })
        }
        if (target.pathname === '/api/v1/ui/daemons') return Response.json(daemonRoster())
        if (target.pathname.endsWith('/api/v1/ui/references')) {
          return Response.json({ issues: [], labels: [], owners: [], projects: [] })
        }
        return Response.json(accepted, { headers: { ETag: '"snapshot-1"' } })
      }),
    )

    render(App)
    const editor = await screen.findByRole('textbox', { name: 'Comment' })
    await fireEvent.input(editor, { target: { value: 'Keep this draft' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Add comment' }))

    await waitFor(() => expect(commentRejected).toBe(true))
    await waitFor(() =>
      expect((screen.getByRole('textbox', { name: 'Comment' }) as HTMLTextAreaElement).value).toBe(
        'Keep this draft',
      ),
    )
  })

  it('keeps accepted authority visible while live updates reconnect', async () => {
    history.replaceState(null, '', '/kata?view=all-open')
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
    )
    const accepted = snapshot()
    accepted.capabilities.updates = 'sse'
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        if (String(input).includes('/api/v1/events/stream')) {
          return new Response(
            new ReadableStream({
              start(controller) {
                controller.close()
              },
            }),
            { status: 200, headers: { 'Content-Type': 'text/event-stream' } },
          )
        }
        return new Response(JSON.stringify(accepted), {
          status: 200,
          headers: { 'Content-Type': 'application/json', ETag: '"snapshot-1"' },
        })
      }),
    )

    render(App)

    expect(await screen.findByRole('button', { name: /Example issue/ })).not.toBeNull()
    expect(await screen.findByRole('status', { name: 'Kata daemon status' })).not.toBeNull()
  })

  it('keeps a reset refresh unconditional across 401 and transparent reauthentication', async () => {
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
    )
    const initial = snapshot()
    initial.capabilities.updates = 'sse'
    const refreshed = snapshot()
    const snapshotRequests: Request[] = []
    let streamRequests = 0
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const request =
          input instanceof Request
            ? input
            : new Request(new URL(String(input), window.location.origin), init)
        const target = new URL(request.url)
        if (target.pathname === '/api/v1/events/stream') {
          streamRequests += 1
          return new Response(
            new ReadableStream({
              start(controller) {
                if (streamRequests === 1) {
                  controller.enqueue(
                    new TextEncoder().encode(
                      'id: 13\nevent: sync.reset_required\ndata: {"reset_required":true}\n\n',
                    ),
                  )
                }
                controller.close()
              },
            }),
            { status: 200, headers: { 'Content-Type': 'text/event-stream' } },
          )
        }
        if (target.pathname === '/api/v1/ui/session/local') {
          return Response.json({
            session: 'renewed-session',
            csrf: 'renewed-csrf',
            return_path: '/kata',
            writable: true,
            updates: 'sse',
            actor_policy: 'request',
          })
        }
        if (target.pathname === '/api/v1/ui/references') {
          return Response.json({ issues: [], labels: [], owners: [], projects: [] })
        }
        if (target.pathname === '/api/v1/ui/snapshot') {
          snapshotRequests.push(request)
          if (snapshotRequests.length === 1) {
            return Response.json(initial, { headers: { ETag: '"snapshot-1"' } })
          }
          if (snapshotRequests.length === 2) {
            return new Response('', {
              status: 401,
              headers: { 'X-Kata-Web-Authentication': 'loopback' },
            })
          }
          if (request.headers.has('If-None-Match')) {
            return new Response(null, { status: 304 })
          }
          return Response.json(refreshed, { headers: { ETag: '"snapshot-2"' } })
        }
        throw new Error(`Unexpected request: ${request.method} ${target.pathname}`)
      }),
    )

    render(App)

    await waitFor(() => expect(snapshotRequests).toHaveLength(3))
    expect(snapshotRequests[2]?.headers.has('If-None-Match')).toBe(false)
    await waitFor(() =>
      expect(screen.queryByRole('status', { name: 'Stale Kata data' })).toBeNull(),
    )
  })

  it('replaces the shell with a visible version mismatch after guarded recovery fails', async () => {
    render(App)

    window.dispatchEvent(new Event('kata:versionMismatch'))

    expect(await screen.findByRole('alert')).not.toBeNull()
    expect(screen.getByRole('heading', { name: 'Kata was updated' })).not.toBeNull()
    expect(screen.getByRole('button', { name: 'Reload Kata' })).not.toBeNull()
  })
})

function snapshot() {
  return {
    contract_version: '1',
    cursor: 12,
    capabilities: { writable: true, updates: 'poll', actor_policy: 'identity' },
    origin: 'https://daemon.example',
    origin_stable: true,
    catalog: [
      {
        project: {
          id: 7,
          uid: '01J00000000000000000000002',
          name: 'example-project',
          metadata: { area: 'Personal' } as Record<string, unknown>,
          revision: 2,
          created_at: '2026-08-01T09:00:00.000Z',
        },
        stats: { Open: 1, Closed: 0, LastEventAt: '2026-08-01T12:00:00.000Z' },
      },
    ],
    collection: [
      {
        id: 1,
        uid: '01J00000000000000000000001',
        project_id: 7,
        project_uid: '01J00000000000000000000002',
        project_name: 'example-project',
        short_id: 'a1',
        qualified_id: 'example-project#a1',
        title: 'Example issue',
        body: '',
        status: 'open',
        metadata: {},
        revision: 1,
        author: 'user-a',
        labels: [],
        created_at: '2026-08-01T09:00:00.000Z',
        updated_at: '2026-08-01T12:00:00.000Z',
      },
    ],
    collection_links: [],
  }
}

function daemonRoster() {
  return {
    daemons: [
      {
        id: 'example-local',
        url: '',
        default: true,
        auth: 'none',
        health: 'connected',
      },
    ],
  }
}
