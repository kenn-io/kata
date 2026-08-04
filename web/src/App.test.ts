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
        const target = new URL(String(input), window.location.origin)
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
        expect(new Headers(init?.headers).get('X-Kata-Web-Session')).toBe('local-session')
        return new Response(JSON.stringify(snapshot()), {
          status: 200,
          headers: { 'Content-Type': 'application/json', ETag: '"snapshot-1"' },
        })
      }),
    )

    render(App)

    expect(await screen.findByRole('region', { name: 'Kata workspace' })).not.toBeNull()
    expect(paths.slice(0, 2)).toEqual(['/api/v1/ui/session/local', '/api/v1/ui/snapshot'])
    expect(sessionStorage.getItem('kata.web.session.v1')).toContain('local-session')
  })

  it('tells a local tab to use the CLI when transparent authorization is unavailable', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const target = new URL(String(input), window.location.origin)
        return new Response('', {
          status: target.pathname === '/api/v1/ui/session/local' ? 404 : 401,
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
      vi.fn(async () => new Response('', { status: 401 })),
    )

    render(App)

    expect(await screen.findByRole('heading', { name: 'Launch Kata securely' })).not.toBeNull()
    expect(sessionStorage.getItem('kata.web.session.v1')).toBeNull()
    expect(screen.queryByText('Loading Kata…')).toBeNull()
  })

  it('renews an expired loopback session transparently', async () => {
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: 'expired-session', csrf: 'expired-csrf' }),
    )
    let snapshotReads = 0
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const target = new URL(String(input), window.location.origin)
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
        snapshotReads += 1
        if (snapshotReads === 1) return new Response('', { status: 401 })
        return new Response(JSON.stringify(snapshot()), {
          status: 200,
          headers: { 'Content-Type': 'application/json', ETag: '"snapshot-1"' },
        })
      }),
    )

    render(App)

    expect(await screen.findByRole('region', { name: 'Kata workspace' })).not.toBeNull()
    expect(sessionStorage.getItem('kata.web.session.v1')).toContain('renewed-session')
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
        return new Response('', { status: 401 })
      }
      if (method === 'POST' && target.pathname === '/api/v1/ui/session/local') {
        return new Response('', { status: 404 })
      }
      if (revoked && target.pathname === '/api/v1/ui/snapshot') {
        return new Response('', { status: 401 })
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
      vi.fn(async () => {
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
    expect(await screen.findByRole('status', { name: 'Kata live updates' })).not.toBeNull()
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
          metadata: { area: 'Personal' },
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
