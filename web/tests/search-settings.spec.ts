import { expect, test } from './fixtures'

test.use({ colorScheme: 'dark' })

const off = {
  provider: 'none',
  download_bytes: 290_000_000,
  ollama_available: false,
  rebuild: false,
  can_change: true,
}

test('Settings failures stand out above static download details', async ({ page, kata }) => {
  let current = off as Record<string, unknown>
  await page.route('**/api/v1/embeddings/local', (route) => route.fulfill({ json: current }))
  await kata.launch(page)
  for (const patch of [
    {
      job: {
        state: 'failed',
        downloaded: 0,
        total: 290_000_000,
        error: 'Not enough disk space. Free space and try again.',
      },
    },
    { provider: 'built_in', indexing: { done: 1, total: 2, failed: true } },
    { provider: 'ollama', indexing: { done: 1, total: 2, failed: true } },
  ]) {
    current = { ...off, ...patch }
    await page.goto(`${kata.origin}/kata?view=settings`)
    const alert = page.getByRole('alert')
    await expect(alert).toBeVisible()
    const colors = await alert.evaluate((element) => {
      const probe = document.createElement('span')
      probe.style.color = 'var(--text-muted)'
      element.append(probe)
      const muted = getComputedStyle(probe).color
      probe.remove()
      return { failure: getComputedStyle(element).color, muted }
    })
    expect(colors.failure).not.toBe(colors.muted)
    const download = page.getByText('Downloads 290 MB once, shared by all Kenn apps', {
      exact: true,
    })
    if (patch.provider !== 'built_in') {
      await expect(download).toBeVisible()
      expect((await alert.boundingBox())!.y).toBeLessThan((await download.boundingBox())!.y)
    }
  }
})

test('a blocked switch shows the blocker without switch or download promises', async ({
  page,
  kata,
}) => {
  await page.route('**/api/v1/embeddings/local', (route) =>
    route.fulfill({
      json: {
        ...off,
        provider: 'ollama',
        can_change: false,
        job: {
          state: 'failed',
          error: 'KATA_SEARCH_EMBEDDINGS_DIMS conflicts with built-in search.',
        },
      },
    }),
  )
  await kata.launch(page)
  await page.goto(`${kata.origin}/kata?view=settings`)
  await expect(page.getByRole('alert')).toContainText('KATA_SEARCH_EMBEDDINGS_DIMS')
  await expect(page.getByText('Switching keeps your index', { exact: true })).toBeHidden()
  await expect(
    page.getByText('Downloads 290 MB once, shared by all Kenn apps', { exact: true }),
  ).toBeHidden()
  await expect(page.getByRole('button', { name: 'Use built-in instead' })).toBeHidden()
})

test('Settings keeps the search title aligned as setup changes state', async ({ page, kata }) => {
  let current = off as Record<string, unknown>
  await page.route('**/api/v1/embeddings/local', (route) => route.fulfill({ json: current }))
  await kata.launch(page)
  const positions: number[] = []
  for (const patch of [
    {},
    { job: { state: 'downloading', downloaded: 120_000_000, total: 290_000_000 } },
    { provider: 'built_in', indexing: { done: 1, total: 2 } },
    { job: { state: 'failed', downloaded: 0, total: 290_000_000, error: 'Disk is full.' } },
    { provider: 'built_in' },
  ]) {
    current = { ...off, ...patch }
    await page.goto(`${kata.origin}/kata?view=settings`)
    const title = page.getByText('Search by meaning', { exact: true })
    await expect(title).toBeVisible()
    positions.push((await title.boundingBox())!.y)
  }
  expect(Math.max(...positions) - Math.min(...positions)).toBeLessThanOrEqual(1)
})

test('Settings separates the search title consistently from every secondary state', async ({
  page,
  kata,
}) => {
  let current = off as Record<string, unknown>
  await page.route('**/api/v1/embeddings/local', (route) => route.fulfill({ json: current }))
  await kata.launch(page)
  const gaps: number[] = []
  for (const patch of [
    {},
    { job: { state: 'downloading', downloaded: 120_000_000, total: 290_000_000 } },
    { job: { state: 'failed', downloaded: 0, total: 290_000_000, error: 'Disk is full.' } },
  ]) {
    current = { ...off, ...patch }
    await page.goto(`${kata.origin}/kata?view=settings`)
    const title = page.getByText('Search by meaning', { exact: true })
    await expect(title).toBeVisible()
    const titleBox = (await title.boundingBox())!
    const secondary = page
      .locator('.kit-search-by-meaning__line, [role="progressbar"], .search-row [role="alert"]')
      .first()
    const secondaryBox = (await secondary.boundingBox())!
    gaps.push(secondaryBox.y - titleBox.y - titleBox.height)
  }
  for (const gap of gaps) expect.soft(gap).toBeCloseTo(8, 0)
})

test('Settings groups Ollama retry guidance before both switch helpers', async ({ page, kata }) => {
  await page.route('**/api/v1/embeddings/local', (route) =>
    route.fulfill({
      json: { ...off, provider: 'ollama', indexing: { done: 1, total: 2, failed: true } },
    }),
  )
  await kata.launch(page)
  await page.goto(`${kata.origin}/kata?view=settings`)
  const failure = page.getByRole('alert')
  await expect(failure).toBeVisible()
  const helper = page.getByText('Switching keeps your index', { exact: true })
  expect((await failure.boundingBox())!.y).toBeLessThan((await helper.boundingBox())!.y)
})

test('Settings groups related details apart from retry errors', async ({ page, kata }) => {
  let current = off as Record<string, unknown>
  await page.route('**/api/v1/embeddings/local', (route) => route.fulfill({ json: current }))
  await kata.launch(page)
  for (const provider of ['ollama', 'built_in']) {
    current = { ...off, provider, indexing: { done: 1, total: 2, failed: true } }
    await page.goto(`${kata.origin}/kata?view=settings`)
    const alert = page.getByRole('alert')
    await expect(alert).toBeVisible()
    const first = provider === 'ollama' ? alert : page.getByRole('progressbar')
    const second =
      provider === 'ollama'
        ? page.getByText('Switching keeps your index', { exact: true })
        : page.getByText('Indexing 1 of 2 issues · Keyword search works meanwhile', { exact: true })
    const third =
      provider === 'ollama'
        ? page.getByText('Downloads 290 MB once, shared by all Kenn apps', { exact: true })
        : alert
    const a = (await first.boundingBox())!
    const b = (await second.boundingBox())!
    const c = (await third.boundingBox())!
    const firstGap = b.y - a.y - a.height
    const secondGap = c.y - b.y - b.height
    const betweenGroups = provider === 'ollama' ? firstGap : secondGap
    const withinGroup = provider === 'ollama' ? secondGap : firstGap
    expect.soft(betweenGroups, provider).toBeGreaterThan(withinGroup)
  }
})
