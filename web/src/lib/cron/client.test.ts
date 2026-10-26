import { expect, test, vi } from 'vitest'
import { setGeneratedFetch, createCredentialedFetch } from '../api/client'
import { CronClient, CronReadError } from './client'
test('uses credentialed fetch, repeated status, cursor without optional enrichment', async () => {
  const upstream = vi.fn(async () => Response.json({ runs: [], next_before_uid: 'next' }))
  setGeneratedFetch(createCredentialedFetch(() => ({ session: 'tab', csrf: 'csrf' }), upstream))
  const signal = new AbortController().signal
  await new CronClient().runs(2, signal, { statuses: ['failed', 'running'], beforeUID: 'cursor' })
  const [url, options] = upstream.mock.calls[0]! as unknown as [URL, RequestInit]
  expect(String(url)).toContain('status=failed&status=running')
  expect(String(url)).toContain('before_uid=cursor')
  expect(String(url)).not.toContain('refresh_usage')
  expect(options.signal).toBe(signal)
  expect(new Headers(options.headers).get('X-Kata-Web-Session')).toBe('tab')
  expect(options.redirect).toBe('error')
})
test('403 is project-local and 401 follows auth transition', async () => {
  const reject = vi.fn(() => true)
  setGeneratedFetch(
    createCredentialedFetch(
      () => undefined,
      vi.fn(async () => new Response('', { status: 401 })),
      reject,
    ),
  )
  await expect(new CronClient().summary(1, new AbortController().signal)).rejects.toMatchObject({
    name: 'AuthenticationRequiredError',
  })
  expect(reject).toHaveBeenCalledOnce()
  setGeneratedFetch(vi.fn(async () => new Response('', { status: 403 })))
  await expect(new CronClient().summary(1, new AbortController().signal)).rejects.toBeInstanceOf(
    CronReadError,
  )
})
