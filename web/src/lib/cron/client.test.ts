import { expect, test, vi } from 'vitest'
import { setGeneratedFetch, createCredentialedFetch } from '../api/client'
import { CronClient, CronReadError } from './client'
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
test('a linked issue that moved projects is read by its UID', async () => {
  const issue = { issue: { uid: 'issue / identifier' }, comments: [] }
  const upstream = vi.fn(async (input: RequestInfo | URL) =>
    new URL(String(input)).pathname.startsWith('/api/v1/projects/')
      ? Response.json({ error: 'not found' }, { status: 404 })
      : Response.json(issue),
  )
  setGeneratedFetch(createCredentialedFetch(() => ({ session: 'tab', csrf: 'csrf' }), upstream))
  await expect(
    new CronClient().issue(2, 'issue / identifier', new AbortController().signal),
  ).resolves.toEqual(issue)
  expect(
    upstream.mock.calls.map(([url]) => decodeURIComponent(new URL(String(url)).pathname)),
  ).toEqual(['/api/v1/projects/2/issues/issue / identifier', '/api/v1/issues/issue / identifier'])
})
