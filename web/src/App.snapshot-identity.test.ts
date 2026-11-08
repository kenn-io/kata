// @vitest-environment-options { "url": "http://127.0.0.2/kata" }

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { tick } from 'svelte'
import { afterEach, expect, it, vi } from 'vitest'

vi.mock('./lib/kata/projection', async (importOriginal) => {
  const original = await importOriginal<typeof import('./lib/kata/projection')>()
  return { ...original, normalizeKataUISnapshot: vi.fn(original.normalizeKataUISnapshot) }
})

import App from './App.svelte'
import { normalizeKataUISnapshot } from './lib/kata/projection'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.clearAllMocks()
  vi.restoreAllMocks()
})

it('keeps the normalized collection when focus revalidation returns unchanged', async () => {
  vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
  sessionStorage.clear()
  sessionStorage.setItem(
    'kata.web.session.v1',
    JSON.stringify({ session: 'tab-session', csrf: 'tab-csrf' }),
  )
  localStorage.clear()
  history.replaceState(null, '', '/kata')
  const requests: Request[] = []
  let referencesRequested = false
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request =
        input instanceof Request
          ? input
          : new Request(new URL(String(input), window.location.origin), init)
      const target = new URL(request.url)
      if (target.pathname === '/api/v1/ui/daemons')
        return Response.json({
          daemons: [
            { id: 'example-local', url: '', default: true, auth: 'none', health: 'connected' },
          ],
        })
      if (target.pathname.endsWith('/references')) {
        referencesRequested = true
        return Response.json({ issues: [], labels: [], owners: [], projects: [] })
      }
      if (target.pathname === '/api/v1/ui/telemetry')
        return Response.json({ status: 'queued' }, { status: 202 })
      requests.push(request)
      if (requests.length > 1) return new Response(null, { status: 304 })
      return Response.json(
        {
          contract_version: '2',
          cursor: 12,
          capabilities: { writable: true, updates: 'poll', actor_policy: 'identity' },
          origin: 'https://daemon.example',
          origin_stable: true,
          catalog: [
            {
              project: {
                active: true,
                id: 7,
                uid: '01J00000000000000000000002',
                name: 'example-project',
                metadata: { role: 'inbox' },
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
        },
        { headers: { ETag: '"snapshot-1"' } },
      )
    }),
  )
  render(App)
  await screen.findByRole('button', { name: /Example issue/ })
  await waitFor(() => expect(referencesRequested).toBe(true))
  await tick()
  const normalizations = vi.mocked(normalizeKataUISnapshot).mock.calls.length
  await fireEvent(window, new Event('focus'))
  await waitFor(() => expect(requests).toHaveLength(2))
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'New task', exact: true }).getAttribute('aria-disabled'),
    ).not.toBe('true'),
  )
  await tick()
  expect(requests[1]!.headers.get('If-None-Match')).toBe('"snapshot-1"')
  expect(normalizeKataUISnapshot).toHaveBeenCalledTimes(normalizations)
  expect(screen.getByRole('button', { name: /Example issue/ })).not.toBeNull()
})
