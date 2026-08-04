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

  await page.getByRole('button', { name: 'Edit title' }).click()
  await expect(page.getByRole('textbox', { name: 'Edit title' })).toBeFocused()
  await page.getByRole('textbox', { name: 'Edit title' }).press('Escape')

  let results = await new AxeBuilder({ page }).analyze()
  expect(results.violations).toEqual([])

  await page.setViewportSize({ width: 390, height: 844 })
  await expect(page.getByRole('region', { name: 'Task detail' })).toBeVisible()
  results = await new AxeBuilder({ page }).analyze()
  expect(results.violations).toEqual([])
})
