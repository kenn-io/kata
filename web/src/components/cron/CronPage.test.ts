import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte'
import { afterEach, expect, test, vi } from 'vitest'
import { tick } from 'svelte'
import { parseCronDocument, rawCronValue } from '../../lib/cron/document'
import { setGeneratedFetch } from '../../lib/api/client'
import type { CronMutationOptions } from '../../lib/cron/mutations'
import type { CronJob } from '../../lib/cron/types'
import CronPage from './CronPage.svelte'
import CronPageWithEditor from './CronPageWithEditor.test.svelte'
import { emptyCronState } from '../../lib/cron/controller'
import { defaultCronRoute } from '../../lib/cron/route'
afterEach(() => {
  cleanup()
  setGeneratedFetch(fetch)
})
test('read-only page uses URL callbacks for tabs, no mutation controls', async () => {
  const navigate = vi.fn()
  render(CronPage, {
    route: defaultCronRoute(),
    state: emptyCronState(),
    onNavigate: navigate,
    onLoadMore: vi.fn(),
    onRefresh: vi.fn(),
    onOpenIssue: vi.fn(),
  })
  expect(screen.getByRole('heading', { name: 'Cron' })).toBeTruthy()
  await fireEvent.click(screen.getByRole('radio', { name: 'Runs' }))
  expect(navigate).toHaveBeenCalledWith(expect.objectContaining({ tab: 'runs' }))
  expect(screen.queryByRole('button', { name: 'New job' })).toBeNull()
})
test('a denied project is reported without hiding the page', () => {
  const state = emptyCronState()
  state.projects = [
    {
      project: { id: 1, uid: 'p1', name: 'example-project' },
      jobs: [],
      runs: [],
      workflows: [],
      hasRecords: false,
      error: 'Project not readable',
    },
  ]
  render(CronPage, {
    route: { ...defaultCronRoute(), tab: 'runs' },
    state,
    onNavigate: vi.fn(),
    onLoadMore: vi.fn(),
    onRefresh: vi.fn(),
    onOpenIssue: vi.fn(),
  })
  expect(screen.getByText(/Project not readable/)).toBeTruthy()
})

test('run rows use a readable job name from their project catalog', () => {
  const state = emptyCronState()
  state.projects = [
    {
      project: { id: 1, uid: 'p1', name: 'example-project' },
      hasRecords: true,
      jobs: [{ uid: 'job', name: 'Inspect workspace' } as never],
      workflows: [],
      runs: [
        {
          uid: 'r',
          status: 'failed',
          job_uid: 'job',
          created_at: '2026-01-01',
          summary: { version: 1 },
        } as never,
      ],
    },
  ]
  render(CronPage, {
    route: { ...defaultCronRoute(), tab: 'runs' },
    state,
    onNavigate: vi.fn(),
    onLoadMore: vi.fn(),
    onRefresh: vi.fn(),
    onOpenIssue: vi.fn(),
  })
  expect(screen.getByRole('button', { name: 'Inspect workspace' })).toBeTruthy()
})

test('switching from Jobs to Runs clears the job-only never-ran status', async () => {
  const onNavigate = vi.fn()
  render(CronPage, {
    route: { ...defaultCronRoute(), statuses: ['never_ran'] },
    state: emptyCronState(),
    onNavigate,
    onLoadMore: vi.fn(),
    onRefresh: vi.fn(),
    onOpenIssue: vi.fn(),
  })
  await fireEvent.click(screen.getByRole('radio', { name: 'Runs' }))
  expect(onNavigate).toHaveBeenCalledWith(expect.objectContaining({ tab: 'runs', statuses: [] }))
})

const definition =
  '{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Old prompt"},"issue":{"kind":"per-run","title":"Result"},"overlap":"forbid","catchup":"all","options":{"big":9007199254740993}}'
const envelope = (raw = definition, event = 'old-event') =>
  new Response(
    `{"job":{"uid":"job1","name":"Example job","definition_event_uid":"${event}","definition":${raw}}}`,
    { headers: { 'Content-Type': 'application/json' } },
  )
const mutate: CronMutationOptions['onMutation'] = async (_attempt, send) =>
  (await send({ headers: new Headers(), body: (body) => body })).status === 200
function writableProps() {
  const state = emptyCronState()
  state.projects = [
    {
      project: { id: 1, uid: 'p1', name: 'example-project' },
      jobs: [],
      runs: [],
      workflows: [],
      hasRecords: true,
    },
  ]
  state.selected = { kind: 'job', projectUID: 'p1', uid: 'job1' }
  state.detail = {
    uid: 'job1',
    name: 'Example job',
    definition_event_uid: 'old-event',
    definition: JSON.parse(definition),
    last_run: null,
  } as never
  return {
    route: defaultCronRoute(),
    state,
    onNavigate: vi.fn(),
    onLoadMore: vi.fn(),
    onRefresh: vi.fn(),
    onOpenIssue: vi.fn(),
    canMutate: true,
    mutationPending: false,
    authorityScope: 'one',
    onMutation: mutate,
    resolveIssue: vi.fn(async () => '01ARZ3NDEKTSV4RRFFQ69G5FAV'),
  }
}
const workflowDefinition =
  '{"version":1,"about":"About","options":{"big":9007199254740993},"steps":[{"key":"first","kind":"command","command":"echo ok"}]}'
function workflowProps() {
  const p = writableProps()
  p.state.selected = { kind: 'workflow', projectUID: 'p1', uid: 'workflow1' }
  p.state.detail = {
    uid: 'workflow1',
    name: 'Example workflow',
    definition: JSON.parse(workflowDefinition),
  } as never
  p.route.tab = 'workflows'
  return p
}
function workflowEnvelope(def = workflowDefinition, event = 'event1') {
  return new Response(
    `{"workflow":{"uid":"workflow1","name":"Example workflow","definition_event_uid":"${event}","definition":${def}}}`,
    { headers: { 'Content-Type': 'application/json' } },
  )
}
test('workflow edit uses raw authenticated guarded transport and has no workflow delete', async () => {
  const p = workflowProps(),
    requests: { url: string; init: RequestInit | undefined }[] = []
  setGeneratedFetch(async (url, init) => {
    requests.push({ url: String(url), init })
    return workflowEnvelope()
  })
  render(CronPageWithEditor, p)
  expect(screen.queryByRole('button', { name: 'Delete workflow' })).toBeNull()
  await fireEvent.click(screen.getByRole('button', { name: 'Edit workflow' }))
  await screen.findByRole('textbox', { name: 'Name' })
  await fireEvent.input(screen.getByRole('textbox', { name: 'About' }), {
    target: { value: 'Changed' },
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Save workflow' }))
  await vi.waitFor(() => expect(requests.filter((r) => r.init?.method === 'PUT')).toHaveLength(1))
  const saved = requests.find((r) => r.init?.method === 'PUT')!
  expect(saved.url).toContain('/projects/1/cron/workflows/workflow1')
  expect(String(saved.init?.body)).toContain('9007199254740993')
  expect(String(saved.init?.body)).toContain('"expected_event_uid":"event1"')
  expect(String(saved.init?.body)).toContain('"about":"Changed"')
})
test('workflow creation retains one UID across project changes and submits workflow fields', async () => {
  const p = workflowProps(),
    requests: string[] = []
  p.state.selected = undefined
  p.state.projects.push({
    project: { id: 2, uid: 'p2', name: 'other-project' },
    jobs: [],
    workflows: [],
    runs: [],
    hasRecords: false,
  })
  setGeneratedFetch(async (url) => {
    requests.push(String(url))
    return workflowEnvelope()
  })
  render(CronPageWithEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'New workflow' }))
  await fireEvent.change(screen.getByRole('combobox', { name: 'Project' }), {
    target: { value: 'p2' },
  })
  await fireEvent.input(screen.getByRole('textbox', { name: 'Name' }), {
    target: { value: 'New workflow' },
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Add command step' }))
  await fireEvent.input(screen.getByRole('textbox', { name: 'Command' }), {
    target: { value: 'echo ok' },
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Save workflow' }))
  await vi.waitFor(() => expect(requests).toHaveLength(1))
  expect(requests[0]).toMatch(/\/projects\/2\/cron\/workflows\/[0-7][0-9A-HJKMNP-TV-Z]{25}$/)
})
test('workflow conflict rebase visibly refuses ambiguous concurrent rename', async () => {
  const p = workflowProps()
  let phase = 'load'
  setGeneratedFetch(async (_url, init) => {
    if (phase === 'load') return workflowEnvelope()
    return init?.method === 'PUT'
      ? new Response('{"error":{"code":"cron_conflict","detail":"changed"}}', { status: 409 })
      : workflowEnvelope(workflowDefinition.replace('"first"', '"remote"'), 'event2')
  })
  render(CronPageWithEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'Edit workflow' }))
  await screen.findByRole('textbox', { name: 'Name' })
  await fireEvent.input(screen.getByRole('textbox', { name: 'About' }), {
    target: { value: 'My about' },
  })
  phase = 'conflict'
  await fireEvent.click(screen.getByRole('button', { name: 'Save workflow' }))
  await screen.findByRole('button', { name: 'Rebase my edits on newer version' })
  await fireEvent.click(screen.getByRole('button', { name: 'Rebase my edits on newer version' }))
  await screen.findByText(/Ambiguous step correspondence/)
  expect((screen.getByRole('textbox', { name: 'About' }) as HTMLTextAreaElement).value).toBe(
    'My about',
  )
  expect(
    (screen.getByRole('button', { name: 'Save workflow' }) as HTMLButtonElement).disabled,
  ).toBe(true)
})
test('writable page creates a draft in the selected project and keeps existing records untouched', async () => {
  const p = writableProps()
  p.state.selected = undefined
  p.state.projects.push({
    project: { id: 2, uid: 'p2', name: 'other-project' },
    jobs: [],
    runs: [],
    workflows: [],
    hasRecords: false,
  })
  render(CronPageWithEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'New job' }))
  expect((screen.getByRole('combobox', { name: 'Project' }) as HTMLSelectElement).value).toBe('p1')
  await fireEvent.change(screen.getByRole('combobox', { name: 'Project' }), {
    target: { value: 'p2' },
  })
  await fireEvent.input(screen.getByRole('textbox', { name: 'Name' }), {
    target: { value: 'New job' },
  })
  await fireEvent.input(screen.getByRole('textbox', { name: 'Prompt' }), {
    target: { value: 'Inspect workspace' },
  })
  const requests: string[] = []
  setGeneratedFetch(async (url, init) => {
    requests.push(String(url))
    expect(String(init?.body)).toContain('"name":"New job"')
    return envelope()
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
  await vi.waitFor(() => expect(requests).toHaveLength(1))
  expect(requests[0]).toContain('/projects/2/cron/jobs/')
  expect(p.state.detail?.name).toBe('Example job')
})
test.each([
  ['pause', true, false],
  ['unpause', false, true],
  ['delete', true, true],
] as const)(
  'lifecycle %s keeps the displayed definition guard until explicit refresh',
  async (operation, displayedEnabled, newerEnabled) => {
    const p = writableProps()
    p.state = {
      ...p.state,
      detail: {
        ...(p.state.detail as CronJob),
        definition: JSON.parse(
          definition.replace('"enabled":true', `"enabled":${displayedEnabled}`),
        ),
      },
    }
    const newer = definition
      .replace('Old prompt', 'Newer prompt')
      .replace('"enabled":true', `"enabled":${newerEnabled}`)
    const writes: RequestInit[] = []
    setGeneratedFetch(async (_url, init) => {
      if (init?.method === 'GET') return envelope(newer, 'new-event')
      writes.push(init!)
      return new Response(
        `{"job":{"uid":"job1","name":"Example job","definition_event_uid":"committed",${operation === 'delete' ? '"deleted_at":"2026-10-25T00:00:00Z",' : ''}"definition":${newer}}}`,
      )
    })
    const view = render(CronPageWithEditor, p)
    const label = operation === 'delete' ? 'Delete job' : displayedEnabled ? 'Pause' : 'Unpause'
    const act = async (actionLabel: string) => {
      await fireEvent.click(screen.getByRole('button', { name: actionLabel }))
      if (operation === 'delete')
        await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete' }))
    }
    await act(label)
    await vi.waitFor(() =>
      expect((screen.getByRole('button', { name: label }) as HTMLButtonElement).disabled).toBe(
        false,
      ),
    )
    expect(writes).toHaveLength(0)
    const detail = screen.getByRole('dialog', { name: 'Job detail' })
    expect(within(detail).getByRole('alert').textContent).toMatch(/changed|refresh|conflict/i)

    p.state = {
      ...p.state,
      detail: {
        ...(p.state.detail as CronJob),
        definition_event_uid: 'new-event',
        definition: JSON.parse(newer),
      },
    }
    await view.rerender({ ...p })
    await act(operation === 'delete' ? 'Delete job' : newerEnabled ? 'Pause' : 'Unpause')
    await vi.waitFor(() => expect(writes).toHaveLength(1))
    const body = JSON.parse(String(writes[0]!.body))
    expect(body.expected_event_uid).toBe('new-event')
    if (operation !== 'delete') expect(body.definition.enabled).toBe(!newerEnabled)
    expect(writes[0]!.method).toBe(operation === 'delete' ? 'DELETE' : 'PUT')
  },
)

test('pause uses raw guarded definition, and delete waits for confirmation', async () => {
  const p = writableProps(),
    requests: RequestInit[] = []
  setGeneratedFetch(async (_url, init) => {
    requests.push(init!)
    return envelope(
      init?.method === 'PUT' ? definition.replace('"enabled":true', '"enabled":false') : definition,
      init?.method === 'GET' ? 'old-event' : 'new-event',
    )
  })
  render(CronPageWithEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'Pause' }))
  await vi.waitFor(() => expect(requests.filter((r) => r.method === 'PUT')).toHaveLength(1))
  const body = String(requests.find((r) => r.method === 'PUT')?.body)
  expect(body).toContain('"expected_event_uid":"old-event"')
  expect(body).toContain('9007199254740993')
  expect(body).toContain('"enabled":false')
  await vi.waitFor(() => expect(p.onRefresh).toHaveBeenCalled())
  await fireEvent.click(screen.getByRole('button', { name: 'Delete job' }))
  expect(requests.filter((r) => r.method === 'DELETE')).toHaveLength(0)
  await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete' }))
  await vi.waitFor(() => expect(requests.filter((r) => r.method === 'DELETE')).toHaveLength(1))
})
test('uncertain saves retain frozen request and require readback before explicit retry', async () => {
  const p = writableProps()
  let phase = 'load'
  const requests: RequestInit[] = []
  setGeneratedFetch(async (_url, init) => {
    requests.push(init!)
    if (phase === 'load') return envelope()
    throw new Error('connection lost')
  })
  render(CronPageWithEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'Edit job' }))
  await screen.findByRole('textbox', { name: 'Prompt' })
  await fireEvent.input(screen.getByRole('textbox', { name: 'Prompt' }), {
    target: { value: 'Changed' },
  })
  phase = 'uncertain'
  await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
  await screen.findByRole('button', { name: 'Check outcome' })
  expect(requests.filter((r) => r.method === 'PUT')).toHaveLength(1)
  expect((screen.getByRole('textbox', { name: 'Prompt' }) as HTMLInputElement).disabled).toBe(true)
  setGeneratedFetch(async (_url, init) => {
    requests.push(init!)
    return envelope()
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Check outcome' }))
  await screen.findByRole('button', { name: 'Retry save' })
  expect(requests.filter((r) => r.method === 'PUT')).toHaveLength(1)
  setGeneratedFetch(async (_url, init) => {
    requests.push(init!)
    return envelope()
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Retry save' }))
  await vi.waitFor(() => expect(requests.filter((r) => r.method === 'PUT')).toHaveLength(2))
  expect(requests.filter((r) => r.method === 'PUT').map((r) => r.body)[1]).toBe(
    requests.filter((r) => r.method === 'PUT').map((r) => r.body)[0],
  )
})
test('conflict retains edits, shows newer definition and requires explicit rebase', async () => {
  const p = writableProps()
  let first = true,
    puts = 0
  setGeneratedFetch(async (_url, init) => {
    if (init?.method === 'PUT') {
      puts++
      return new Response('{"error":{"code":"cron_conflict","detail":"changed"}}', { status: 409 })
    }
    if (first) {
      first = false
      return envelope()
    }
    return envelope(definition.replace('Old prompt', 'Newer prompt'), 'new-event')
  })
  render(CronPageWithEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'Edit job' }))
  await screen.findByRole('textbox', { name: 'Prompt' })
  await fireEvent.input(screen.getByRole('textbox', { name: 'Prompt' }), {
    target: { value: 'My edits' },
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
  await screen.findByRole('button', { name: 'Rebase my edits on newer version' })
  expect(screen.getByRole('region', { name: 'Definition conflict' }).textContent).toContain(
    'Newer prompt',
  )
  expect((screen.getByRole('textbox', { name: 'Prompt' }) as HTMLInputElement).value).toBe(
    'My edits',
  )
  expect(
    ((await screen.findByRole('button', { name: 'Save job' })) as HTMLButtonElement).disabled,
  ).toBe(true)
  await fireEvent.click(screen.getByRole('button', { name: 'Rebase my edits on newer version' }))
  setGeneratedFetch(async (_url, init) => {
    if (init?.method === 'PUT') {
      expect(String(init.body)).toContain('"expected_event_uid":"new-event"')
      expect(String(init.body)).toContain('My edits')
      puts++
    }
    return envelope()
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
  await vi.waitFor(() => expect(puts).toBe(2))
})
test('authority scope change permanently fences an open draft and hides create project picker on edit', async () => {
  const p = writableProps(),
    requests: RequestInit[] = []
  setGeneratedFetch(async (_url, init) => {
    requests.push(init!)
    return envelope()
  })
  const view = render(CronPageWithEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'Edit job' }))
  await screen.findByRole('textbox', { name: 'Prompt' })
  expect(screen.queryByRole('combobox', { name: 'Project' })).toBeNull()
  await view.rerender({ ...p, authorityScope: 'two' })
  expect((screen.getByRole('button', { name: 'Save job' }) as HTMLButtonElement).disabled).toBe(
    true,
  )
  expect(screen.getByRole('alert').textContent).toContain('Authority changed')
  await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
  expect(requests.filter((r) => r.method === 'PUT')).toHaveLength(0)
})
test('rebase reapplies trigger selection before its edited schedule', async () => {
  const p = writableProps()
  let first = true,
    puts = 0
  setGeneratedFetch(async (_url, init) => {
    if (init?.method === 'PUT') {
      puts++
      return new Response('{"error":{"code":"cron_conflict","detail":"changed"}}', { status: 409 })
    }
    if (first) {
      first = false
      return envelope()
    }
    return envelope(definition, 'new-event')
  })
  render(CronPageWithEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'Edit job' }))
  await screen.findByRole('textbox', { name: 'Prompt' })
  await fireEvent.change(screen.getByRole('combobox', { name: 'Schedule' }), {
    target: { value: 'interval' },
  })
  await fireEvent.input(screen.getByRole('textbox', { name: 'Interval' }), {
    target: { value: '5m' },
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
  await screen.findByRole('button', { name: 'Rebase my edits on newer version' })
  await screen.findByRole('button', { name: 'Save job' })
  await fireEvent.click(screen.getByRole('button', { name: 'Rebase my edits on newer version' }))
  await vi.waitFor(() =>
    expect((screen.getByRole('textbox', { name: 'Interval' }) as HTMLInputElement).value).toBe(
      '300',
    ),
  )
  expect(screen.queryAllByRole('alert').map((e) => e.textContent)).toEqual([])
  let sentBody = ''
  setGeneratedFetch(async (_url, init) => {
    if (init?.method === 'PUT') {
      sentBody = String(init.body)
      puts++
    }
    return envelope()
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
  expect(screen.queryAllByRole('alert').map((e) => e.textContent)).toEqual([])
  await vi.waitFor(() => expect(puts).toBe(2))
  expect(
    rawCronValue(parseCronDocument(sentBody), ['definition', 'trigger', 'interval_seconds']),
  ).toBe('300')
})

test('delete conflict requires explicit rebase and renews delete confirmation with the new guard', async () => {
  const p = writableProps()
  let first = true,
    deletes = 0
  const bodies: string[] = []
  setGeneratedFetch(async (_url, init) => {
    if (init?.method === 'DELETE') {
      deletes++
      return new Response('{"error":{"code":"cron_conflict","detail":"changed"}}', { status: 409 })
    }
    if (first) {
      first = false
      return envelope()
    }
    return envelope(definition, 'new-event')
  })
  render(CronPageWithEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'Delete job' }))
  await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete' }))
  await screen.findByRole('button', { name: 'Rebase my edits on newer version' })
  await vi.waitFor(() =>
    expect(
      (
        screen.getByRole('button', {
          name: 'Rebase my edits on newer version',
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(false),
  )
  await fireEvent.click(screen.getByRole('button', { name: 'Rebase my edits on newer version' }))
  setGeneratedFetch(async (_url, init) => {
    if (init?.method === 'DELETE') {
      deletes++
      bodies.push(String(init.body))
    }
    return envelope()
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete' }))
  await vi.waitFor(() => expect(deletes).toBe(2))
  expect(bodies[0]).toBe('{"expected_event_uid":"new-event"}')
})
test('server field errors remain visible and keep the editable draft', async () => {
  const p = writableProps()
  setGeneratedFetch(async (_url, init) =>
    init?.method === 'PUT'
      ? new Response('{"error":{"code":"invalid_cron","detail":"invalid cron expression"}}', {
          status: 422,
        })
      : envelope(),
  )
  const view = render(CronPageWithEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'Edit job' }))
  await screen.findByRole('textbox', { name: 'Prompt' })
  await fireEvent.input(screen.getByRole('textbox', { name: 'Prompt' }), {
    target: { value: 'My retained prompt' },
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
  await screen.findByRole('alert')
  await view.rerender({ ...p, mutationMessage: 'invalid cron expression' })
  expect(screen.getByRole('alert').textContent).toContain('invalid cron expression')
  expect((screen.getByRole('textbox', { name: 'Prompt' }) as HTMLInputElement).value).toBe(
    'My retained prompt',
  )
  expect((screen.getByRole('textbox', { name: 'Prompt' }) as HTMLInputElement).disabled).toBe(false)
})
test('a fenced uncertain draft can be explicitly closed without retrying its old request', async () => {
  const p = writableProps()
  let loading = true,
    puts = 0
  setGeneratedFetch(async (_url, init) => {
    if (loading) return envelope()
    if (init?.method === 'PUT') puts++
    throw new Error('offline')
  })
  const view = render(CronPageWithEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'Edit job' }))
  await screen.findByRole('textbox', { name: 'Prompt' })
  await fireEvent.input(screen.getByRole('textbox', { name: 'Prompt' }), {
    target: { value: 'My edits' },
  })
  loading = false
  await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
  await screen.findByRole('button', { name: 'Check outcome' })
  await view.rerender({ ...p, authorityScope: 'two' })
  await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(screen.queryByRole('textbox', { name: 'Prompt' })).toBeNull()
  expect(puts).toBe(1)
})
test.each(['prompt-to-workflow', 'interval-to-manual', 'per-run-to-existing'])(
  'conflict rebase retains only final active edits for %s',
  async (family) => {
    const p = writableProps(),
      workflowUID = '01ARZ3NDEKTSV4RRFFQ69G5FAW'
    p.state.projects[0]!.workflows = [
      { uid: workflowUID, name: 'Example workflow', deleted_at: null },
    ] as never
    let initial = definition.replace(
      '"options":{"big":9007199254740993}',
      '"options":{"big":9007199254740993,"input":{"n":9007199254740993},"tags":["keep"],"description":"Preserved"}',
    )
    if (family === 'interval-to-manual')
      initial = initial.replace(
        '"trigger":{"kind":"manual"}',
        '"trigger":{"kind":"interval","interval_seconds":60}',
      )
    const newer = initial.replace('"big":9007199254740993', '"big":9007199254740995'),
      bodies: string[] = []
    let reads = 0
    setGeneratedFetch(async (_url, init) => {
      if (init?.method === 'PUT') {
        bodies.push(String(init.body))
        return bodies.length === 1
          ? new Response('{"error":{"code":"cron_conflict","detail":"changed"}}', { status: 409 })
          : envelope(newer, 'new-event')
      }
      return envelope(++reads === 1 ? initial : newer, reads === 1 ? 'old-event' : 'new-event')
    })
    render(CronPageWithEditor, p)
    await fireEvent.click(screen.getByRole('button', { name: 'Edit job' }))
    await screen.findByRole('textbox', { name: 'Name' })
    const choose = async (label: string, value: string) =>
      fireEvent.change(screen.getByRole('combobox', { name: label }), { target: { value } })
    if (family === 'prompt-to-workflow') {
      await fireEvent.input(screen.getByRole('textbox', { name: 'Prompt' }), {
        target: { value: 'Dormant prompt' },
      })
      await choose('Action', 'workflow')
      await choose('Workflow', workflowUID)
    } else if (family === 'interval-to-manual') {
      await fireEvent.input(screen.getByRole('textbox', { name: 'Interval' }), {
        target: { value: '5m' },
      })
      await choose('Schedule', 'manual')
    } else {
      await fireEvent.input(screen.getByRole('textbox', { name: 'Issue title' }), {
        target: { value: 'Dormant title' },
      })
      await choose('Issue', 'existing')
      await fireEvent.input(screen.getByRole('textbox', { name: 'Existing issue' }), {
        target: { value: '01ARZ3NDEKTSV4RRFFQ69G5FAV' },
      })
    }
    await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
    const rebase = await screen.findByRole('button', { name: 'Rebase my edits on newer version' })
    await vi.waitFor(() => expect((rebase as HTMLButtonElement).disabled).toBe(false))
    await fireEvent.click(rebase)
    await vi.waitFor(() =>
      expect(screen.queryByRole('region', { name: 'Definition conflict' })).toBeNull(),
    )
    expect(bodies).toHaveLength(1)
    // A dormant value remains an editor buffer, not an active serialized field.
    if (family === 'prompt-to-workflow') {
      await choose('Action', 'prompt')
      expect((screen.getByRole('textbox', { name: 'Prompt' }) as HTMLInputElement).value).toBe(
        'Dormant prompt',
      )
      await choose('Action', 'workflow')
    } else if (family === 'interval-to-manual') {
      await choose('Schedule', 'interval')
      expect((screen.getByRole('textbox', { name: 'Interval' }) as HTMLInputElement).value).toBe(
        '300',
      )
      await choose('Schedule', 'manual')
    } else {
      await choose('Issue', 'per-run')
      expect((screen.getByRole('textbox', { name: 'Issue title' }) as HTMLInputElement).value).toBe(
        'Dormant title',
      )
      await choose('Issue', 'existing')
    }
    await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
    await vi.waitFor(() => expect(bodies).toHaveLength(2))
    const saved = parseCronDocument(bodies[1]!)
    expect(rawCronValue(saved, ['expected_event_uid'])).toBe('"new-event"')
    expect(rawCronValue(saved, ['definition', 'options'])).toBe(
      rawCronValue(parseCronDocument(newer), ['options']),
    )
    if (family === 'prompt-to-workflow') {
      expect(rawCronValue(saved, ['definition', 'action', 'prompt'])).toBeUndefined()
      expect(rawCronValue(saved, ['definition', 'action', 'workflow_uid'])).toBe(
        JSON.stringify(workflowUID),
      )
    } else if (family === 'interval-to-manual') {
      expect(rawCronValue(saved, ['definition', 'trigger', 'kind'])).toBe('"manual"')
      expect(rawCronValue(saved, ['definition', 'trigger', 'interval_seconds'])).toBeUndefined()
    } else {
      expect(rawCronValue(saved, ['definition', 'issue', 'kind'])).toBe('"existing"')
      expect(rawCronValue(saved, ['definition', 'issue', 'title'])).toBeUndefined()
      expect(rawCronValue(saved, ['definition', 'issue', 'uid'])).toBe(
        '"01ARZ3NDEKTSV4RRFFQ69G5FAV"',
      )
    }
  },
)
// Exact existing native acceptance shape from internal/cron/validate_test.go.
const triggerNotify =
  '{"version":1,"kind":"job","trigger":{"kind":"issue-deadline","issue_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","lead_seconds":1800},"action":{"kind":"notify","recipient":"operator/worker","message":"Due soon"},"overlap":"forbid","catchup":"latest"}'
test.each([
  ['Name', 'Renamed', ['name'], '"Renamed"'],
  ['Message', 'Due shortly', ['definition', 'action', 'message'], '"Due shortly"'],
  ['Grace', '2m', ['definition', 'grace_seconds'], '120'],
  ['Lead', '1h', ['definition', 'trigger', 'lead_seconds'], '3600'],
] as const)(
  'native trigger-targeted notify accepts a %s edit without adding issue policy',
  async (label, value, path, raw) => {
    const bodies: string[] = []
    setGeneratedFetch(async (_url, init) => {
      if (init?.method === 'PUT') bodies.push(String(init.body))
      return envelope(triggerNotify)
    })
    render(CronPageWithEditor, writableProps())
    await fireEvent.click(screen.getByRole('button', { name: 'Edit job' }))
    await screen.findByRole('textbox', { name: label })
    await fireEvent.input(screen.getByRole('textbox', { name: label }), { target: { value } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
    await vi.waitFor(() => expect(bodies).toHaveLength(1))
    const saved = parseCronDocument(bodies[0]!)
    expect(rawCronValue(saved, path)).toBe(raw)
    expect(rawCronValue(saved, ['definition', 'issue'])).toBeUndefined()
    expect(rawCronValue(saved, ['definition', 'trigger', 'issue_uid'])).toBe(
      '"01ARZ3NDEKTSV4RRFFQ69G5FAV"',
    )
  },
)

test('a linked issue that moved projects opens in its current project', async () => {
  const onOpenIssue = vi.fn()
  const state = emptyCronState()
  const run = {
    uid: 'run',
    status: 'failed',
    issue_uid: 'issue',
    created_at: '2026-01-01',
    summary: { version: 1 },
  } as never
  state.projects = [
    {
      project: { id: 1, uid: 'p1', name: 'example-project' },
      hasRecords: true,
      jobs: [],
      workflows: [],
      runs: [run],
    },
    {
      project: { id: 2, uid: 'p2', name: 'example-inbox' },
      hasRecords: false,
      jobs: [],
      workflows: [],
      runs: [],
    },
  ]
  state.selected = { kind: 'run', projectUID: 'p1', uid: 'run' }
  state.detail = run
  state.peek = {
    status: 'empty',
    issue: {
      issue: { uid: 'issue', project_id: 2, short_id: 'abc4', title: 'Moved issue' },
      comments: [],
    } as never,
  }
  render(CronPage, {
    route: { ...defaultCronRoute(), tab: 'runs' },
    state,
    onNavigate: vi.fn(),
    onLoadMore: vi.fn(),
    onRefresh: vi.fn(),
    onOpenIssue,
  })
  await fireEvent.click(screen.getByRole('button', { name: 'abc4 · Moved issue' }))
  expect(onOpenIssue).toHaveBeenLastCalledWith('p2', 'issue')
  await fireEvent.click(screen.getByRole('button', { name: 'issue' }))
  expect(onOpenIssue).toHaveBeenLastCalledWith('p2', 'issue')
})

test.each([
  { tab: 'jobs', field: 'jobsError', message: 'Jobs refresh unavailable' },
  { tab: 'workflows', field: 'workflowsError', message: 'Workflows refresh unavailable' },
  { tab: 'runs', field: 'historyError', message: 'History refresh unavailable' },
] as const)('a failed $tab refresh is reported on the $tab tab', ({ tab, field, message }) => {
  const state = emptyCronState()
  state.projects = [
    {
      project: { id: 1, uid: 'p1', name: 'example-project' },
      jobs: [],
      runs: [],
      workflows: [],
      hasRecords: true,
      [field]: message,
    },
  ]
  render(CronPage, {
    route: { ...defaultCronRoute(), tab },
    state,
    onNavigate: vi.fn(),
    onLoadMore: vi.fn(),
    onRefresh: vi.fn(),
    onOpenIssue: vi.fn(),
  })
  expect(screen.getByText(`example-project: ${message}`)).toBeTruthy()
})

const lateReads = [
  { kind: 'job', change: 'closed' },
  { kind: 'job', change: 'changed' },
  { kind: 'workflow', change: 'closed' },
  { kind: 'workflow', change: 'changed' },
] as const
test.each(lateReads)(
  'a $kind definition read cannot open an editor after the selection $change',
  async ({ kind, change }) => {
    const p = kind === 'job' ? writableProps() : workflowProps()
    let release!: () => void
    const held = new Promise<void>((resolve) => (release = resolve))
    let requested = false
    setGeneratedFetch(async () => {
      requested = true
      await held
      return kind === 'job' ? envelope() : workflowEnvelope()
    })
    const view = render(CronPageWithEditor, p)
    await fireEvent.click(screen.getByRole('button', { name: `Edit ${kind}` }))
    await vi.waitFor(() => expect(requested).toBe(true))
    const state = { ...p.state, detail: undefined }
    state.selected = change === 'closed' ? undefined : { kind, projectUID: 'p1', uid: 'other' }
    await view.rerender({ ...p, state })
    release()
    await new Promise((resolve) => setTimeout(resolve, 0))
    await tick()
    expect(
      screen.queryByRole('form', { name: kind === 'job' ? 'Job editor' : 'Workflow editor' }),
    ).toBeNull()
  },
)

test('dismissing a job during its definition read cancels it and frees the controls', async () => {
  const p = writableProps()
  let signal: AbortSignal | undefined
  setGeneratedFetch(async (_url, init) => {
    signal = init?.signal ?? undefined
    return new Promise<Response>(() => {})
  })
  const view = render(CronPageWithEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'Edit job' }))
  await vi.waitFor(() => expect(signal).toBeDefined())
  expect((screen.getByRole('button', { name: 'New job' }) as HTMLButtonElement).disabled).toBe(true)
  await view.rerender({ ...p, state: { ...p.state, selected: undefined, detail: undefined } })
  expect(signal?.aborted).toBe(true)
  await vi.waitFor(() =>
    expect((screen.getByRole('button', { name: 'New job' }) as HTMLButtonElement).disabled).toBe(
      false,
    ),
  )
})

test('a cancelled definition read cannot clear a newer read', async () => {
  const p = writableProps()
  const held: (() => void)[] = []
  setGeneratedFetch(() => new Promise<Response>((resolve) => held.push(() => resolve(envelope()))))
  const view = render(CronPageWithEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'Edit job' }))
  await vi.waitFor(() => expect(held).toHaveLength(1))
  const second = {
    ...p.state,
    selected: { kind: 'job' as const, projectUID: 'p1', uid: 'job2' },
    detail: { ...(p.state.detail as CronJob), uid: 'job2' },
  }
  await view.rerender({ ...p, state: second })
  await fireEvent.click(screen.getByRole('button', { name: 'Edit job' }))
  await vi.waitFor(() => expect(held).toHaveLength(2))
  held[0]!()
  await new Promise((resolve) => setTimeout(resolve, 0))
  await tick()
  expect((screen.getByRole('button', { name: 'New job' }) as HTMLButtonElement).disabled).toBe(true)
  expect(screen.queryByRole('form', { name: 'Job editor' })).toBeNull()
  held[1]!()
  expect(await screen.findByRole('form', { name: 'Job editor' })).toBeTruthy()
})

test('a delete confirmed by Check outcome closes the deleted job', async () => {
  const p = writableProps()
  let gets = 0
  setGeneratedFetch(async (_url, init) => {
    if (init?.method === 'DELETE') throw new Error('connection lost')
    gets++
    if (gets === 1) return envelope()
    if (gets === 2) throw new Error('connection lost')
    return new Response(
      `{"job":{"uid":"job1","name":"Example job","definition_event_uid":"delete-event","deleted_at":"2026-10-25T00:00:00Z","definition":${definition}}}`,
      { headers: { 'Content-Type': 'application/json' } },
    )
  })
  render(CronPageWithEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'Delete job' }))
  await fireEvent.click(screen.getByRole('button', { name: 'Confirm delete' }))
  await fireEvent.click(await screen.findByRole('button', { name: 'Check outcome' }))
  await vi.waitFor(() => expect(p.onRefresh).toHaveBeenCalled())
  expect(p.onNavigate).toHaveBeenCalledWith(expect.objectContaining({ selection: undefined }))
})
