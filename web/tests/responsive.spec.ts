import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('a long server name leaves New task reachable', async ({ page, kata }) => {
  const name = 'example-local-server-with-a-very-long-configured-name'
  await page.route('**/api/v1/**', async (route) => {
    const headers = { ...route.request().headers() }
    if (headers['x-kata-web-daemon'] === name) headers['x-kata-web-daemon'] = 'example-local'
    if (new URL(route.request().url()).pathname === '/api/v1/ui/daemons') {
      const response = await route.fetch({ headers })
      const roster = await response.json()
      for (const daemon of roster.daemons) if (daemon.id === 'example-local') daemon.id = name
      await route.fulfill({ response, json: roster })
    } else await route.continue({ headers })
  })
  await kata.launch(page)
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 800 })
    if (width === 390) await page.getByRole('button', { name: 'Open navigation' }).click()
    const sidebar = page.locator('.sidebar-header')
    const task = page.getByRole('button', { name: 'New task' })
    const bounds = (await sidebar.boundingBox())!
    const button = (await task.boundingBox())!
    expect(button.x + button.width).toBeLessThanOrEqual(bounds.x + bounds.width)
  }
})

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
  await expect(navigation.locator('[data-testid="daemon-chip"]')).toHaveCount(1)
  await navigation.getByRole('button', { name: 'Today' }).click()
  await expect(navigation).toBeHidden()
  await expect(page).toHaveURL(/view=today/)
})

test('keeps the desktop project group collapsed after opening mobile navigation', async ({
  page,
  kata,
}) => {
  await page.setViewportSize({ width: 1000, height: 684 })
  await kata.launch(page)
  const projects = page.getByRole('button', { name: /Projects/ })
  await projects.click()
  await expect(projects).toHaveAttribute('aria-expanded', 'false')

  await page.setViewportSize({ width: 600, height: 684 })
  await page.getByRole('button', { name: 'Open navigation' }).click()
  await page
    .getByRole('dialog', { name: 'Navigation' })
    .getByRole('button', { name: 'Close' })
    .click()
  await page.setViewportSize({ width: 1000, height: 684 })

  await expect(projects).toHaveAttribute('aria-expanded', 'false')
})

test('resizing an open navigation drawer restores desktop navigation', async ({ page, kata }) => {
  await page.setViewportSize({ width: 600, height: 684 })
  await kata.launch(page)
  await page.getByRole('button', { name: 'Open navigation' }).click()
  await expect(page.getByRole('dialog', { name: 'Navigation' })).toBeVisible()

  await page.setViewportSize({ width: 1000, height: 684 })
  await expect(page.getByRole('dialog', { name: 'Navigation' })).toBeHidden()
  await expect(page.getByRole('button', { name: 'Today' })).toBeVisible()
})

test('Appearance changes the sidebar color', async ({ page, kata }) => {
  await page.emulateMedia({ colorScheme: 'dark' })
  await kata.launch(page)

  const sidebarColor = () =>
    page.locator('.kata-sidebar').evaluate((element) => getComputedStyle(element).backgroundColor)
  const darkColor = await sidebarColor()
  await page.getByRole('button', { name: 'Settings' }).click()
  await page.getByRole('radio', { name: 'Light', exact: true }).click()
  await expect.poll(sidebarColor).not.toBe(darkColor)
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
  const title = (await list.getByRole('heading', { name: 'All open' }).boundingBox())!
  const count = (await list.getByText(/^\d+ tasks?$/).boundingBox())!

  expect(count.y).toBeLessThan(title.y + title.height)
  expect(count.y + count.height).toBeGreaterThan(title.y)
  expect(count.x).toBeGreaterThan(title.x + title.width)
  expect(count.x - (title.x + title.width)).toBeLessThan(40)
})
