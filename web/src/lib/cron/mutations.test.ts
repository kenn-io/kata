import { beforeEach, expect, test, vi } from 'vitest'
import { createCredentialedFetch, setGeneratedFetch } from '../api/client'
import { createDaemonFetch } from '../daemons/client'
import { MutationController, type MutationContext } from '../mutations/controller'
import { CronMutations, freezeCronAttempt } from './mutations'
import { parseCronDocument, rawCronValue } from './document'
import { buildCronJobDraft, type CronDefinitionDraft } from './jobDraft'
import {
  addWorkflowStep,
  buildWorkflowDraft,
  serializeWorkflowDraft,
  setWorkflowField,
  setWorkflowStepField,
} from './workflowDraft'

const signal = () => new AbortController().signal
const source =
  '{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"old"},"options":{"big":9007199254740993,"zero":-0}}'
function draft(creating = false): CronDefinitionDraft {
  const value = buildCronJobDraft(
    creating
      ? undefined
      : {
          uid: 'retained',
          name: 'Original',
          definitionEventUID: 'old-event',
          document: parseCronDocument(source),
        },
    7,
  )
  value.values.name = 'Intended'
  if (!creating) {
    value.values.prompt = 'new'
    value.dirty.add('prompt')
  }
  return value
}
function envelope(
  definition: string,
  event = 'new-event',
  name = 'Intended',
  deleted = false,
  kind = 'job',
  uid = 'retained',
) {
  return new Response(
    `{"${kind}":{"uid":${JSON.stringify(uid)},"name":${JSON.stringify(name)},"definition_event_uid":${JSON.stringify(event)},${deleted ? '"deleted_at":"today",' : ''}"definition":${definition}}}`,
  )
}
function setup(authority = { canMutate: true, actorPolicy: 'request' }) {
  const reject = vi.fn()
  const controller = new MutationController({
    authority: () => authority,
    refresh: async () => true,
    onAuthenticationRequired: reject,
  })
  const mutations = new CronMutations({
    onMutation: (attempt, mutate) =>
      controller.execute({ draft: attempt }, mutate).then((value) => value !== false),
    actor: () => 'user-a',
  })
  return { mutations, controller, reject }
}
beforeEach(() => setGeneratedFetch(fetch))

test('freezes retained UID, original guard, intended name and exact raw bytes before sending', async () => {
  const value = draft()
  const attempt = freezeCronAttempt(value, 'save')
  value.values.name = 'Changed after freeze'
  value.values.prompt = 'Changed again'
  expect(attempt.body).toContain('9007199254740993')
  expect(attempt.body).toContain('"zero":-0')
  expect(attempt.body).toContain('"name":"Intended"')
  expect(attempt.body).toContain('"expected_event_uid":"old-event"')
  expect(attempt.body).toContain('"prompt":"new"')
  expect(Object.isFrozen(attempt)).toBe(true)
  const requests: RequestInit[] = []
  setGeneratedFetch(async (url, init) => {
    expect(String(url)).toContain('/projects/7/cron/jobs/retained')
    requests.push(init!)
    return envelope(source)
  })
  expect((await setup().mutations.execute(attempt, signal())).kind).toBe('saved')
  expect(requests[0]!.method).toBe('PUT')
  expect(String(requests[0]!.body)).toContain('"actor":"user-a"')
  expect(new Headers(requests[0]!.headers).has('If-Match')).toBe(false)
})

test('create uses its retained UID without an expected event guard', async () => {
  const value = draft(true),
    attempt = freezeCronAttempt(value, 'save')
  expect(attempt.uid).toBe(value.uid)
  expect(attempt.creating).toBe(true)
  expect(attempt.body).not.toContain('expected_event_uid')
  const calls: string[] = []
  setGeneratedFetch(async (url, init) => {
    calls.push(String(url))
    expect(init!.method).toBe('PUT')
    return envelope(source)
  })
  await setup().mutations.execute(attempt, signal())
  expect(calls).toHaveLength(1)
  expect(calls[0]).toContain('/jobs/' + value.uid)
})

test('pause changes only enabled and delete sends only the original guard plus actor', async () => {
  const value = draft()
  value.dirty.clear()
  value.values.enabled = 'false'
  value.dirty.add('enabled')
  const pause = freezeCronAttempt(value, 'save')
  expect(rawCronValue(parseCronDocument(pause.body), ['definition', 'action', 'prompt'])).toBe(
    '"old"',
  )
  expect(rawCronValue(parseCronDocument(pause.body), ['definition', 'enabled'])).toBe('false')
  const attempt = freezeCronAttempt(value, 'delete')
  expect(attempt.body).toBe('{"expected_event_uid":"old-event"}')
  setGeneratedFetch(async (_url, init) => {
    expect(init!.method).toBe('DELETE')
    expect(JSON.parse(String(init!.body))).toEqual({
      expected_event_uid: 'old-event',
      actor: 'user-a',
    })
    return envelope(source, 'deleted-event', 'Original', true)
  })
  expect((await setup().mutations.execute(attempt, signal())).kind).toBe('deleted')
})

test('workflow save requires an explicit raw document and never permits workflow delete', async () => {
  const value = { ...draft(), kind: 'workflow' as const }
  expect(() => freezeCronAttempt(value, 'save')).toThrow()
  expect(() => freezeCronAttempt(value, 'delete', parseCronDocument('{}'))).toThrow()
  const attempt = freezeCronAttempt(
    value,
    'save',
    parseCronDocument('{"version":1,"steps":[],"options":{"big":9007199254740993}}'),
  )
  setGeneratedFetch(async (url, init) => {
    expect(String(url)).toContain('/workflows/retained')
    expect(init!.method).toBe('PUT')
    expect(String(init!.body)).toContain('9007199254740993')
    return envelope('{}', 'new-event', 'Intended', false, 'workflow')
  })
  expect((await setup().mutations.execute(attempt, signal())).kind).toBe('saved')
})

test.each([false, true])(
  'lost response with matching raw readback resolves committed save (create=%s)',
  async (creating) => {
    const attempt = freezeCronAttempt(draft(creating), 'save')
    const intended = rawCronValue(parseCronDocument(attempt.body), ['definition'])!
    let writes = 0
    setGeneratedFetch(async (_url, init) => {
      if (init!.method === 'PUT') {
        writes++
        throw new TypeError('response lost')
      }
      return envelope(intended.replace('9007199254740993', '9007199254740993e0'), 'committed')
    })
    const { mutations } = setup()
    expect((await mutations.execute(attempt, signal())).kind).toBe('saved')
    expect((await mutations.execute(attempt, signal())).kind).toBe('saved')
    expect(writes).toBe(1)
  },
)

// Literal native readbacks; JSON-v2 omits empty options instead of echoing the sent document.
const nativeDefinitions = {
  job: '{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Inspect"},"issue":{"kind":"per-run","title":"Inspection"},"overlap":"forbid","catchup":"all"}',
  workflow: '{"version":1,"steps":[{"key":"step1","kind":"command","command":"echo inspection"}]}',
}
for (const kind of ['job', 'workflow'] as const) {
  test.each([false, true])(
    `lost response reconciles native empty options for ${kind} (create=%s)`,
    async (creating) => {
      const value = { ...draft(creating), kind }
      const wire = nativeDefinitions[kind].replace(/}$/, ',"options":{}}')
      const attempt = freezeCronAttempt(value, 'save', parseCronDocument(wire))
      const body = attempt.body
      let writes = 0
      setGeneratedFetch(async (_url, init) => {
        if (init!.method === 'PUT') {
          writes++
          throw new TypeError('response lost')
        }
        return envelope(nativeDefinitions[kind], 'committed', 'Intended', false, kind, attempt.uid)
      })
      const { mutations } = setup()
      expect((await mutations.execute(attempt, signal())).kind).toBe('saved')
      expect((await mutations.execute(attempt, signal())).kind).toBe('saved')
      expect(writes).toBe(1)
      expect(attempt.body).toBe(body)
    },
  )
  test.each([
    [false, ' Nightly ', 'Nightly'],
    [true, ' Nightly ', 'Nightly'],
    [false, '\u0085Nightly\u0085', 'Nightly'],
    [true, '\u0085Nightly\u0085', 'Nightly'],
    [false, '\uFEFFNightly\uFEFF', '\uFEFFNightly\uFEFF'],
    [true, '\uFEFFNightly\uFEFF', '\uFEFFNightly\uFEFF'],
  ] as const)(
    `lost response reconciles native stored names for ${kind} (create=%s, name=%j)`,
    async (creating, name, storedName) => {
      const value = { ...draft(creating), kind }
      value.values.name = name
      const attempt = freezeCronAttempt(value, 'save', parseCronDocument(nativeDefinitions[kind]))
      const body = attempt.body
      let writes = 0
      setGeneratedFetch(async (_url, init) => {
        if (init!.method === 'PUT') {
          writes++
          throw new TypeError('response lost')
        }
        return envelope(nativeDefinitions[kind], 'committed', storedName, false, kind, attempt.uid)
      })
      const { mutations } = setup()
      expect((await mutations.execute(attempt, signal())).kind).toBe('saved')
      expect((await mutations.execute(attempt, signal())).kind).toBe('saved')
      expect(writes).toBe(1)
      expect(attempt.body).toBe(body)
    },
  )
}
test('lost response reconciles native empty options on a newly added workflow step', async () => {
  let value = setWorkflowField(buildWorkflowDraft(undefined, 7), 'name', 'Intended')
  value = addWorkflowStep(value, 'command')
  value = setWorkflowStepField(value, value.steps[0]!.editorID, 'command', 'echo inspection')
  const attempt = freezeCronAttempt(value, 'save', serializeWorkflowDraft(value))
  let writes = 0
  setGeneratedFetch(async (_url, init) => {
    if (init!.method === 'PUT') {
      writes++
      throw new TypeError('response lost')
    }
    return envelope(
      nativeDefinitions.workflow,
      'committed',
      'Intended',
      false,
      'workflow',
      attempt.uid,
    )
  })
  expect((await setup().mutations.execute(attempt, signal())).kind).toBe('saved')
  expect(writes).toBe(1)
})

test('old guard is retryable and manual retry reconciles first then sends the identical frozen bytes', async () => {
  const attempt = freezeCronAttempt(draft(), 'save'),
    order: string[] = [],
    bodies: string[] = []
  let writes = 0
  setGeneratedFetch(async (_url, init) => {
    order.push(init!.method!)
    if (init!.method === 'GET') return envelope(source, 'old-event', 'Original')
    bodies.push(String(init!.body))
    if (++writes === 1) throw new TypeError('lost')
    return envelope(source)
  })
  const { mutations } = setup()
  expect((await mutations.execute(attempt, signal())).kind).toBe('retryable')
  expect(order).toEqual(['PUT', 'GET'])
  expect((await mutations.execute(attempt, signal())).kind).toBe('saved')
  expect(order).toEqual(['PUT', 'GET', 'GET', 'PUT'])
  expect(bodies[1]).toBe(bodies[0])
})

test.each([
  ['later edit', 200, 'later', false, 'conflict'],
  ['tombstone', 200, 'later', true, 'conflict'],
  ['missing edit', 404, '', false, 'conflict'],
  ['failed readback', 503, '', false, 'uncertain'],
] as const)('lost response: %s never resends', async (_name, status, event, deleted, outcome) => {
  const attempt = freezeCronAttempt(draft(), 'save')
  let writes = 0
  setGeneratedFetch(async (_url, init) => {
    if (init!.method === 'PUT') {
      writes++
      throw new TypeError('lost')
    }
    return status === 200 ? envelope(source, event, 'Later', deleted) : new Response('', { status })
  })
  const { mutations } = setup()
  const result = await mutations.execute(attempt, signal())
  expect(result.kind).toBe(outcome)
  if (status === 200) expect(result.record?.definitionEventUID).toBe(event)
  expect((await mutations.execute(attempt, signal())).kind).toBe(outcome)
  expect(writes).toBe(1)
})

test('missing create is retryable with its retained UID', async () => {
  const attempt = freezeCronAttempt(draft(true), 'save')
  const urls: string[] = []
  setGeneratedFetch(async (url, init) => {
    urls.push(String(url))
    if (init!.method === 'PUT') throw new TypeError('lost')
    return new Response('', { status: 404 })
  })
  expect((await setup().mutations.execute(attempt, signal())).kind).toBe('retryable')
  expect(urls).toHaveLength(2)
  expect(urls.every((url) => url.endsWith('/jobs/' + attempt.uid))).toBe(true)
})

test.each([
  ['tombstone', 'later', true, 'deleted'],
  ['unchanged', 'old-event', false, 'retryable'],
  ['later edit', 'later', false, 'conflict'],
] as const)('delete readback: %s', async (_name, event, deleted, kind) => {
  setGeneratedFetch(async (_url, init) => {
    if (init!.method === 'DELETE') throw new TypeError('lost')
    return envelope(source, event, 'Original', deleted)
  })
  expect(
    (await setup().mutations.execute(freezeCronAttempt(draft(), 'delete'), signal())).kind,
  ).toBe(kind)
})

test('409 cron conflict retains the attempt and newer raw record; unrelated 409 remains a domain error', async () => {
  const attempt = freezeCronAttempt(draft(), 'save'),
    { mutations, controller } = setup()
  setGeneratedFetch(async (_url, init) =>
    init!.method === 'GET'
      ? envelope(source, 'later', 'Later')
      : Response.json({ error: { code: 'cron_conflict', detail: 'Changed' } }, { status: 409 }),
  )
  const result = await mutations.execute(attempt, signal())
  expect(result).toMatchObject({ kind: 'conflict', record: { definitionEventUID: 'later' } })
  expect(controller.state).toMatchObject({ kind: 'revision-conflict', draft: attempt })
  setGeneratedFetch(async () =>
    Response.json({ error: { code: 'project_archived' } }, { status: 409 }),
  )
  await setup().mutations.execute(freezeCronAttempt(draft(), 'save'), signal())
  const domain = setup()
  await domain.mutations.execute(freezeCronAttempt(draft(), 'save'), signal())
  expect(domain.controller.state).toMatchObject({ kind: 'domain-error', code: 'project_archived' })
})

test('uses existing credentials, CSRF, daemon selection and redirect refusal', async () => {
  setGeneratedFetch(
    createCredentialedFetch(
      () => ({ session: 'tab', csrf: 'csrf' }),
      createDaemonFetch(
        () => 'remote',
        async (url, init) => {
          expect(String(url)).toContain('/api/v1/ui/proxy/api/v1/projects/7/cron/jobs/retained')
          const headers = new Headers(init!.headers)
          expect(headers.get('X-Kata-Web-Session')).toBe('tab')
          expect(headers.get('X-Kata-CSRF')).toBe('csrf')
          expect(headers.get('X-Kata-Web-Daemon')).toBe('remote')
          expect(init!.redirect).toBe('error')
          return envelope(source)
        },
      ),
    ),
  )
  expect((await setup().mutations.execute(freezeCronAttempt(draft(), 'save'), signal())).kind).toBe(
    'saved',
  )
})

test('401 rejects existing authority and read-only/stale/cancelled attempts send no request', async () => {
  const upstream = vi.fn(async () => new Response('', { status: 401 }))
  setGeneratedFetch(upstream)
  const attempt = freezeCronAttempt(draft(), 'save'),
    auth = setup()
  expect((await auth.mutations.execute(attempt, signal())).kind).toBe('blocked')
  expect(auth.reject).toHaveBeenCalledOnce()
  for (const actorPolicy of ['readonly', 'identity'])
    expect(
      (await setup({ canMutate: false, actorPolicy }).mutations.execute(attempt, signal())).kind,
    ).toBe('blocked')
  const abort = new AbortController()
  abort.abort()
  expect((await setup().mutations.execute(attempt, abort.signal)).kind).toBe('blocked')
  expect(upstream).toHaveBeenCalledOnce()
})

test('identity actor policy never inserts a request actor', async () => {
  setGeneratedFetch(async (_url, init) => {
    expect(String(init!.body)).not.toContain('"actor"')
    return envelope(source)
  })
  await setup({ canMutate: true, actorPolicy: 'identity' }).mutations.execute(
    freezeCronAttempt(draft(), 'save'),
    signal(),
  )
})

test('manual reconcile reads without writing and compares semantic numbers without rounding', async () => {
  const attempt = freezeCronAttempt(draft(), 'save'),
    intended = rawCronValue(parseCronDocument(attempt.body), ['definition'])!
  setGeneratedFetch(async (_url, init) => {
    expect(init!.method).toBe('GET')
    return envelope(intended.replace('9007199254740993', '9007199254740992'))
  })
  expect((await setup().mutations.reconcile(attempt, signal())).kind).toBe('conflict')
})

test('an authority scope change permanently fences an old attempt', async () => {
  let scope = 'original'
  const upstream = vi.fn(async () => {
    throw new TypeError('lost')
  })
  setGeneratedFetch(upstream)
  const controller = new MutationController({
    authority: () => ({ canMutate: true, actorPolicy: 'request' }),
    refresh: async () => true,
  })
  const mutations = new CronMutations({
    scope: () => scope,
    onMutation: (attempt, mutate) =>
      controller.execute({ draft: attempt }, mutate).then((value) => value !== false),
  })
  const attempt = freezeCronAttempt(draft(), 'save')
  expect((await mutations.execute(attempt, signal())).kind).toBe('uncertain')
  scope = 'replacement'
  expect((await mutations.execute(attempt, signal())).kind).toBe('blocked')
  expect(upstream).toHaveBeenCalledTimes(2)
})

import { CronMutationGate } from './mutations'
test('the App cron gate blocks global pending writes and cancels/fences drafts on authority changes', async () => {
  let pending = true,
    writable = true
  let observedSignal: AbortSignal | undefined, finish!: () => void
  const controller = new MutationController({
    authority: () => ({ canMutate: writable, actorPolicy: 'request' }),
    refresh: async () => true,
  })
  const gate = new CronMutationGate({
    pending: () => pending,
    canMutate: () => writable,
    runMutation: (draft, mutate) =>
      controller.execute({ draft }, mutate).then((value) => value !== false),
  })
  const attempt = freezeCronAttempt(draft(), 'save'),
    mutate = vi.fn(async (context: MutationContext) => {
      observedSignal = context.signal
      await new Promise<void>((resolve) => {
        finish = resolve
      })
      return { status: 200, data: true, headers: new Headers() }
    })
  expect(await gate.run(attempt, mutate)).toBe(false)
  pending = false
  writable = false
  expect(await gate.run(attempt, mutate)).toBe(false)
  expect(mutate).not.toHaveBeenCalled()
  writable = true
  const result = gate.run(attempt, mutate)
  expect(observedSignal?.aborted).toBe(false)
  gate.fence()
  expect(observedSignal?.aborted).toBe(true)
  finish()
  expect(await result).toBe(false)
  expect(await gate.run(attempt, mutate)).toBe(false)
  expect(mutate).toHaveBeenCalledOnce()
})

test('a cron conflict carries the raw current response even when a subsequent read would fail', async () => {
  const current =
    '{"uid":"retained","name":"Later","definition_event_uid":"later","definition":{"options":{"big":9007199254740993}}}'
  let reads = 0
  setGeneratedFetch(async (_url, init) => {
    if (init!.method === 'GET') {
      reads++
      return new Response('', { status: 403 })
    }
    return new Response(
      `{"status":409,"error":{"code":"cron_conflict","message":"The definition changed","data":{"current":${current}}}}`,
      { status: 409 },
    )
  })
  const { mutations } = setup()
  const result = await mutations.execute(freezeCronAttempt(draft(), 'save'), signal())
  expect(result).toMatchObject({ kind: 'conflict', record: { definitionEventUID: 'later' } })
  expect(result.record!.document.source).toBe('{"options":{"big":9007199254740993}}')
  expect(reads).toBe(0)
})

test('unrelated domain errors retain the real API message', async () => {
  setGeneratedFetch(async () =>
    Response.json(
      { error: { code: 'project_archived', message: 'This project is archived.' } },
      { status: 409 },
    ),
  )
  const { mutations, controller } = setup()
  await mutations.execute(freezeCronAttempt(draft(), 'save'), signal())
  expect(controller.state).toMatchObject({
    kind: 'domain-error',
    code: 'project_archived',
    detail: 'This project is archived.',
  })
})

test('an interrupted error response body reads back without automatically resending', async () => {
  const attempt = freezeCronAttempt(draft(), 'save'),
    intended = rawCronValue(parseCronDocument(attempt.body), ['definition'])!
  setGeneratedFetch(async (_url, init) => {
    if (init!.method === 'GET') return envelope(intended)
    const response = new Response('', { status: 502 })
    response.text = async () => {
      throw new TypeError('body interrupted')
    }
    return response
  })
  expect((await setup().mutations.execute(attempt, signal())).kind).toBe('saved')
})

test('401 during readback uses the existing rejection path and never resends', async () => {
  let writes = 0
  setGeneratedFetch(async (_url, init) => {
    if (init!.method === 'PUT') {
      writes++
      throw new TypeError('lost')
    }
    return new Response('', { status: 401 })
  })
  const { mutations, reject } = setup()
  expect((await mutations.execute(freezeCronAttempt(draft(), 'save'), signal())).kind).toBe(
    'blocked',
  )
  expect(reject).toHaveBeenCalledOnce()
  expect(writes).toBe(1)
})

test('a cancelled late response cannot become a success', async () => {
  const abort = new AbortController()
  let finish!: (response: Response) => void
  setGeneratedFetch(
    async () =>
      new Promise<Response>((resolve) => {
        finish = resolve
      }),
  )
  const { mutations } = setup()
  const result = mutations.execute(freezeCronAttempt(draft(), 'save'), abort.signal)
  abort.abort()
  finish(envelope(source))
  expect((await result).kind).toBe('blocked')
})

test.each([
  ['gate fence', 'saved'],
  ['caller abort', 'saved'],
  ['scope change', 'saved'],
  ['gate fence', 'conflict'],
  ['caller abort', 'conflict'],
  ['scope change', 'conflict'],
] as const)(
  '%s during final authority refresh blocks a reconciled %s without its old record',
  async (change, outcome) => {
    const attempt = freezeCronAttempt(draft(), 'save')
    const intended = rawCronValue(parseCronDocument(attempt.body), ['definition'])!
    let writes = 0,
      reads = 0,
      scope = 'original'
    let releaseRefresh!: () => void, startRefresh!: () => void
    const refreshStarted = new Promise<void>((resolve) => {
      startRefresh = resolve
    })
    const refreshFinished = new Promise<void>((resolve) => {
      releaseRefresh = resolve
    })
    const abort = new AbortController()
    const controller = new MutationController({
      authority: () => ({ canMutate: true, actorPolicy: 'request' }),
      refresh: async () => {
        startRefresh()
        await refreshFinished
        return true
      },
    })
    const gate = new CronMutationGate({
      pending: () => false,
      canMutate: () => true,
      runMutation: (draft, mutate) =>
        controller.execute({ draft }, mutate).then((value) => value !== false),
    })
    const mutations = new CronMutations({
      scope: () => scope,
      onMutation: (draft, mutate) => gate.run(draft, mutate),
    })
    setGeneratedFetch(async (_url, init) => {
      if (init!.method === 'PUT') {
        writes++
        throw new TypeError('response lost')
      }
      reads++
      return outcome === 'saved' ? envelope(intended) : envelope(source, 'later', 'Later')
    })
    const result = mutations.execute(attempt, abort.signal)
    await refreshStarted
    expect(writes).toBe(1)
    expect(reads).toBe(1)
    if (change === 'gate fence') gate.fence()
    else if (change === 'caller abort') abort.abort()
    else scope = 'replacement'
    releaseRefresh()
    expect(await result).toEqual({ kind: 'blocked' })
    expect(writes).toBe(1)
    expect(reads).toBe(1)
  },
)

test('a rejected callback cannot expose an otherwise successful mutation outcome', async () => {
  const controller = new MutationController({
    authority: () => ({ canMutate: true, actorPolicy: 'request' }),
    refresh: async () => true,
  })
  const gate = new CronMutationGate({
    pending: () => false,
    canMutate: () => true,
    runMutation: async (draft, mutate) => {
      await controller.execute({ draft }, mutate)
      return false
    },
  })
  const mutations = new CronMutations({ onMutation: (draft, mutate) => gate.run(draft, mutate) })
  let writes = 0
  setGeneratedFetch(async () => {
    writes++
    return envelope(source)
  })
  expect(await mutations.execute(freezeCronAttempt(draft(), 'save'), signal())).toEqual({
    kind: 'blocked',
  })
  expect(writes).toBe(1)
})
