import AxeBuilder from '@axe-core/playwright'

import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('standalone shell preserves the Forge base typography and header geometry', async ({
  page,
  kata,
}) => {
  await kata.launch(page)

  const shell = await page.locator('.kata-header').evaluate((element) => {
    const style = getComputedStyle(element)
    return { boxSizing: style.boxSizing, height: element.getBoundingClientRect().height }
  })
  expect(shell).toEqual({ boxSizing: 'border-box', height: 61 })
  await expect(page.locator('body')).toHaveCSS('font-size', '13px')
})

test('standalone issue table retains its own vertical scroll plane', async ({ page, kata }) => {
  await page.setViewportSize({ width: 1100, height: 600 })
  const credentials = await kata.launch(page)
  await Promise.all(
    Array.from({ length: 32 }, (_, index) =>
      kata.seedIssue(page, credentials, { title: `Scrollable example task ${index + 1}` }),
    ),
  )
  await page.goto(`${kata.origin}/kata?view=all-open`)

  const tableBody = page.locator('.table-body')
  await expect(tableBody.getByRole('button', { name: /Scrollable example task/ })).toHaveCount(32)
  const dimensions = await tableBody.evaluate((element) => ({
    clientHeight: element.clientHeight,
    scrollHeight: element.scrollHeight,
  }))
  expect(dimensions.scrollHeight).toBeGreaterThan(dimensions.clientHeight)

  await tableBody.evaluate((element) => {
    element.scrollTop = element.scrollHeight
  })
  await expect.poll(() => tableBody.evaluate((element) => element.scrollTop)).toBeGreaterThan(0)
})

test('desktop and responsive detail remain focused, contrasted, and axe-clean', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, {
    title: 'Accessible example task',
    body: 'Accessible content',
  })
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)

  await expect(page.getByRole('button', { name: 'Switch daemon' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Open workspace' })).toHaveCount(0)
  await expect(page.getByRole('dialog', { name: 'Command palette' })).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Kata issue detail' })).toBeVisible()

  let results = await new AxeBuilder({ page }).analyze()
  expect(results.violations).toEqual([])

  await page.getByRole('button', { name: 'Edit issue' }).click()
  await page.getByRole('button', { name: 'Edit title' }).click()
  await expect(page.getByRole('textbox', { name: 'Edit title' })).toBeFocused()
  await page.getByRole('textbox', { name: 'Edit title' }).press('Escape')

  results = await new AxeBuilder({ page }).analyze()
  expect(results.violations).toEqual([])

  await page.setViewportSize({ width: 390, height: 844 })
  await expect(page.getByRole('region', { name: 'Task detail' })).toBeVisible()
  results = await new AxeBuilder({ page }).analyze()
  expect(results.violations).toEqual([])
})

test('dark read-only detail actions keep a dark Kit UI surface', async ({ page, kata }) => {
  await page.emulateMedia({ colorScheme: 'dark' })
  await page.route('**/api/v1/ui/snapshot?*', async (route) => {
    const response = await route.fetch()
    const snapshot = (await response.json()) as {
      capabilities: { writable: boolean; updates: string }
    }
    snapshot.capabilities = { writable: false, updates: 'poll' }
    await route.fulfill({ response, json: snapshot })
  })

  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Dark theme example task' })
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)

  const action = page.getByRole('button', { name: 'Edit issue' })
  await expect(action).toBeDisabled()
  await expect(action).toHaveClass(/kit-button/)
  const appearance = await action.evaluate((element) => {
    const style = getComputedStyle(element)
    return { background: style.backgroundColor, foreground: style.color }
  })

  expect(relativeLuminance(appearance.background)).toBeLessThan(0.05)
  expect(relativeLuminance(appearance.foreground)).toBeGreaterThan(
    relativeLuminance(appearance.background),
  )
})

function relativeLuminance(cssColor: string): number {
  const channels = cssColor
    .match(/[\d.]+/g)
    ?.slice(0, 3)
    .map(Number)
  if (!channels || channels.length !== 3) throw new Error(`unsupported CSS color: ${cssColor}`)
  const linear = channels.map((channel) => {
    const value = channel / 255
    return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4
  })
  return 0.2126 * linear[0]! + 0.7152 * linear[1]! + 0.0722 * linear[2]!
}
