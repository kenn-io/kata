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
    const sidebar = page.locator('.sidebar-header:visible')
    const task = sidebar.getByRole('button', { name: 'New task' })
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

test('mobile bar opens task capture without opening navigation', async ({ page, kata }) => {
  await page.setViewportSize({ width: 390, height: 800 })
  await kata.launch(page)

  await page.locator('.mobile-navigation-trigger').getByRole('button', { name: 'New task' }).click()
  await expect(page.getByRole('dialog', { name: 'New task' })).toBeVisible()
  await expect(page.getByRole('dialog', { name: 'Navigation' })).toHaveCount(0)
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

test('resizing an open drawer restores desktop navigation and its project state', async ({
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
  await expect(page.getByRole('dialog', { name: 'Navigation' })).toBeVisible()
  await page.setViewportSize({ width: 1000, height: 684 })

  await expect(page.getByRole('dialog', { name: 'Navigation' })).toBeHidden()
  await expect(page.getByRole('button', { name: 'Today' })).toBeVisible()
  await expect(projects).toHaveAttribute('aria-expanded', 'false')
})

test('Settings tabs stay above the section at desktop and mobile widths', async ({
  page,
  kata,
}) => {
  await kata.launch(page, '/kata?view=settings&section=appearance')
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 800 })
    const tabs = page.getByRole('tablist', { name: 'Settings' })
    const heading = page.getByRole('heading', { name: 'Appearance' })
    const tabsBounds = (await tabs.boundingBox())!
    const headingBounds = (await heading.boundingBox())!
    expect(tabsBounds.y + tabsBounds.height).toBeLessThanOrEqual(headingBounds.y)
    if (width === 1440) {
      const headerHeight = await tabs.evaluate((list) =>
        parseFloat(getComputedStyle(list).getPropertyValue('--layout-header-h')),
      )
      expect(tabsBounds.height).toBeCloseTo(headerHeight, 0)
      const selected = page.getByRole('tab', { name: 'Appearance' })
      const indicatorBottom = await selected.evaluate(
        (tab) =>
          tab.getBoundingClientRect().bottom -
          Number.parseFloat(getComputedStyle(tab, '::after').bottom),
      )
      expect(indicatorBottom).toBeCloseTo(tabsBounds.y + tabsBounds.height - 1, 0)
    }
    const labelBounds = (await tabs.locator('.kit-tabs__label').first().boundingBox())!
    const underline = await page.getByRole('tab', { name: 'Appearance' }).evaluate((tab) => {
      const bounds = tab.getBoundingClientRect()
      const style = getComputedStyle(tab, '::after')
      return {
        left: bounds.left + parseFloat(style.left),
        right: bounds.right - parseFloat(style.right),
      }
    })
    expect(underline.left).toBeCloseTo(labelBounds.x, 0)
    expect(underline.right).toBeCloseTo(labelBounds.x + labelBounds.width, 0)
    if (width === 1440) {
      expect(tabsBounds.x).toBeCloseTo(headingBounds.x, 0)
      expect(tabsBounds.x + tabsBounds.width).toBeCloseTo(headingBounds.x + headingBounds.width, 0)
    }
    expect(labelBounds.x).toBeCloseTo(headingBounds.x, 0)
  }
})

test('Settings tabs keep direct links and keyboard focus visible on mobile', async ({
  page,
  kata,
}) => {
  await page.setViewportSize({ width: 390, height: 800 })
  await kata.launch(page)
  await kata.loginOwner(page, '/kata?view=settings&section=access')
  const tabs = page.getByRole('tablist', { name: 'Settings' })
  const selected = page.getByRole('tab', { name: 'Teams and visibility' })
  await expect(selected).toHaveAttribute('aria-selected', 'true')
  const strip = (await tabs.boundingBox())!
  const tab = (await selected.boundingBox())!
  const view = (await page.locator('.settings-view').boundingBox())!
  expect(strip.x).toBeCloseTo(view.x, 0)
  expect(strip.x + strip.width).toBeCloseTo(view.x + view.width, 0)
  expect(tab.x).toBeGreaterThanOrEqual(strip.x + 15)
  expect(tab.x + tab.width).toBeLessThanOrEqual(strip.x + strip.width - 15)
  const first = page.getByRole('tab', { name: 'Appearance' })
  const last = page.getByRole('tab', { name: 'Teams and visibility' })
  await first.click()
  await expect(page).toHaveURL(/view=settings&section=appearance/)
  await first.focus()
  await page.keyboard.press('End')
  await expect(last).toBeFocused()
  await expect(page).toHaveURL(/view=settings&section=appearance/)
  await expect(first).toHaveAttribute('aria-selected', 'true')
  const focusedStrip = (await tabs.boundingBox())!
  const focused = (await last.boundingBox())!
  expect(focused.x).toBeGreaterThanOrEqual(focusedStrip.x)
  expect(focused.x + focused.width).toBeLessThanOrEqual(focusedStrip.x + focusedStrip.width + 1)
  await page.keyboard.press('Enter')
  await expect(page).toHaveURL(/view=settings&section=access/)
  await expect(last).toHaveAttribute('aria-selected', 'true')
})

test('Settings selection updates keep keyboard focus in view', async ({ page, kata }) => {
  await page.setViewportSize({ width: 390, height: 800 })
  await kata.launch(page)
  await kata.loginOwner(page, '/kata?view=settings&section=access')
  const first = page.getByRole('tab', { name: 'Appearance' })
  const last = page.getByRole('tab', { name: 'Teams and visibility' })
  await expect(last).toHaveAttribute('aria-selected', 'true')

  await first.evaluate((tab) => {
    // Move focus before Svelte runs the effect for the new selection.
    tab.click()
    tab.focus()
    tab.dispatchEvent(new KeyboardEvent('keydown', { key: 'End', bubbles: true }))
  })
  // Let the deferred selection effect finish before measuring the focused tab.
  await page.evaluate(() => new Promise(requestAnimationFrame))
  await expect(last).toBeFocused()
  await expect(first).toHaveAttribute('aria-selected', 'true')
  const strip = (await page.getByRole('tablist', { name: 'Settings' }).boundingBox())!
  const focused = (await last.boundingBox())!
  expect(focused.x).toBeGreaterThanOrEqual(strip.x)
  expect(focused.x + focused.width).toBeLessThanOrEqual(strip.x + strip.width + 1)
})

test('Appearance changes the sidebar color', async ({ page, kata }) => {
  await page.emulateMedia({ colorScheme: 'dark' })
  await kata.launch(page)

  const sidebarColor = () =>
    page.locator('.kata-sidebar').evaluate((element) => getComputedStyle(element).backgroundColor)
  const darkColor = await sidebarColor()
  await page.getByRole('button', { name: 'Settings' }).click()
  await page.getByRole('tab', { name: /^Appearance/ }).click()
  await page.getByRole('radio', { name: 'Light', exact: true }).click()
  await expect.poll(sidebarColor).not.toBe(darkColor)
})

test('task filters stay inside a narrow list pane without overlapping', async ({ page, kata }) => {
  await page.setViewportSize({ width: 1440, height: 800 })
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Narrow pane example' })
  await page.goto(`${kata.origin}/kata?view=all-open&issue=${issue.uid}`)
  await page.getByRole('button', { name: 'Display' }).click()
  await page.getByRole('radio', { name: 'Beside list' }).click()
  await page.keyboard.press('Escape')
  await page.setViewportSize({ width: 900, height: 800 })

  const layout = await page.locator('.kata-search-panel').evaluate((panel) => {
    const panelBounds = panel.getBoundingClientRect()
    const toolbar = panel.querySelector('.kata-search-toolbar')
    const listHeader = document.querySelector('.issue-list .table-header')
    if (!toolbar || !listHeader) throw new Error('Task filters or list header are missing')
    const toolbarBounds = toolbar.getBoundingClientRect()
    const listHeaderBounds = listHeader.getBoundingClientRect()
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
      panelContainsToolbar: panelBounds.bottom >= toolbarBounds.bottom,
      panelBeforeList: panelBounds.bottom <= listHeaderBounds.top,
      controlsContained: controls.every(
        (control) =>
          control.left >= panelBounds.left &&
          control.right <= panelBounds.right &&
          control.top >= panelBounds.top &&
          control.bottom <= panelBounds.bottom,
      ),
      overlaps,
    }
  })

  expect(layout).toEqual({
    panelContainsToolbar: true,
    panelBeforeList: true,
    controlsContained: true,
    overlaps: false,
  })
})

test('the title fills space as columns and the list pane resize', async ({ page, kata }) => {
  await page.setViewportSize({ width: 1440, height: 800 })
  const credentials = await kata.launch(page)
  await kata.seedIssue(page, credentials, { title: 'Narrow saved column example' })
  await page.goto(`${kata.origin}/kata?view=all-open`)
  const heading = page.locator('.column-heading-title')
  const wideWidth = (await heading.boundingBox())!.width
  const handle = page.getByRole('separator', { name: 'Resize Due column' })
  await handle.focus()
  await page.keyboard.press('ArrowRight')
  await expect.poll(async () => (await heading.boundingBox())!.width).toBeLessThan(wideWidth)
  await page.getByRole('button', { name: /Narrow saved column example/ }).click()
  await page.getByRole('button', { name: 'Display' }).click()
  await page.getByRole('radio', { name: 'Beside list' }).click()
  await page.keyboard.press('Escape')
  await page.setViewportSize({ width: 900, height: 800 })

  await expect.poll(async () => (await heading.boundingBox())!.width).toBeLessThan(wideWidth)
  await expect(page.getByRole('button', { name: /Narrow saved column example/ })).toBeVisible()

  await page.evaluate(() =>
    localStorage.setItem('kata:column-widths/v1', JSON.stringify({ due: 480 })),
  )
  await page.reload()
  const dueHeading = page.locator('.column-heading-due')
  const displayedWidth = Math.round((await dueHeading.boundingBox())!.width)
  expect(displayedWidth).toBeLessThan(480)
  await handle.focus()
  await expect(handle).toHaveAttribute('aria-valuenow', String(displayedWidth))
  await page.keyboard.press('ArrowLeft')
  await expect(handle).toHaveAttribute('aria-valuenow', String(displayedWidth - 8))
  expect(Math.round((await dueHeading.boundingBox())!.width)).toBe(displayedWidth - 8)
})

test('task rows stay 36px tall at desktop and mobile widths, including hover', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  await kata.seedIssue(page, credentials, { title: 'Compact example task' })
  await page.goto(`${kata.origin}/kata?view=all-open`)
  const row = page.getByRole('button', { name: /Compact example task/ })

  for (const width of [1440, 1024, 600]) {
    await page.setViewportSize({ width, height: 800 })
    await expect(row).toBeVisible()
    expect(await row.evaluate((element) => element.getBoundingClientRect().height)).toBe(36)
    await row.hover()
    expect(await row.evaluate((element) => element.getBoundingClientRect().height)).toBe(36)
  }
})

test('the task list and pane stay aligned beside an open task', async ({ page, kata }) => {
  await page.setViewportSize({ width: 1440, height: 800 })
  const credentials = await kata.launch(page)
  await kata.seedIssue(page, credentials, {
    title: 'Add retries to the task worker',
    priority: 1,
    metadata: { deadline_on: '2026-11-25' },
  })
  await kata.seedIssue(page, credentials, { title: 'Pin it' })
  await page.goto(`${kata.origin}/kata?view=all-open`)
  const row = page.getByRole('button', { name: /Add retries to the task worker/ })

  const search = (await page.locator('.kata-search-toolbar .kit-search-input').boundingBox())!
  const listTitle = (await page
    .getByRole('heading', { name: 'All open', exact: true })
    .boundingBox())!
  expect(Math.abs(search.x - listTitle.x)).toBeLessThan(1)
  for (const column of ['due', 'updated', 'priority']) {
    const header = (await page.locator(`.column-heading-${column} .col`).boundingBox())!
    const cell = (await row.locator(`.cell-${column}`).boundingBox())!
    expect(Math.abs(header.x + header.width - cell.x - cell.width)).toBeLessThan(1)
  }

  await row.click()
  await expect(row).toHaveClass(/selected/)
  const title = row.locator('.title-text')
  const project = row.locator('.cell-project')
  await expect(project.locator('.project-mark')).toBeVisible()
  expect(await title.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true)
  expect((await project.boundingBox())!.width).toBeLessThanOrEqual(16)
  const shortMark = page
    .getByRole('button', { name: /Pin it/ })
    .locator('.cell-project .project-mark')
  expect((await shortMark.boundingBox())!.x).toBe((await project.boundingBox())!.x)
  const titleBox = (await title.boundingBox())!
  const titleHeading = (await page.locator('.column-heading-title .col span').boundingBox())!
  expect(Math.abs(titleHeading.x - titleBox.x)).toBeLessThan(1)
  for (const column of ['due', 'priority']) {
    const header = (await page.locator(`.column-heading-${column} .col`).boundingBox())!
    const cell = (await row.locator(`.cell-${column}`).boundingBox())!
    expect(Math.abs(header.x + header.width - cell.x - cell.width)).toBeLessThan(1)
  }

  const right = (box: { x: number; width: number } | null) => box!.x + box!.width
  const toolbar = right(await page.locator('.kata-search-toolbar').boundingBox())
  const displayRight = right(await page.getByRole('button', { name: 'Display' }).boundingBox())
  const selection = right(await row.boundingBox())
  expect(Math.abs(displayRight - toolbar)).toBeLessThan(1)
  expect(Math.abs(selection - toolbar)).toBeLessThan(1)
  const priority = right(await row.locator('.pri-pill').boundingBox())
  const heading = right(await page.locator('.column-heading-priority .col').boundingBox())
  expect(selection - priority).toBeGreaterThanOrEqual(8)
  expect(Math.abs(heading - right(await row.locator('.cell-priority').boundingBox()))).toBeLessThan(
    1,
  )
  const header = page.locator('.table-header')
  const headerRight = await header.evaluate((element) => {
    const bounds = element.getBoundingClientRect()
    const divider = getComputedStyle(element, '::after')
    const inset = Number.parseFloat(divider.right)
    return divider.content === 'none' || !Number.isFinite(inset)
      ? bounds.right
      : bounds.right - inset
  })
  expect(Math.abs(headerRight - selection)).toBeLessThan(1)
})

test('a narrow list names the Updated sort its hidden column applies', async ({ page, kata }) => {
  await page.setViewportSize({ width: 1440, height: 800 })
  const credentials = await kata.launch(page)
  await kata.seedIssue(page, credentials, { title: 'Sort hint example', priority: 2 })
  await page.goto(`${kata.origin}/kata?view=all-open`)
  const header = page.locator('.table-header')
  const updated = header.getByText('Updated', { exact: true }).filter({ visible: true })
  // At full width the Updated column shows the sort itself.
  await expect(updated).toHaveCount(1)
  await expect(header.getByRole('button', { name: /^Sort by Updated, currently/ })).toBeVisible()

  await page.getByRole('button', { name: /Sort hint example/ }).click()
  await expect(header.getByRole('button', { name: /^Sort by Updated/ })).toBeHidden()
  await expect(updated).toHaveCount(1)
  const hint = (await updated.boundingBox())!
  const title = (await header.locator('.column-heading-title').boundingBox())!
  const titleLabel = (await header.locator('.column-heading-title .col-title span').boundingBox())!
  const sortGap = hint.x - (titleLabel.x + titleLabel.width)
  expect(sortGap).toBeGreaterThan(4)
  expect(sortGap).toBeLessThan(24)
  expect(hint.x + hint.width).toBeLessThanOrEqual(title.x + title.width)

  await header.getByRole('button', { name: 'Sort by Due' }).click()
  await expect(updated).toHaveCount(0)
  await expect(header.getByRole('button', { name: /^Sort by Due, currently/ })).toBeVisible()
})

test('the task count sits beside the list title', async ({ page, kata }) => {
  await page.setViewportSize({ width: 1440, height: 800 })
  const credentials = await kata.launch(page)
  await kata.seedIssue(page, credentials, { title: 'Counted example task' })
  await page.goto(`${kata.origin}/kata?view=all-open`)

  const list = page.getByRole('region', { name: 'Issues' })
  const title = (await list.getByRole('heading', { name: 'All open' }).boundingBox())!
  const count = (await list.locator('.count').boundingBox())!

  expect(count.y).toBeLessThan(title.y + title.height)
  expect(count.y + count.height).toBeGreaterThan(title.y)
  expect(count.x).toBeGreaterThan(title.x + title.width)
  expect(count.x - (title.x + title.width)).toBeLessThan(40)
})

test('the navigation sidebar resizes and collapses, and keeps both across reloads', async ({
  page,
  kata,
}) => {
  await page.setViewportSize({ width: 1440, height: 800 })
  await kata.launch(page)
  const sidebar = page.locator('.kata-sidebar')
  const handle = page.getByRole('separator', { name: 'Resize sidebar' })
  const startWidth = (await sidebar.boundingBox())!.width

  const grip = (await handle.boundingBox())!
  await page.mouse.move(grip.x + grip.width / 2, grip.y + grip.height / 2)
  await page.mouse.down()
  await page.mouse.move(grip.x + grip.width / 2 + 120, grip.y + grip.height / 2, { steps: 5 })
  await page.mouse.up()
  await expect.poll(async () => (await sidebar.boundingBox())!.width).toBe(startWidth + 120)

  await page.getByRole('button', { name: 'Collapse sidebar' }).click()
  await expect(sidebar).toBeHidden()
  await page.reload()
  await expect(page.getByRole('button', { name: 'Expand sidebar' })).toBeVisible()
  await expect(sidebar).toBeHidden()

  await page.getByRole('button', { name: 'Expand sidebar' }).click()
  await expect(page.getByRole('button', { name: 'Today' })).toBeVisible()
  await expect.poll(async () => (await sidebar.boundingBox())!.width).toBe(startWidth + 120)
})
