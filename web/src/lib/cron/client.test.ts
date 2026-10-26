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

import { createDaemonFetch } from '../daemons/client'
import { rawCronValue } from './document'
test('raw definition reads preserve values through credentialed daemon transport', async () => {
  const definition =
    '{"version":1,"options":{"big":9007199254740993,"exp":1e40,"zero":-0,"text":"\\u0061"}}'
  const upstream = vi.fn(
    async () =>
      new Response(
        `{"job":{"uid":"uid","name":"Example job","definition_event_uid":"rev","deleted_at":"today","definition":${definition}}}`,
        { headers: { 'Content-Type': 'application/json' } },
      ),
  )
  setGeneratedFetch(
    createCredentialedFetch(
      () => ({ session: 'tab', csrf: 'csrf' }),
      createDaemonFetch(() => 'remote', upstream),
    ),
  )
  const value = await new CronClient().readDefinition('job', 2, 'uid', new AbortController().signal)
  expect(value.document.source).toBe(definition)
  expect(rawCronValue(value.document, ['options', 'big'])).toBe('9007199254740993')
  expect(value).toMatchObject({
    uid: 'uid',
    name: 'Example job',
    definitionEventUID: 'rev',
    deletedAt: 'today',
  })
  const [url, init] = upstream.mock.calls[0]! as unknown as [URL, RequestInit]
  expect(String(url)).toContain('/api/v1/ui/proxy/api/v1/projects/2/cron/jobs/uid')
  expect(new Headers(init.headers).get('X-Kata-Web-Session')).toBe('tab')
  expect(new Headers(init.headers).get('X-Kata-Web-Daemon')).toBe('remote')
  expect(init.redirect).toBe('error')
})
test('raw reads retain auth rejection and project-local errors for both kinds', async () => {
  for (const kind of ['job', 'workflow'] as const) {
    const reject = vi.fn(() => true)
    setGeneratedFetch(
      createCredentialedFetch(
        () => undefined,
        async () => new Response('unauthorized', { status: 401 }),
        reject,
      ),
    )
    await expect(
      new CronClient().readDefinition(kind, 2, 'uid', new AbortController().signal),
    ).rejects.toMatchObject({ name: 'AuthenticationRequiredError' })
    expect(reject).toHaveBeenCalledOnce()
    setGeneratedFetch(async () => new Response('denied', { status: 403 }))
    await expect(
      new CronClient().readDefinition(kind, 2, 'uid', new AbortController().signal),
    ).rejects.toBeInstanceOf(CronReadError)
  }
})

test('workflow raw reads use encoded UID and the caller abort signal', async () => {
  const signal = new AbortController().signal
  const upstream = vi.fn(
    async () =>
      new Response(
        '{"workflow":{"uid":"uid","name":"Example workflow","definition_event_uid":"rev","definition":{"version":1,"steps":[],"options":{"n":9007199254740993}}}}',
      ),
  )
  setGeneratedFetch(upstream)
  const record = await new CronClient().readDefinition('workflow', 2, 'uid/segment', signal)
  expect(rawCronValue(record.document, ['options', 'n'])).toBe('9007199254740993')
  const [url, init] = upstream.mock.calls[0]! as unknown as [URL, RequestInit]
  expect(String(url)).toContain('/cron/workflows/uid%2Fsegment')
  expect(init.signal).toBe(signal)
})
