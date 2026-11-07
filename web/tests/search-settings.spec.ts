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
