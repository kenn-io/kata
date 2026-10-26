import { randomUUID } from 'node:crypto'
import type { Page, Request, TestInfo } from '@playwright/test'
import { expect, test, type BrowserCredentials, type KataFixture } from './fixtures'

test.setTimeout(60_000)

// These scenarios exercise the native daemon and its persisted definitions.
// Routes below only inject transport/authority failures; they never fake a save.
interface DefinitionRecord {
  uid: string
  name: string
  definition_event_uid: string
  deleted_at?: string
  definition: {
    enabled?: boolean
    action?: Record<string, unknown>
    issue?: Record<string, unknown>
    overlap?: string
    about?: string
    steps?: Array<Record<string, unknown>>
  }
}

const jobDefinition = {
  version: 1,
  kind: 'job',
  enabled: true,
  trigger: { kind: 'manual' },
  action: { kind: 'execute', prompt: 'Inspect the example workspace.' },
  issue: { kind: 'per-run', title: 'Inspection' },
  overlap: 'forbid',
  catchup: 'all',
}

function uid() {
  // Native ULID syntax, independent of the UI's UID generator.
  return `01${randomUUID().replaceAll('-', '').slice(0, 24).toUpperCase()}`
}

function endpoint(kata: KataFixture, kind: 'job' | 'workflow', id?: string) {
  return `/api/v1/projects/${kata.projectID}/cron/${kind}s${id ? `/${id}` : ''}`
}

async function read(
  kata: KataFixture,
  page: Page,
  credentials: BrowserCredentials,
  kind: 'job' | 'workflow',
  id: string,
) {
  const response = await kata.request(page, credentials, 'GET', endpoint(kata, kind, id))
  expect(response.ok(), await response.text()).toBe(true)
  return ((await response.json()) as Record<string, DefinitionRecord>)[kind]!
}

async function seed(
  kata: KataFixture,
  page: Page,
  credentials: BrowserCredentials,
  kind: 'job' | 'workflow',
  name: string,
  definition: unknown = jobDefinition,
  rawDefinition?: string,
) {
  const id = uid()
  const body = `{"actor":"user-a","name":${JSON.stringify(name)},"definition":${rawDefinition ?? JSON.stringify(definition)}}`
  const response = await kata.request(page, credentials, 'PUT', endpoint(kata, kind, id), body, {
    'Content-Type': 'application/json',
  })
  expect(response.ok(), await response.text()).toBe(true)
  return ((await response.json()) as Record<string, DefinitionRecord>)[kind]!
}

function writes(page: Page) {
  const requests: Request[] = []
  page.on('request', (request) => {
    if (
      /\/cron\/(jobs|workflows)\//.test(request.url()) &&
      ['PUT', 'DELETE'].includes(request.method())
    )
      requests.push(request)
  })
  return requests
}

async function openCron(page: Page, kata: KataFixture, tab = 'jobs') {
  await page.goto(`${kata.origin}/kata?view=cron&cron_tab=${tab}&cron_project=${kata.projectUID}`)
  await expect(page.getByRole('heading', { name: 'Cron', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Refresh', exact: true })).toBeEnabled()
}

async function editJob(page: Page, name: string) {
  await page.getByRole('button', { name, exact: true }).click()
  await page.getByRole('button', { name: 'Edit job', exact: true }).click()
  await expect(page.getByRole('form', { name: 'Job editor' })).toBeVisible()
}

async function newJob(page: Page, kata: KataFixture, name: string) {
  await page.getByRole('button', { name: 'New job', exact: true }).click()
  await page.getByRole('combobox', { name: 'Project', exact: true }).selectOption(kata.projectUID)
  const editor = page.getByRole('form', { name: 'Job editor' })
  await editor.getByLabel('Name', { exact: true }).fill(name)
  await editor.getByLabel('Prompt', { exact: true }).fill('Inspect the example workspace.')
  await editor.getByLabel('Issue title', { exact: true }).fill('Inspection')
  return editor
}

async function screenshot(page: Page, info: TestInfo, name: string) {
  await page.screenshot({ path: info.outputPath(`${name}.png`), fullPage: true })
}

test('native job create, edit, pause and delete send the recorded definition guard', async ({
  page,
  kata,
}, info) => {
  const credentials = await kata.launch(page)
  await page.setViewportSize({ width: 1440, height: 1000 })
  await openCron(page, kata)
  const requests = writes(page)
  const name = `Example job ${uid()}`
  const editor = await newJob(page, kata, name)
  await screenshot(page, info, 'job-editor-desktop')
  await page.setViewportSize({ width: 390, height: 844 })
  await screenshot(page, info, 'job-editor-mobile')
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(
    true,
  )
  await editor.getByRole('button', { name: 'Save job', exact: true }).click()
  await expect(editor).toHaveCount(0)
  expect(requests).toHaveLength(1)
  const id = requests[0]!.url().split('/').at(-1)!
  expect(requests[0]!.postDataJSON()).not.toHaveProperty('expected_event_uid')
  const created = await read(kata, page, credentials, 'job', id)
  expect(created.name).toBe(name)
  expect(created.definition.action?.prompt).toBe('Inspect the example workspace.')
  await page.reload()
  await editJob(page, name)
  const renamed = `${name} revised`
  await page.getByLabel('Name', { exact: true }).fill(renamed)
  await page.getByRole('button', { name: 'Save job', exact: true }).click()
  await expect(editor).toHaveCount(0)
  expect(requests).toHaveLength(2)
  expect(requests[1]!.postDataJSON().expected_event_uid).toBe(created.definition_event_uid)
  const edited = await read(kata, page, credentials, 'job', id)
  expect(edited.name).toBe(renamed)
  await page.reload()
  const detail = page.getByRole('dialog', { name: 'Job detail', exact: true })
  await expect(detail.getByRole('heading', { name: renamed, exact: true })).toBeVisible()
  await detail.getByRole('button', { name: 'Pause', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Unpause', exact: true })).toBeVisible()
  expect(requests).toHaveLength(3)
  expect(requests[2]!.postDataJSON().expected_event_uid).toBe(edited.definition_event_uid)
  const paused = await read(kata, page, credentials, 'job', id)
  expect(paused.definition.enabled).toBe(false)
  await page.getByRole('button', { name: 'Delete job', exact: true }).click()
  expect(requests).toHaveLength(3)
  await page.getByRole('button', { name: 'Confirm delete', exact: true }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  expect(requests).toHaveLength(4)
  expect(requests[3]!.method()).toBe('DELETE')
  expect(requests[3]!.postDataJSON().expected_event_uid).toBe(paused.definition_event_uid)
  expect((await read(kata, page, credentials, 'job', id)).deleted_at).toBeTruthy()
  await page.reload()
  await expect(page.getByRole('button', { name: renamed, exact: true })).toHaveCount(0)
})

test('editing a job preserves an opaque integer in real request and native readback bytes', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const definition = JSON.stringify(jobDefinition).replace(
    /}$/,
    ',"options":{"opaque":{"counter":9007199254740993},"tags":["inspection"]}}',
  )
  const record = await seed(
    kata,
    page,
    credentials,
    'job',
    `Exact job ${uid()}`,
    undefined,
    definition,
  )
  await openCron(page, kata)
  const requests = writes(page)
  await editJob(page, record.name)
  await page.getByLabel('Prompt', { exact: true }).fill('Inspect again.')
  await page.getByRole('button', { name: 'Save job', exact: true }).click()
  await expect(page.getByRole('form', { name: 'Job editor' })).toHaveCount(0)
  expect(requests).toHaveLength(1)
  expect(requests[0]!.postData()).toMatch(/"counter"\s*:\s*9007199254740993\b/)
  const response = await kata.request(page, credentials, 'GET', endpoint(kata, 'job', record.uid))
  expect(response.ok()).toBe(true)
  expect(await response.text()).toMatch(/"counter"\s*:\s*9007199254740993\b/)
  await page.reload()
  const detail = page.getByRole('dialog', { name: 'Job detail', exact: true })
  await expect(detail.getByRole('heading', { name: record.name, exact: true })).toBeVisible()
  await detail.getByRole('button', { name: 'Edit job', exact: true }).click()
  await expect(page.getByRole('form', { name: 'Job editor' })).toBeVisible()
  await expect(page.getByLabel('Prompt', { exact: true })).toHaveValue('Inspect again.')
  await expect(page.getByRole('form', { name: 'Job editor' })).toContainText('9007199254740993')
})

test('native recurring edits retain immutable ownership and parent fields', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const parent = await kata.seedIssue(page, credentials, { title: 'Example recurring parent' })
  const definition = {
    ...jobDefinition,
    enabled: false,
    trigger: { kind: 'interval', interval_seconds: 7200, timezone: 'UTC' },
    action: { kind: 'notify', recipient: 'current-owner-or-author' },
    issue: {
      kind: 'per-run',
      parent_uid: parent.uid,
      title: 'Inspect service',
      body: 'Keep this body.',
    },
    overlap: 'allow',
  }
  const record = await seed(kata, page, credentials, 'job', `Recurring job ${uid()}`, definition)
  await openCron(page, kata)
  const requests = writes(page)
  await editJob(page, record.name)
  const editor = page.getByRole('form', { name: 'Job editor' })
  await expect(editor.getByRole('combobox', { name: 'Action', exact: true })).toHaveCount(0)
  await expect(editor.getByLabel('Parent issue', { exact: true })).toHaveCount(0)
  await expect(editor).toContainText(parent.uid)
  await editor.getByLabel('Child title', { exact: true }).fill('Revised inspection')
  await editor.getByRole('button', { name: 'Save job', exact: true }).click()
  await expect(editor).toHaveCount(0)
  expect(requests).toHaveLength(1)
  const stored = await read(kata, page, credentials, 'job', record.uid)
  expect(stored.definition.action).toEqual(definition.action)
  expect(stored.definition.issue).toEqual({ ...definition.issue, title: 'Revised inspection' })
  expect(stored.definition.overlap).toBe('allow')
  await page.reload()
  const detail = page.getByRole('dialog', { name: 'Job detail', exact: true })
  await expect(detail.getByRole('heading', { name: record.name, exact: true })).toBeVisible()
  await detail.getByRole('button', { name: 'Edit job', exact: true }).click()
  await expect(editor).toBeVisible()
  await expect(page.getByLabel('Child title', { exact: true })).toHaveValue('Revised inspection')
  await expect(editor).toContainText(parent.uid)
})

test('workflow graph validates ordering, rewrites renamed keys and confirms dependent deletion before native save', async ({
  page,
  kata,
}, info) => {
  const credentials = await kata.launch(page)
  await openCron(page, kata, 'workflows')
  const requests = writes(page)
  await page.getByRole('button', { name: 'New workflow', exact: true }).click()
  await page.getByRole('combobox', { name: 'Project', exact: true }).selectOption(kata.projectUID)
  const editor = page.getByRole('form', { name: 'Workflow editor' })
  const name = `Example workflow ${uid()}`
  await editor.getByLabel('Name', { exact: true }).fill(name)
  await editor.getByLabel('About', { exact: true }).fill('Inspect and summarize')
  await editor.getByLabel('Input', { exact: true }).fill('Example workspace')
  await editor.getByLabel('Workflow options', { exact: true }).fill('{"counter":9007199254740993}')
  await editor.getByRole('button', { name: 'Add command step', exact: true }).click()
  const first = editor.getByRole('group', { name: /^Step / }).nth(0)
  await first.getByLabel('Key', { exact: true }).fill('inspect')
  await first.getByLabel('Command', { exact: true }).fill('echo inspection')
  await first.getByLabel('Step options', { exact: true }).fill('{"counter":9007199254740993}')
  await editor.getByRole('button', { name: 'Add prompt step', exact: true }).click()
  const second = editor.getByRole('group', { name: /^Step / }).nth(1)
  await second.getByLabel('Key', { exact: true }).fill('summarize')
  await second.getByLabel('Prompt', { exact: true }).fill('Summarize the inspection.')
  await second.getByLabel('Runs after', { exact: true }).fill('inspect')
  const graph = page.getByRole('region', { name: 'Workflow dependency graph' })
  await expect(graph.locator('.svelte-flow__node')).toHaveCount(2)
  await expect(graph.locator('.svelte-flow__edge')).toHaveCount(1)
  await first.getByLabel('Runs after', { exact: true }).fill('summarize')
  await expect(
    first.getByRole('alert').filter({ hasText: /must name an earlier step/ }),
  ).toBeVisible()
  await editor.getByRole('button', { name: 'Save workflow', exact: true }).click()
  expect(requests).toHaveLength(0)
  await first.getByLabel('Runs after', { exact: true }).fill('')
  await first.getByLabel('Key', { exact: true }).fill('review')
  await expect(second.getByLabel('Runs after', { exact: true })).toHaveValue('review')
  await expect(
    graph.locator('.svelte-flow__node').filter({ hasText: /^\s*review\s*$/ }),
  ).toHaveCount(1)
  await page.setViewportSize({ width: 1440, height: 1000 })
  await screenshot(page, info, 'workflow-editor-desktop')
  await page.setViewportSize({ width: 390, height: 844 })
  await screenshot(page, info, 'workflow-editor-mobile')
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(
    true,
  )
  await editor.getByRole('button', { name: 'Save workflow', exact: true }).click()
  await expect(editor).toHaveCount(0)
  expect(requests).toHaveLength(1)
  const id = requests[0]!.url().split('/').at(-1)!
  expect(requests[0]!.postData()?.match(/9007199254740993/g)).toHaveLength(2)
  let stored = await read(kata, page, credentials, 'workflow', id)
  expect(stored.definition.steps).toMatchObject([
    { key: 'review', kind: 'command', command: 'echo inspection' },
    { key: 'summarize', kind: 'prompt', after: ['review'] },
  ])
  await page.reload()
  await page.getByRole('button', { name, exact: true }).click()
  await page.getByRole('button', { name: 'Edit workflow', exact: true }).click()
  await expect(
    editor
      .getByRole('group', { name: /^Step / })
      .nth(1)
      .getByLabel('Runs after', { exact: true }),
  ).toHaveValue('review')
  await editor
    .getByRole('group', { name: /^Step / })
    .nth(0)
    .getByRole('button', { name: 'Delete step', exact: true })
    .click()
  await expect(editor.getByRole('region', { name: 'Confirm step deletion' })).toContainText(
    'summarize',
  )
  await expect(editor.getByRole('group', { name: /^Step / })).toHaveCount(2)
  await expect(graph.locator('.svelte-flow__edge')).toHaveCount(1)
  expect(requests).toHaveLength(1)
  await editor.getByRole('button', { name: 'Keep step', exact: true }).click()
  await expect(editor.getByRole('group', { name: /^Step / })).toHaveCount(2)
  await editor
    .getByRole('group', { name: /^Step / })
    .nth(0)
    .getByRole('button', { name: 'Delete step', exact: true })
    .click()
  await editor
    .getByRole('button', { name: 'Delete step and remove references', exact: true })
    .click()
  await expect(editor.getByRole('group', { name: /^Step / })).toHaveCount(1)
  await expect(
    editor.getByRole('group', { name: /^Step / }).getByLabel('Runs after', { exact: true }),
  ).toHaveValue('')
  await expect(graph.locator('.svelte-flow__node')).toHaveCount(1)
  await expect(graph.locator('.svelte-flow__edge')).toHaveCount(0)
  await editor.getByLabel('About', { exact: true }).fill('Only summarize')
  const priorGuard = stored.definition_event_uid
  await editor.getByRole('button', { name: 'Save workflow', exact: true }).click()
  await expect(editor).toHaveCount(0)
  expect(requests).toHaveLength(2)
  expect(requests[1]!.postDataJSON().expected_event_uid).toBe(priorGuard)
  expect(requests[1]!.postData()).toMatch(/"counter"\s*:\s*9007199254740993\b/)
  stored = await read(kata, page, credentials, 'workflow', id)
  expect(stored.definition.about).toBe('Only summarize')
  expect(stored.definition.steps).toHaveLength(1)
  expect(stored.definition.steps![0]).toMatchObject({ key: 'summarize', kind: 'prompt' })
  expect(stored.definition.steps![0]!.after ?? []).toEqual([])
  const response = await kata.request(page, credentials, 'GET', endpoint(kata, 'workflow', id))
  expect(await response.text()).toMatch(/"counter"\s*:\s*9007199254740993\b/)
  await page.reload()
  const detail = page.getByRole('dialog', { name: 'Workflow detail', exact: true })
  await expect(detail.getByRole('heading', { name, exact: true })).toBeVisible()
  await detail.getByRole('button', { name: 'Edit workflow', exact: true }).click()
  await expect(editor.getByLabel('About', { exact: true })).toHaveValue('Only summarize')
  await expect(editor.getByRole('group', { name: /^Step / })).toHaveCount(1)
})

test('a real 409 retains the job draft and requires explicit rebase onto the newer definition', async ({
  page,
  kata,
}, info) => {
  const credentials = await kata.launch(page)
  const record = await seed(kata, page, credentials, 'job', `Conflict job ${uid()}`)
  await openCron(page, kata)
  const requests = writes(page)
  await editJob(page, record.name)
  await page.getByLabel('Prompt', { exact: true }).fill('Retained local prompt')
  const concurrent = await kata.request(
    page,
    credentials,
    'PUT',
    endpoint(kata, 'job', record.uid),
    {
      actor: 'user-a',
      name: `${record.name} newer`,
      definition: { ...jobDefinition, timeout_seconds: 120 },
      expected_event_uid: record.definition_event_uid,
    },
  )
  expect(concurrent.ok(), await concurrent.text()).toBe(true)
  const newer = ((await concurrent.json()) as { job: DefinitionRecord }).job
  const conflictResponse = page.waitForResponse(
    (response) =>
      response.request().method() === 'PUT' && response.url().endsWith(`/jobs/${record.uid}`),
  )
  await page.getByRole('button', { name: 'Save job', exact: true }).click()
  expect((await conflictResponse).status()).toBe(409)
  const conflict = page.getByRole('region', { name: 'Definition conflict' })
  await expect(conflict).toContainText('Retained local prompt')
  await expect(conflict).toContainText(`Newer version · ${newer.name}`)
  await expect(page.getByLabel('Prompt', { exact: true })).toHaveValue('Retained local prompt')
  await expect(page.getByRole('button', { name: 'Save job', exact: true })).toBeDisabled()
  await page.setViewportSize({ width: 1440, height: 1000 })
  await screenshot(page, info, 'job-conflict-desktop')
  await page.setViewportSize({ width: 390, height: 844 })
  await screenshot(page, info, 'job-conflict-mobile')
  expect(requests).toHaveLength(1)
  await page.getByRole('button', { name: 'Rebase my edits on newer version', exact: true }).click()
  await expect(page.getByLabel('Timeout', { exact: true })).toHaveValue('120')
  await page.getByRole('button', { name: 'Save job', exact: true }).click()
  await expect(page.getByRole('form', { name: 'Job editor' })).toHaveCount(0)
  expect(requests).toHaveLength(2)
  expect(requests[1]!.postDataJSON().expected_event_uid).toBe(newer.definition_event_uid)
  const saved = await read(kata, page, credentials, 'job', record.uid)
  expect(saved.name).toBe(newer.name)
  expect(saved.definition.action?.prompt).toBe('Retained local prompt')
  await page.reload()
  const detail = page.getByRole('dialog', { name: 'Job detail', exact: true })
  await expect(detail.getByRole('heading', { name: newer.name, exact: true })).toBeVisible()
  await detail.getByRole('button', { name: 'Edit job', exact: true }).click()
  await expect(page.getByRole('form', { name: 'Job editor' })).toBeVisible()
  await expect(page.getByLabel('Prompt', { exact: true })).toHaveValue('Retained local prompt')
  await expect(page.getByLabel('Timeout', { exact: true })).toHaveValue('120')
})

for (const loseReadback of [false, true])
  test(`a committed create with a dropped response ${loseReadback ? 'retains an uncertain draft until checked' : 'reads back automatically'} without a second entity`, async ({
    page,
    kata,
  }) => {
    const credentials = await kata.launch(page)
    await openCron(page, kata)
    const requests = writes(page)
    let createdUID = ''
    let droppedReads = 0
    await page.route('**/cron/jobs/*', async (route) => {
      const request = route.request()
      if (request.method() === 'PUT') {
        createdUID = request.url().split('/').at(-1)!
        const response = await route.fetch()
        expect(response.ok(), await response.text()).toBe(true)
        await route.abort('connectionreset')
      } else if (loseReadback && createdUID && request.method() === 'GET' && !droppedReads++) {
        await route.abort('connectionreset')
      } else await route.continue()
    })
    const name = `Uncertain job ${uid()}`
    const editor = await newJob(page, kata, name)
    await editor.getByRole('button', { name: 'Save job', exact: true }).click()
    if (loseReadback) {
      await expect(page.getByRole('region', { name: 'Uncertain write' })).toBeVisible()
      await expect(editor.getByLabel('Name', { exact: true })).toHaveValue(name)
      await expect(editor.getByRole('button', { name: 'Save job', exact: true })).toBeDisabled()
      await page.keyboard.press('Escape')
      await expect(editor).toBeVisible()
      await page.getByRole('button', { name: 'Check outcome', exact: true }).click()
    }
    await expect(editor).toHaveCount(0)
    expect(requests).toHaveLength(1)
    const response = await kata.request(page, credentials, 'GET', endpoint(kata, 'job'))
    expect(response.ok()).toBe(true)
    const jobs = ((await response.json()) as { jobs: DefinitionRecord[] }).jobs.filter(
      (job) => job.name === name,
    )
    expect(jobs).toHaveLength(1)
    expect(jobs[0]!.uid).toBe(createdUID)
    await page.reload()
    await expect(page.getByRole('button', { name, exact: true })).toHaveCount(1)
  })

test('pending authority disables editor changes and duplicate submits while one native write is in flight', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  await openCron(page, kata)
  const requests = writes(page)
  let release!: () => void
  const held = new Promise<void>((resolve) => {
    release = resolve
  })
  let intercepted = false
  await page.route('**/cron/jobs/*', async (route) => {
    if (route.request().method() !== 'PUT') return route.continue()
    intercepted = true
    await held
    await route.continue()
  })
  const name = `Pending job ${uid()}`
  const editor = await newJob(page, kata, name)
  try {
    await editor.getByRole('button', { name: 'Save job', exact: true }).click()
    await expect.poll(() => intercepted).toBe(true)
    await expect(
      editor.getByRole('button', { name: 'Resolving references…', exact: true }),
    ).toBeDisabled()
    await expect(editor.getByRole('button', { name: 'Cancel', exact: true })).toBeDisabled()
    await expect(editor.getByLabel('Prompt', { exact: true })).toBeDisabled()
    await expect(page.getByRole('button', { name: 'New job', exact: true })).toBeDisabled()
    await editor.evaluate((form: HTMLFormElement) => {
      form.requestSubmit()
      form.requestSubmit()
    })
    expect(requests).toHaveLength(1)
  } finally {
    release()
  }
  await expect(editor).toHaveCount(0)
  const id = requests[0]!.url().split('/').at(-1)!
  expect((await read(kata, page, credentials, 'job', id)).name).toBe(name)
  expect(requests).toHaveLength(1)
})

test('401 rejection retains and fences the old draft after local session renewal', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const record = await seed(kata, page, credentials, 'job', `Authority job ${uid()}`)
  await openCron(page, kata)
  const requests = writes(page)
  await editJob(page, record.name)
  await page.getByLabel('Prompt', { exact: true }).fill('Draft before authentication rejection')
  await page.route('**/cron/jobs/*', async (route) => {
    if (route.request().method() === 'PUT')
      await route.fulfill({
        status: 401,
        headers: { 'X-Kata-Web-Authentication': 'loopback' },
        json: { error: { code: 'unauthorized', detail: 'Session expired' } },
      })
    else await route.continue()
  })
  const renewal = page.waitForResponse(
    (response) =>
      new URL(response.url()).pathname === '/api/v1/ui/session/local' &&
      response.request().method() === 'POST' &&
      response.status() === 200,
  )
  await page.getByRole('button', { name: 'Save job', exact: true }).click()
  const renewedCredentials = (await (await renewal).json()) as BrowserCredentials
  expect(renewedCredentials.session).not.toBe(credentials.session)
  await expect
    .poll(() =>
      page.evaluate(() => {
        const stored = sessionStorage.getItem('kata.web.session.v1')
        return stored ? (JSON.parse(stored) as { session?: string }).session : undefined
      }),
    )
    .toBe(renewedCredentials.session)
  const renewedSnapshot = await kata.snapshot(page, renewedCredentials)
  expect((renewedSnapshot.capabilities as { writable: boolean }).writable).toBe(true)
  await expect(page.getByRole('button', { name: 'New project', exact: true })).toBeEnabled()
  await expect(
    page.getByText('Authority changed. Close this draft and reopen the definition before saving.'),
  ).toBeVisible()
  const editor = page.getByRole('form', { name: 'Job editor' })
  await expect(editor.getByLabel('Prompt', { exact: true })).toHaveValue(
    'Draft before authentication rejection',
  )
  await expect(editor.getByRole('button', { name: 'Save job', exact: true })).toBeDisabled()
  await editor.evaluate((form: HTMLFormElement) => form.requestSubmit())
  expect(requests).toHaveLength(1)
  // Reload reuses the renewed tab session and reads the unchanged native state.
  await page.reload()
  const detail = page.getByRole('dialog', { name: 'Job detail', exact: true })
  await expect(detail.getByRole('heading', { name: record.name, exact: true })).toBeVisible()
  await detail.getByRole('button', { name: 'Edit job', exact: true }).click()
  await expect(editor).toBeVisible()
  await expect(page.getByLabel('Prompt', { exact: true })).toHaveValue(
    'Inspect the example workspace.',
  )
  const currentCredentials = (await page.evaluate(() =>
    JSON.parse(sessionStorage.getItem('kata.web.session.v1')!),
  )) as BrowserCredentials
  expect((await read(kata, page, currentCredentials, 'job', record.uid)).definition_event_uid).toBe(
    record.definition_event_uid,
  )
  expect(requests).toHaveLength(1)
})

test('daemon switching fences an open job draft and never sends it to either daemon', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const record = await seed(kata, page, credentials, 'job', `Switch job ${uid()}`)
  await openCron(page, kata)
  const requests = writes(page)
  await editJob(page, record.name)
  await page.getByLabel('Prompt', { exact: true }).fill('Draft for the original daemon')
  // Trigger the real switcher while the drawer owns pointer/focus trapping.
  // This models an authority change arriving while the draft stays open.
  await page
    .getByRole('button', { name: 'Switch Kata daemon: example-local' })
    .dispatchEvent('click')
  await page.getByRole('menuitemradio', { name: /example-remote/ }).dispatchEvent('click')
  await expect(
    page.getByRole('button', { name: 'Switch Kata daemon: example-remote' }),
  ).toBeVisible()
  const editor = page.getByRole('form', { name: 'Job editor' })
  await expect(
    page.getByText('Authority changed. Close this draft and reopen the definition before saving.'),
  ).toBeVisible()
  await expect(editor.getByLabel('Prompt', { exact: true })).toHaveValue(
    'Draft for the original daemon',
  )
  await expect(editor.getByRole('button', { name: 'Save job', exact: true })).toBeDisabled()
  await editor.evaluate((form: HTMLFormElement) => form.requestSubmit())
  expect(requests).toHaveLength(0)
  expect((await read(kata, page, credentials, 'job', record.uid)).definition_event_uid).toBe(
    record.definition_event_uid,
  )
})

test('read-only authority offers no job or workflow mutation controls and rejects native writes', async ({
  page,
  kata,
}) => {
  await kata.restartReadonly()
  try {
    const requests = writes(page)
    await openCron(page, kata)
    await expect(page.getByRole('status', { name: 'Active connection' })).toContainText(
      'Credential: Read-only',
    )
    await expect(page.getByRole('button', { name: 'New job', exact: true })).toHaveCount(0)
    await page.getByRole('radio', { name: 'Workflows', exact: true }).click()
    await expect(page.getByRole('button', { name: 'New workflow', exact: true })).toHaveCount(0)
    const response = await page.request.put(`${kata.origin}${endpoint(kata, 'job', uid())}`, {
      headers: { Origin: kata.origin },
      data: { name: 'Forbidden job', definition: jobDefinition },
    })
    expect([401, 403]).toContain(response.status())
    expect(requests).toHaveLength(0)
  } finally {
    await kata.restart()
  }
})
