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
    const header = element.querySelector('.mobile-navigation-trigger')?.getBoundingClientRect()
    const main = element.querySelector('.kata-main')?.getBoundingClientRect()
    if (!header || !main) throw new Error('Kata header or main pane is missing')
    return {
      headerBottom: Math.round(header.bottom),
      mainTop: Math.round(main.top),
    }
  })

  expect(layout.mainTop).toBe(layout.headerBottom)

  await page.getByRole('button', { name: 'Open navigation' }).click()
  const navigation = page.getByRole('dialog', { name: 'Navigation' })
  await expect(navigation).toBeVisible()
  await expect(page.locator('[data-testid="daemon-chip"]')).toHaveCount(1)
  await page.setViewportSize({ width: 853, height: 684 })
  expect((await page.locator('.kata-main').boundingBox())!.width).toBeGreaterThan(500)
  await page.setViewportSize({ width: 600, height: 684 })
  await navigation.getByRole('button', { name: 'Today' }).click()
  await expect(navigation).toBeHidden()
  await expect(page).toHaveURL(/view=today/)
})

test('New task closes mobile navigation before choosing an Inbox', async ({ page, kata }) => {
  await page.setViewportSize({ width: 600, height: 684 })
  await kata.launch(page)

  await page.getByRole('button', { name: 'Open navigation' }).click()
  const navigation = page.getByRole('dialog', { name: 'Navigation' })
  await navigation.getByRole('button', { name: 'New task' }).click()

  await expect(navigation).toBeHidden()
  await expect(page.getByRole('dialog', { name: 'Choose Inbox project' })).toBeVisible()
})

test('sidebar navigation rows keep their height on hover', async ({ page, kata }) => {
  await page.setViewportSize({ width: 1440, height: 800 })
  await kata.launch(page)

  const today = page.getByRole('button', { name: /^Today(?: \d+)?$/ })
  const before = (await today.boundingBox())!
  await today.hover()
  const after = (await today.boundingBox())!

  expect(after.height).toBe(before.height)
})

test('Appearance changes and keeps the workspace theme', async ({ page, kata }) => {
  await page.emulateMedia({ colorScheme: 'dark' })
  await kata.launch(page)

  const sidebarColor = () =>
    page.locator('.kata-sidebar').evaluate((element) => getComputedStyle(element).backgroundColor)
  const darkColor = await sidebarColor()
  expect(await page.locator('html').evaluate((element) => element.classList.contains('dark'))).toBe(
    true,
  )

  await page.getByRole('button', { name: 'Settings' }).click()
  await page.getByRole('radio', { name: 'Light', exact: true }).click()
  await expect
    .poll(() => page.locator('html').evaluate((element) => element.classList.contains('dark')))
    .toBe(false)
  const lightColor = await sidebarColor()
  expect(lightColor).not.toBe(darkColor)

  await page.reload()
  await expect
    .poll(() => page.locator('html').evaluate((element) => element.classList.contains('dark')))
    .toBe(false)
  await page.getByRole('button', { name: 'Settings' }).click()
  await page.getByRole('radio', { name: 'Dark', exact: true }).click()
  await expect
    .poll(() => page.locator('html').evaluate((element) => element.classList.contains('dark')))
    .toBe(true)
  await expect.poll(sidebarColor).toBe(darkColor)
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

test('the task count sits beside the list title', async ({ page, kata }) => {
  await page.setViewportSize({ width: 1440, height: 800 })
  const credentials = await kata.launch(page)
  await kata.seedIssue(page, credentials, { title: 'Counted example task' })
  await page.goto(`${kata.origin}/kata?view=all-open`)

  const list = page.getByRole('region', { name: 'Issues' })
  const title = (await list.getByRole('heading', { name: 'All Open' }).boundingBox())!
  const count = (await list.getByText(/^\d+ tasks?$/).boundingBox())!

  expect(count.y).toBeLessThan(title.y + title.height)
  expect(count.y + count.height).toBeGreaterThan(title.y)
  expect(count.x).toBeGreaterThan(title.x + title.width)
  expect(count.x - (title.x + title.width)).toBeLessThan(40)
})
