import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('keyboard selection opens detail when search hides its row', async ({ page, kata }) => {
  const credentials = await kata.launch(page)
  await kata.seedIssue(page, credentials, { title: 'Example second task' })
  await kata.seedIssue(page, credentials, { title: 'Example first task' })
  const first = page.getByRole('button', { name: /Example first task/ })
  const second = page.getByRole('button', { name: /Example second task/ })
  await expect(second).toBeVisible()
  await first.focus()
  await page.clock.install()
  await page.clock.pauseAt(Date.now())
  await first.press('ArrowDown')
  await expect(second).toBeFocused()
  await page.getByRole('searchbox', { name: 'Search tasks' }).fill('unmatched')
  await expect(page).toHaveURL(/text=unmatched/)
  await expect(second).toHaveCount(0)
  await page.clock.runFor(50)
  await expect(
    page.getByRole('heading', { name: 'Example second task', exact: true }),
  ).toBeVisible()
  await expect(page).toHaveURL(/issue=.*text=unmatched/)
})

test('views, projects, filters, columns, hierarchy, and keyboard stay first-class', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const parent = await kata.seedIssue(page, credentials, {
    title: 'Example parent task',
    owner: 'user-a',
    labels: ['planning'],
    priority: 1,
  })
  await kata.seedIssue(page, credentials, {
    title: 'Example child task',
    owner: 'user-b',
    labels: ['delivery'],
    links: [{ type: 'parent', to_ref: parent.uid }],
  })

  await expect(page.getByRole('button', { name: /Example parent task/ })).toBeVisible()
  await expect(page.getByRole('button', { name: /Example child task/ })).toHaveCount(0)
  await page.getByRole('button', { name: /Example parent task/ }).press('ArrowRight')
  await expect(page.getByRole('button', { name: /Example child task/ })).toBeVisible()

  await page.getByRole('button', { name: 'Columns' }).click()
  await expect(page.getByText('Shown when space allows')).toBeVisible()
  await expect(page.getByRole('checkbox', { name: 'Owner' })).toBeChecked()
  await page.keyboard.press('Escape')

  const parentRow = page.getByRole('button', { name: /Example parent task/ })
  await parentRow.focus()
  let releaseSnapshot = () => {}
  const heldSnapshot = new Promise<void>((resolve) => {
    releaseSnapshot = resolve
  })
  let reportSnapshot = () => {}
  const snapshotRequested = new Promise<void>((resolve) => {
    reportSnapshot = resolve
  })
  let intercepted = false
  await page.route('**/api/v1/ui/snapshot?*', async (request) => {
    if (intercepted || new URL(request.request().url()).searchParams.get('text') !== 'c') {
      await request.continue()
      return
    }
    intercepted = true
    const response = await request.fetch()
    reportSnapshot()
    await heldSnapshot
    await request.fulfill({ response }).catch(() => {})
  })
  await page.clock.install()
  await page.clock.pauseAt(Date.now())
  await parentRow.press('ArrowDown')
  await expect(page.getByRole('button', { name: /Example child task/ })).toBeFocused()

  const search = page.getByRole('searchbox', { name: 'Search tasks' })
  await search.focus()
  try {
    await page.keyboard.type('c')
    await snapshotRequested
    await page.clock.runFor(50)
    await expect(
      page.getByRole('heading', { name: 'Example child task', exact: true }),
    ).toBeVisible()
    await expect(search).toBeFocused()
    await page.clock.resume()
    await page.keyboard.type('hild')
  } finally {
    releaseSnapshot()
  }
  await expect(search).toHaveValue('child')
  await expect(page).toHaveURL(/issue=.*text=child/)
  await expect(page.getByRole('button', { name: /Example child task/ })).toBeVisible()

  for (const view of ['Inbox', 'Today', 'Delegated', 'Scheduled']) {
    await page.getByRole('button', { name: new RegExp(`^${view}( \\d+)?$`) }).click()
    await expect(page.locator('[aria-label="Issues"]')).toBeVisible()
  }
  await expect(page).toHaveURL(/view=scheduled/)
  await expect(page.getByRole('button', { name: 'Upcoming' })).toHaveCount(0)
  await page.getByRole('button', { name: /^example-project \d+$/ }).click()
  await expect(page).toHaveURL(`${kata.origin}/kata?scope=${kata.projectUID}`)
})

test('project creation and quick capture use the designated inbox project', async ({
  page,
  kata,
}) => {
  await kata.launch(page)

  await page.getByRole('button', { name: 'New project' }).click()
  await page.getByRole('textbox', { name: 'New project name' }).fill('example-workspace')
  await page.getByRole('textbox', { name: 'New project name' }).press('Enter')
  await expect(page.getByRole('button', { name: /^example-workspace\b/ })).toBeVisible()

  await page.getByRole('button', { name: 'New task' }).click()
  await page.getByRole('textbox', { name: 'Quick capture' }).fill('Captured example task')
  await page.getByRole('textbox', { name: 'Quick capture' }).press('Enter')
  await expect(page.getByRole('button', { name: /Captured example task/ })).toBeVisible()
})

test('New task designates an Inbox before opening quick capture', async ({ page, kata }) => {
  const credentials = await kata.launch(page)
  const snapshot = (await kata.snapshot(page, credentials)) as {
    catalog: Array<{
      project: { id: number; name: string; metadata: Record<string, unknown> }
    }>
  }
  const inbox = snapshot.catalog.find(({ project }) => project.metadata.role === 'inbox')?.project
  expect(inbox).toBeDefined()
  const cleared = await kata.request(
    page,
    credentials,
    'POST',
    `/api/v1/projects/${inbox!.id}/metadata`,
    { actor: 'user-a', patch: { role: null } },
  )
  expect(cleared.ok()).toBe(true)
  await page.reload()

  const create = page.getByRole('button', { name: 'New task' })
  await expect(create).toBeEnabled()
  await create.click()
  await expect(page.getByRole('dialog', { name: 'Choose Inbox project' })).toBeVisible()
  await page.getByRole('button', { name: 'Use example-project as Inbox' }).click()

  await expect(page.getByRole('textbox', { name: 'Quick capture' })).toBeVisible()
  const restored = await kata.request(
    page,
    credentials,
    'POST',
    `/api/v1/projects/${inbox!.id}/metadata`,
    { actor: 'user-a', patch: { role: 'inbox' } },
  )
  expect(restored.ok()).toBe(true)
})
