import { expect, test } from './fixtures'

test.use({ colorScheme: 'dark', reducedMotion: 'reduce' })

test('read-only notices remain visible', async ({ page, kata }) => {
  let inventoryReads = 0
  page.on('request', (request) => {
    if (new URL(request.url()).pathname === '/api/v1/tokens') inventoryReads++
  })
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Review example release' })
  await page.goto(`${kata.origin}/kata?view=all-open`)
  await expect(page.getByRole('button', { name: /Review example release/ })).toBeVisible()
  const summary = page.getByRole('status', { name: 'Active connection' })

  await kata.restartReadonly()
  try {
    await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
    await expect(summary).toContainText('Credential: Read-only')
    await page.setViewportSize({ width: 1440, height: 900 })
    await expect(summary).not.toContainText('example-local-token')
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: 900 })
      const layout = await summary.evaluate((element) => {
        const rect = element.getBoundingClientRect()
        const header = document
          .querySelector(
            window.innerWidth <= 760 ? '.mobile-navigation-trigger' : '.sidebar-header',
          )!
          .getBoundingClientRect()
        return {
          top: rect.top,
          bottom: rect.bottom,
          headerTop: header.top,
          left: rect.left,
          right: rect.right,
          viewport: window.innerWidth,
        }
      })
      expect(layout.top).toBe(0)
      expect(layout.bottom).toBeLessThanOrEqual(layout.headerTop)
      expect(layout.left).toBeGreaterThanOrEqual(0)
      expect(layout.right).toBeLessThanOrEqual(layout.viewport)
    }
  } finally {
    await kata.restart()
  }
  expect(inventoryReads).toBe(0)
})
