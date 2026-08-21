import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('system views stay beside the list until the mobile layout', async ({ page, kata }) => {
  await page.setViewportSize({ width: 853, height: 684 })
  await kata.launch(page)

  const layout = await page.locator('.kata-layout').evaluate((element) => {
    const sidebar = element.querySelector('.kata-sidebar')?.getBoundingClientRect()
    const main = element.querySelector('.kata-main')?.getBoundingClientRect()
    if (!sidebar || !main) throw new Error('Kata layout panes are missing')
    return {
      sameRow: Math.round(sidebar.top) === Math.round(main.top),
      sidebarBeforeMain: Math.round(sidebar.right) <= Math.round(main.left),
    }
  })

  expect(layout).toEqual({ sameRow: true, sidebarBeforeMain: true })
})

test('mobile navigation stays out of the task flow until opened', async ({ page, kata }) => {
  await page.setViewportSize({ width: 600, height: 684 })
  await kata.launch(page)

  const layout = await page.locator('.kata-feature').evaluate((element) => {
    const header = element.querySelector('.kata-header')?.getBoundingClientRect()
    const main = element.querySelector('.kata-main')?.getBoundingClientRect()
    if (!header || !main) throw new Error('Kata header or main pane is missing')
    return {
      headerBottom: Math.round(header.bottom),
      mainTop: Math.round(main.top),
    }
  })

  expect(layout.mainTop).toBe(layout.headerBottom)

  await page.getByRole('button', { name: 'Open navigation' }).click()
  const navigation = page.getByRole('dialog', { name: 'Kata navigation' })
  await expect(navigation).toBeVisible()
  await navigation.getByRole('button', { name: 'Today' }).click()
  await expect(navigation).toBeHidden()
  await expect(page).toHaveURL(/view=today/)
})

test('task filters stay inside a narrow list pane without overlapping', async ({ page, kata }) => {
  await page.setViewportSize({ width: 1440, height: 800 })
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Narrow pane example' })
  await page.goto(`${kata.origin}/kata?view=all-open&issue=${issue.uid}`)
  await page.getByRole('button', { name: 'Switch to side-by-side layout' }).click()

  const layout = await page.locator('.kata-search-toolbar').evaluate((toolbar) => {
    const container = toolbar.getBoundingClientRect()
    const controls = Array.from(toolbar.querySelectorAll('input, button'), (element) => {
      const rect = element.getBoundingClientRect()
      return { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom }
    })
    const overlaps = controls.some((control, index) =>
      controls
        .slice(index + 1)
        .some(
          (other) =>
            control.left < other.right &&
            control.right > other.left &&
            control.top < other.bottom &&
            control.bottom > other.top,
        ),
    )
    return {
      height: container.height,
      contained: controls.every(
        (control) => control.left >= container.left && control.right <= container.right,
      ),
      overlaps,
    }
  })

  expect(layout).toEqual({ height: expect.any(Number), contained: true, overlaps: false })
  expect(layout.height).toBeGreaterThan(30)
})
