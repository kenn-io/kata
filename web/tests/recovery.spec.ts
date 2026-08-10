import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('SSE disruption surfaces reconnecting state and resumes authority', async ({ page, kata }) => {
  await page.route('**/api/v1/events/stream*', (route) => route.abort('connectionrefused'))
  const credentials = await kata.launch(page)
  await expect(page.getByRole('status', { name: 'Kata daemon status' })).toBeVisible()
  const reconnectingHeader = await page.locator('.kata-header').boundingBox()
  const reconnectingLayout = await page.locator('.kata-layout').boundingBox()
  await page.unroute('**/api/v1/events/stream*')
  await kata.seedIssue(page, credentials, { title: 'Recovered stream example' })
  await page.reload()
  await expect(page.getByRole('button', { name: /Recovered stream example/ })).toBeVisible()
  await expect(page.getByRole('status', { name: 'Kata daemon status' })).toHaveCount(0)
  const connectedHeader = await page.locator('.kata-header').boundingBox()
  const connectedLayout = await page.locator('.kata-layout').boundingBox()
  expect(connectedHeader?.height).toBe(reconnectingHeader?.height)
  expect(connectedLayout?.y).toBe(reconnectingLayout?.y)
})

test('a 412 refresh preserves the current checklist draft for explicit retry', async ({
  page,
  kata,
}) => {
  await page.route('**/api/v1/events/stream*', (route) => route.abort('connectionrefused'))
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Conflict example task' })
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
  await page.getByRole('button', { name: 'Edit issue' }).click()
  await page.getByRole('button', { name: 'More actions' }).click()
  await page.getByRole('menuitem', { name: 'Add checklist' }).click()
  await page.getByRole('textbox', { name: 'New checklist item' }).fill('Local conflict draft')

  const external = await kata.request(
    page,
    credentials,
    'POST',
    `/api/v1/projects/${kata.projectID}/issues/${issue.short_id}/metadata`,
    { actor: 'user-a', patch: { external_marker: 'server conflict' } },
    { 'If-Match': `"rev-${issue.revision}"` },
  )
  expect(external.ok()).toBe(true)
  await page.getByRole('textbox', { name: 'New checklist item' }).press('Enter')
  await expect(page.getByRole('alert')).toContainText(/changed|conflict/i)
  await expect(page.getByRole('textbox', { name: 'New checklist item' })).toHaveValue(
    'Local conflict draft',
  )
})

test('daemon restart keeps an old draft visible but unsubmittable', async ({ page, kata }) => {
  await kata.launch(page)
  const credentials = await storedCredentials(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Restart draft example' })
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
  await page.getByRole('button', { name: 'Edit issue' }).click()
  await page.getByRole('textbox', { name: 'Comment' }).fill('Draft survives process restart')
  await kata.restart()
  await page.getByRole('button', { name: 'Add comment' }).click()
  await expect(page.getByRole('textbox', { name: 'Comment' })).toHaveValue(
    'Draft survives process restart',
  )
  await expect(page.getByRole('button', { name: 'Add comment' })).toBeDisabled()
})

test('read-only polling authority fences mutations without opening SSE', async ({ page, kata }) => {
  let streamRequests = 0
  page.on('request', (request) => {
    if (request.url().includes('/api/v1/events/stream')) streamRequests += 1
  })
  await page.route('**/api/v1/ui/snapshot?*', async (route) => {
    const response = await route.fetch()
    const snapshot = (await response.json()) as {
      capabilities: { writable: boolean; updates: string }
    }
    snapshot.capabilities = { writable: false, updates: 'poll' }
    await route.fulfill({ response, json: snapshot })
  })

  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Read-only example task' })
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
  await expect(page.getByRole('status', { name: 'Read-only Kata session' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Edit issue' })).toBeDisabled()
  expect(streamRequests).toBe(0)
})

test('read-only loopback listeners open directly without login ceremony', async ({
  page,
  kata,
}) => {
  await kata.restartReadonly()
  try {
    await page.goto(`${kata.origin}/kata?view=all-open`)

    await expect(page.getByRole('status', { name: 'Read-only Kata session' })).toBeVisible()
    await expect(page.getByRole('button', { name: 'New task' })).toBeDisabled()
  } finally {
    await kata.restart()
  }
})

test('structured 409 refusals preserve the draft and expose code and detail', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Domain refusal example' })
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
  await page.getByRole('button', { name: 'Edit issue' }).click()
  await page.getByRole('button', { name: 'More actions' }).click()
  await page.getByRole('menuitem', { name: 'Add checklist' }).click()
  await page.getByRole('textbox', { name: 'New checklist item' }).fill('Preserved refusal draft')
  await page.route('**/api/v1/projects/*/issues/*/metadata', async (route) => {
    await route.fulfill({
      status: 409,
      contentType: 'application/json',
      body: JSON.stringify({
        error: { code: 'example_refusal', detail: 'The example transition is unavailable.' },
      }),
    })
  })
  await page.getByRole('textbox', { name: 'New checklist item' }).press('Enter')
  await expect(page.getByRole('alert')).toContainText(
    'example_refusal: The example transition is unavailable.',
  )
  await expect(page.getByRole('textbox', { name: 'New checklist item' })).toHaveValue(
    'Preserved refusal draft',
  )
})

test('uncertain writes refresh authority before preserving an editable draft', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Uncertain example task' })
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
  await page.getByRole('button', { name: 'Edit issue' }).click()
  await page.route('**/api/v1/projects/*/issues/*', async (route) => {
    if (route.request().method() === 'PATCH') await route.abort('connectionreset')
    else await route.continue()
  })
  await page.getByRole('button', { name: 'Edit title' }).click()
  await page.getByRole('textbox', { name: 'Edit title' }).fill('Uncertain local draft')
  await page.getByRole('textbox', { name: 'Edit title' }).press('Enter')
  await expect(page.getByRole('alert')).toContainText(/result is uncertain/i)
  await expect(page.getByRole('textbox', { name: 'Edit title' })).toHaveValue(
    'Uncertain local draft',
  )
})

async function storedCredentials(page: import('@playwright/test').Page) {
  return page.evaluate(() =>
    JSON.parse(sessionStorage.getItem('kata.web.session.v1')!),
  ) as Promise<{
    session: string
    csrf: string
  }>
}
