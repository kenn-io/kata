import type { Locator, Page } from '@playwright/test'

import { expect, test } from './fixtures'

// Headless Chromium hides scrollbars by default, which would hide the
// scrollbar-width shift these tests guard against.
test.use({
  trace: 'off',
  launchOptions: { ignoreDefaultArgs: ['--hide-scrollbars'] },
})

const overflows = (scroller: Locator) =>
  scroller.evaluate((element) => element.scrollHeight > element.clientHeight)

async function expectNoShiftWhenScrollbarAppears(
  scroller: Locator,
  target: Locator,
  growContent: () => Promise<void>,
): Promise<void> {
  await expect.poll(() => overflows(scroller)).toBe(false)
  const before = await target.boundingBox()

  await growContent()

  await expect.poll(() => overflows(scroller)).toBe(true)
  expect(
    await scroller.evaluate(
      (element) => (element as HTMLElement).offsetWidth - element.clientWidth,
    ),
  ).toBeGreaterThan(0)
  expect((await target.boundingBox())?.x).toBe(before?.x)
}

async function addCatalogProjects(page: Page): Promise<void> {
  await page.route('**/api/v1/ui/snapshot?*', async (route) => {
    const response = await route.fetch()
    if (response.status() !== 200) return route.fulfill({ response })
    const snapshot = (await response.json()) as {
      catalog: Array<{ project: Record<string, unknown> }>
    }
    const base = snapshot.catalog[0]!
    for (let index = 0; index < 40; index++) {
      snapshot.catalog.push({
        ...base,
        project: {
          ...base.project,
          id: 1000 + index,
          uid: `01KEXAMPLEPROJECT${String(index).padStart(9, '0')}`,
          name: `example-extra-${index}`,
          metadata: {},
        },
      })
    }
    return route.fulfill({ response, json: snapshot })
  })
}

test('issue list columns stay in place when clearing a search makes the list scroll', async ({
  page,
  kata,
}) => {
  await page.setViewportSize({ width: 1400, height: 700 })
  const credentials = await kata.launch(page)
  await kata.seedIssue(page, credentials, { title: 'Scrollbar list anchor' })
  for (let index = 0; index < 40; index++) {
    await kata.seedIssue(page, credentials, { title: `Scrollbar list filler ${index}` })
  }
  await page.goto(`${kata.origin}/kata?view=all-open`)
  const search = page.getByRole('searchbox', { name: 'Search tasks' })
  await search.fill('Scrollbar list anchor')
  const filler = page.getByRole('button', { name: /Scrollbar list filler 39/ })
  await expect(filler).toBeHidden()

  await expectNoShiftWhenScrollbarAppears(
    page.locator('.table-body'),
    page.locator('.table-header .col-updated'),
    async () => {
      await search.fill('')
      await expect(filler).toBeVisible()
    },
  )
})

for (const viewport of [
  { width: 1000, height: 1100 },
  { width: 1400, height: 1300 },
]) {
  test(`issue actions stay in place when the detail scrolls at ${viewport.width}x${viewport.height}`, async ({
    page,
    kata,
  }) => {
    await page.setViewportSize(viewport)
    const credentials = await kata.launch(page)
    const short = await kata.seedIssue(page, credentials, {
      title: 'Scrollbar short detail',
      body: 'Short body',
    })
    const long = await kata.seedIssue(page, credentials, {
      title: 'Scrollbar long detail',
      body: Array.from({ length: 200 }, (_, index) => `Paragraph ${index}.`).join('\n\n'),
    })
    await page.goto(`${kata.origin}/kata?issue=${short.uid}`)
    await expect(page.getByRole('heading', { name: 'Scrollbar short detail' })).toBeVisible()

    await expectNoShiftWhenScrollbarAppears(
      page.locator('.kata-detail'),
      page.getByRole('button', { name: 'Edit title' }),
      async () => {
        await page.locator(`button.row[data-uid="${long.uid}"] .title-text`).click()
        await expect(page.getByRole('heading', { name: 'Scrollbar long detail' })).toBeVisible()
      },
    )
  })
}

test('credential controls stay in place when clearing a search makes the page scroll', async ({
  page,
  kata,
}) => {
  await page.setViewportSize({ width: 1400, height: 900 })
  await kata.launch(page)
  await page.route('**/api/v1/ui/snapshot?*', async (route) => {
    const response = await route.fetch()
    if (response.status() !== 200) return route.fulfill({ response })
    const snapshot = (await response.json()) as { capabilities: Record<string, unknown> }
    snapshot.capabilities.token_audit_read = true
    return route.fulfill({ response, json: snapshot })
  })
  await page.route('**/api/v1/tokens', (route) =>
    route.fulfill({
      json: {
        tokens: Array.from({ length: 60 }, (_, index) => ({
          id: `example-token-${index}`,
          name: `example-name-${index}`,
          actor: 'user-a',
          state: 'live',
          created_at: '2026-01-01T00:00:00Z',
        })),
      },
    }),
  )
  await page.goto(`${kata.origin}/kata?view=credentials`)
  const search = page.getByRole('searchbox', { name: 'Search credentials' })
  await search.fill('example-name-59')
  const filler = page.getByRole('cell', { name: 'example-token-0', exact: true })
  await expect(filler).toBeHidden()

  await expectNoShiftWhenScrollbarAppears(
    page.locator('.credential-scene'),
    page.getByRole('button', { name: 'Refresh' }),
    async () => {
      await search.fill('')
      await expect(filler).toBeVisible()
    },
  )
})

test('move picker names stay in place when clearing its search makes the list scroll', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Scrollbar move picker' })
  await addCatalogProjects(page)
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
  await page.getByRole('button', { name: 'Project: example-project', exact: true }).click()
  const search = page.getByRole('searchbox', { name: 'Find project' })
  const picker = page.getByRole('dialog', { name: 'Move task' })
  await expect(picker).toBeVisible()
  const bounds = await picker.boundingBox()
  expect(bounds).not.toBeNull()
  expect(bounds!.y + bounds!.height).toBeLessThanOrEqual(page.viewportSize()!.height)
  await search.fill('example-extra-39')
  const options = page.locator('.move-options')
  const filler = options.getByRole('button', { name: /example-extra-0/ })
  await expect(filler).toBeHidden()

  await expectNoShiftWhenScrollbarAppears(
    options,
    options.getByRole('button', { name: /example-extra-39/ }).locator('.move-project-name'),
    async () => {
      await search.fill('')
      await expect(filler).toBeVisible()
    },
  )
})

test('website column stays in place when leaving the short not-found page', async ({ page }) => {
  // A Playwright browser server drops ignoreDefaultArgs unless started with
  // --unsafe, so it keeps hiding the root scrollbar this test measures.
  test.skip(
    Boolean(process.env.PW_TEST_CONNECT_WS_ENDPOINT),
    'remote browser servers hide the root scrollbar',
  )
  await page.setViewportSize({ width: 1700, height: 900 })
  await page.route('**/*', (route) => {
    const { hostname, pathname } = new URL(route.request().url())
    // The docs tier supplies /docs/ assets; this page's layout does not depend on them.
    if (hostname !== 'website.example' || pathname.startsWith('/docs/')) return route.abort()
    const file = pathname.endsWith('/') ? `${pathname}index.html` : pathname
    return route.fulfill({ path: `../docs/website${file}` })
  })
  const rootOverflows = () =>
    page.evaluate(() => document.documentElement.scrollHeight > window.innerHeight)
  const wordmark = page.getByRole('link', { name: /^kata/ }).first()

  await page.goto('http://website.example/404.html')
  expect(await rootOverflows()).toBe(false)
  const before = await wordmark.boundingBox()

  await page.getByRole('link', { name: 'Back to the overview' }).click()
  await expect(page).toHaveURL('http://website.example/')
  expect(await rootOverflows()).toBe(true)
  expect(
    await page.evaluate(() => window.innerWidth - document.documentElement.clientWidth),
  ).toBeGreaterThan(0)
  expect((await wordmark.boundingBox())?.x).toBe(before?.x)
})
