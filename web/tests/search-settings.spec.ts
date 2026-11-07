import { expect, test } from './fixtures'

test.use({ colorScheme: 'dark' })

const off = {
  provider: 'none',
  download_bytes: 290_000_000,
  ollama_available: false,
  can_change: true,
}

test('Settings keeps Turning on equally active when indexing counts are hidden', async ({
  page,
  kata,
}) => {
  let current = off as Record<string, unknown>
  await page.route('**/api/v1/embeddings/local', (route) => route.fulfill({ json: current }))
  await kata.launch(page)
  const colors: Array<{ color: string; background: string }> = []
  for (const patch of [
    { provider: 'built_in', can_change: false, indexing: { done: 1, total: 2, failed: false } },
    { provider: 'built_in', can_change: false, indexing: { done: 0, total: 0, failed: false } },
  ]) {
    current = { ...off, ...patch }
    await page.goto(`${kata.origin}/kata?view=settings`)
    const badge = page.getByText('Turning on', { exact: true })
    await expect(badge).toBeVisible()
    colors.push(
      await badge.evaluate((element) => {
        const style = getComputedStyle(element.closest('.kit-chip')!)
        return { color: style.color, background: style.backgroundColor }
      }),
    )
  }
  expect(colors[1]).toEqual(colors[0])
})

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
    { provider: 'built_in', can_change: false, indexing: { done: 1, total: 2, failed: true } },
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
    } else {
      const progress = (await page.getByRole('progressbar').boundingBox())!
      const detail = (await page
        .getByText('Indexing 1 of 2 issues · Keyword search works meanwhile', { exact: true })
        .boundingBox())!
      const error = (await alert.boundingBox())!
      expect(error.y - detail.y - detail.height).toBeGreaterThan(
        detail.y - progress.y - progress.height,
      )
    }
  }
})

test('Settings keeps the search title aligned and separates secondary states', async ({
  page,
  kata,
}) => {
  let current = off as Record<string, unknown>
  await page.route('**/api/v1/embeddings/local', (route) => route.fulfill({ json: current }))
  await kata.launch(page)
  const positions: number[] = []
  const gaps: number[] = []
  for (const patch of [
    {},
    {
      can_change: false,
      job: { state: 'downloading', downloaded: 120_000_000, total: 290_000_000 },
    },
    { provider: 'built_in', can_change: false, indexing: { done: 1, total: 2, failed: false } },
    { job: { state: 'failed', downloaded: 0, total: 290_000_000, error: 'Disk is full.' } },
    { provider: 'built_in', can_change: false },
  ]) {
    current = { ...off, ...patch }
    await page.goto(`${kata.origin}/kata?view=settings`)
    const title = page.getByText('Search by meaning', { exact: true })
    await expect(title).toBeVisible()
    const titleBox = (await title.boundingBox())!
    positions.push(titleBox.y)
    const secondary = page
      .locator('.kit-search-by-meaning__line, [role="progressbar"], .search-row [role="alert"]')
      .first()
    if (await secondary.count()) {
      const secondaryBox = (await secondary.boundingBox())!
      gaps.push(secondaryBox.y - titleBox.y - titleBox.height)
    }
  }
  expect(Math.max(...positions) - Math.min(...positions)).toBeLessThanOrEqual(1)
  for (const gap of gaps) expect.soft(gap).toBeCloseTo(8, 0)
})
