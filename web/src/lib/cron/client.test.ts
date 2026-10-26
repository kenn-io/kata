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

test('AgentsViewCron capability and issue history use the credentialed transport', async () => {
  const requests: Request[] = []
  const upstream = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = new Request(new URL(String(input), 'http://127.0.0.2'), init)
    requests.push(request)
    if (request.url.includes('/cron/capabilities'))
      return Response.json({ project_uid: 'p1', event_features: [], agentsview_enabled: true })
    return Response.json({
      contract_version: '2',
      cursor: 1,
      capabilities: { writable: false, updates: 'poll', actor_policy: 'readonly' },
      catalog: [],
      collection: [],
      collection_links: [],
      selected: {
        state: 'visible',
        issue: { uid: 'issue/uid', project_id: 2, title: 'Issue' },
        links: [],
        history: [
          {
            id: 4,
            type: 'issue.closed',
            project_uid: 'p1',
            issue_uid: 'issue/uid',
            payload: '{"transcript":{"url":"https://agentsview.example/sessions/abc"}}',
          },
        ],
      },
    })
  })
  setGeneratedFetch(createCredentialedFetch(() => ({ session: 'tab', csrf: 'csrf' }), upstream))
  const signal = new AbortController().signal
  const client = new CronClient()
  await expect(client.capabilities(2, signal)).resolves.toEqual({ agentsviewEnabled: true })
  const history = await client.issueHistory('p1', 'issue/uid', signal)
  expect(history.map((event) => [event.event_id, event.payload])).toEqual([
    [4, { transcript: { url: 'https://agentsview.example/sessions/abc' } }],
  ])
  expect(new URL(requests[0]!.url).pathname).toBe('/api/v1/projects/2/cron/capabilities')
  const snapshot = new URL(requests[1]!.url)
  expect(snapshot.pathname).toBe('/api/v1/ui/snapshot')
  expect(snapshot.searchParams.get('project_uid')).toBe('p1')
  expect(snapshot.searchParams.get('selected_issue_uid')).toBe('issue/uid')
  expect(snapshot.searchParams.get('include_history')).toBe('true')
  expect(snapshot.searchParams.get('include_graph')).toBe('false')
  for (const request of requests) expect(request.headers.get('X-Kata-Web-Session')).toBe('tab')
})

test('AgentsViewCron reads keep 401 authentication rejection and project-local errors', async () => {
  const reject = vi.fn(() => true)
  setGeneratedFetch(
    createCredentialedFetch(
      () => undefined,
      vi.fn(async () => new Response('', { status: 401 })),
      reject,
    ),
  )
  const signal = new AbortController().signal
  await expect(new CronClient().capabilities(1, signal)).rejects.toMatchObject({
    name: 'AuthenticationRequiredError',
  })
  await expect(new CronClient().issueHistory('p1', 'issue', signal)).rejects.toMatchObject({
    name: 'AuthenticationRequiredError',
  })
  setGeneratedFetch(vi.fn(async () => new Response('', { status: 403 })))
  await expect(new CronClient().capabilities(1, signal)).rejects.toBeInstanceOf(CronReadError)
  await expect(new CronClient().issueHistory('p1', 'issue', signal)).rejects.not.toMatchObject({
    name: 'AuthenticationRequiredError',
  })
})

test('AgentsViewCron capability is disabled when an older daemon omits the field', async () => {
  setGeneratedFetch(vi.fn(async () => Response.json({ project_uid: 'p1', event_features: [] })))
  await expect(new CronClient().capabilities(1, new AbortController().signal)).resolves.toEqual({
    agentsviewEnabled: false,
  })
})
