import { expect, test } from './fixtures'

// A11: inspect the real production UI's current authority summary at desktop
// and narrow widths, with no credential-inventory request or plaintext token.
test('active connection stays above the workspace and wraps on narrow screens', async ({
  page,
  kata,
}, testInfo) => {
  let inventoryReads = 0
  page.on('request', (request) => {
    if (new URL(request.url()).pathname === '/api/v1/tokens') inventoryReads++
  })
  const credentials = await kata.launch(page)
  const snapshot = await kata.snapshot(page, credentials)
  const capabilities = snapshot.capabilities as { account?: string }
  const summary = page.getByRole('status', { name: 'Active connection' })
  await expect(summary).toBeVisible()
  await expect(summary).toContainText('Hub:')
  await expect(summary).toContainText(`Account: ${capabilities.account || 'Local connection'}`)
  await expect(summary).toContainText('Connected')
  await expect(summary).not.toContainText('example-local-token')
  const accent = await summary.evaluate((element) => {
    const color = getComputedStyle(element).borderInlineStartColor
    const [red, green, blue] = color.match(/\d+/g)!.map(Number)
    return { red: red!, green: green!, blue: blue! }
  })
  expect(accent.red).toBeGreaterThanOrEqual(accent.green)
  expect(accent.green - accent.blue).toBeGreaterThan(accent.red - accent.green)

  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 800 })
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
    await page.screenshot({ path: testInfo.outputPath(`active-connection-${width}.png`) })
  }
  expect(inventoryReads).toBe(0)
})
