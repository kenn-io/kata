import { expect, test } from './fixtures'

test.use({ colorScheme: 'dark', reducedMotion: 'reduce' })

test('healthy connections leave the toolbar at the top; read-only notices remain visible', async ({
  page,
  kata,
}, testInfo) => {
  let inventoryReads = 0
  page.on('request', (request) => {
    if (new URL(request.url()).pathname === '/api/v1/tokens') inventoryReads++
  })
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Review example release' })
  await page.goto(`${kata.origin}/kata?view=all-open`)
  await expect(page.getByRole('button', { name: /Review example release/ })).toBeVisible()
  const summary = page.getByRole('status', { name: 'Active connection' })

  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 900 })
    await expect(summary).toHaveCount(0)
    expect((await page.locator('.kata-header').boundingBox())?.y).toBe(0)
    await expect(page.getByRole('button', { name: 'New task' })).toBeEnabled()
    await expect(page.getByRole('button', { name: /Switch Kata daemon:/ })).toBeVisible()
    await page.screenshot({
      path: testInfo.outputPath(`connected-workspace-${width}.png`),
      clip: { x: 0, y: 0, width, height: width === 1440 ? 360 : 900 },
    })
  }

  await kata.restartReadonly()
  try {
    await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
    await expect(summary).toContainText('Credential: Read-only')
    await expect(page.getByRole('button', { name: 'New task' })).toBeDisabled()
    await expect(summary).not.toContainText('example-local-token')
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: 900 })
      const layout = await summary.evaluate((element) => {
        const rect = element.getBoundingClientRect()
        const header = document.querySelector('.kata-header')!.getBoundingClientRect()
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
