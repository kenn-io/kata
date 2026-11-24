import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('graph refits and preserves its size as the viewport changes', async ({ page, kata }) => {
  const credentials = await kata.launch(page)
  const source = await kata.seedIssue(page, credentials, { title: 'Graph source example' })
  await kata.seedIssue(page, credentials, {
    title: 'Graph child example',
    links: [{ type: 'parent', to_ref: source.uid }],
  })
  await page.goto(`${kata.origin}/kata?issue=${source.uid}&graph=1`)
  const graph = page.getByRole('region', { name: 'Reachable task graph' })
  await expect(graph).toBeVisible()
  await expect(graph.getByRole('button', { name: /Graph source example/ })).toBeVisible()
  await expect(graph.getByRole('button', { name: /Graph child example/ })).toBeVisible()
  const viewport = graph.locator('.svelte-flow__viewport')
  await expect(graph.locator('.svelte-flow__minimap')).toHaveCount(0)
  for (let i = 0; i < 3; i++) await graph.getByRole('button', { name: 'Zoom In' }).click()
  await expect(graph.locator('.svelte-flow__minimap')).toBeVisible()
  const zoomed = await viewport.getAttribute('style')
  await page.getByRole('separator', { name: 'Resize Kata panes' }).focus()
  await page.keyboard.press('ArrowLeft')
  await expect.poll(() => viewport.getAttribute('style')).not.toBe(zoomed)
  const primary = page.locator('.kata-sash .pane-primary')
  const draggedSize = (await primary.boundingBox())!.width
  const size = page.viewportSize()!
  await page.setViewportSize({ width: 750, height: size.height })
  await expect.poll(async () => (await primary.boundingBox())!.width).toBeLessThan(draggedSize)
  await page.setViewportSize(size)
  await expect.poll(async () => (await primary.boundingBox())!.width).toBe(draggedSize)
})

test('keeps zoom when opening a node without changing the graph layout', async ({ page, kata }) => {
  const credentials = await kata.launch(page)
  const source = await kata.seedIssue(page, credentials, { title: 'Graph source example' })
  const child = await kata.seedIssue(page, credentials, {
    title: 'Graph child example',
    links: [{ type: 'parent', to_ref: source.uid }],
  })
  await page.goto(`${kata.origin}/kata?issue=${source.uid}&graph=1`)
  const graph = page.getByRole('region', { name: 'Reachable task graph' })
  await expect(graph.getByRole('button', { name: /Graph child example/ })).toBeVisible()
  await graph.getByRole('button', { name: 'Zoom In' }).click({ clickCount: 2 })
  const viewport = graph.locator('.svelte-flow__viewport')
  const zoomed = await viewport.getAttribute('style')
  await graph.getByRole('button', { name: /Graph child example/ }).click()
  await expect(page).toHaveURL(`${kata.origin}/kata?issue=${child.uid}&graph=1`)
  await expect(graph.getByRole('button', { name: /selected.*Graph child example/ })).toBeVisible()
  await expect(viewport).toHaveAttribute('style', zoomed!)
})

test('creates, edits, and stops a repeating task from the project header', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Recurring example task' })
  await page.goto(`${kata.origin}/kata?view=all-open&scope=${kata.projectUID}&issue=${issue.uid}`)
  await page.getByRole('button', { name: 'Repeats…' }).click()
  const create = page.getByRole('dialog', { name: 'Repeat' })
  await create.getByLabel('Title', { exact: true }).fill('Weekly example review')
  await create.getByRole('button', { name: 'Save' }).click()
  const chip = page.getByRole('button', { name: /1 repeating task/ })
  await expect(chip).toBeVisible()
  await chip.click()
  await page.getByRole('button', { name: 'Edit', exact: true }).click()
  const edit = page.getByRole('dialog', { name: 'Edit recurrence' })
  await edit.getByLabel('Title', { exact: true }).fill('Updated weekly review')
  await edit.getByRole('button', { name: 'Save' }).click()
  await expect(edit).toBeHidden()
  await chip.click()
  await expect(page.getByText('Updated weekly review', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Stop repeating…' }).click()
  await page
    .getByRole('dialog', { name: 'Stop repeating' })
    .getByRole('button', { name: 'Stop repeating' })
    .click()
  await expect(chip).toHaveCount(0)
})

test('fits tasks around the key and controls in a short stacked pane', async ({ page, kata }) => {
  const credentials = await kata.launch(page)
  const source = await kata.seedIssue(page, credentials, { title: 'Graph root' })
  await kata.seedIssue(page, credentials, {
    title: 'Graph child',
    links: [{ type: 'parent', to_ref: source.uid }],
  })
  await page.goto(`${kata.origin}/kata?issue=${source.uid}&graph=1`)
  const graph = page.getByRole('region', { name: 'Reachable task graph' })
  await expect(graph.getByRole('button', { name: /Graph child/ })).toBeVisible()
  await graph.getByRole('button', { name: 'Switch to stacked layout' }).click()
  const handle = page.getByRole('separator', { name: 'Resize Kata panes' })
  await handle.focus()
  for (let i = 0; i < 20; i++) await page.keyboard.press('Shift+ArrowUp')
  await expect(handle).toHaveAttribute('aria-valuenow', '220')
  await graph.getByRole('button', { name: 'Fit View' }).click()
  await graph.scrollIntoViewIfNeeded()
  const nodes = graph.locator('.svelte-flow__node')
  const key = graph.getByRole('group', { name: 'Line key' })
  const controls = graph.locator('.svelte-flow__controls')
  await expect
    .poll(async () => {
      const overlays = await Promise.all([key.boundingBox(), controls.boundingBox()])
      const canvas = await graph.locator('.svelte-flow').boundingBox()
      const pane = await graph.boundingBox()
      const boxes = await nodes.evaluateAll((elements) =>
        elements.map((element) => {
          const { x, y, width, height } = element.getBoundingClientRect()
          return { x, y, width, height }
        }),
      )
      return (
        canvas &&
        pane &&
        canvas.y + canvas.height <= pane.y + pane.height &&
        boxes.every(
          (box) =>
            box.width >= 150 &&
            box.x >= canvas.x &&
            box.y >= canvas.y &&
            box.x + box.width <= canvas.x + canvas.width &&
            box.y + box.height <= canvas.y + canvas.height,
        ) &&
        overlays.every(
          (overlay) =>
            overlay &&
            boxes.every(
              (box) =>
                box.x >= overlay.x + overlay.width ||
                box.x + box.width <= overlay.x ||
                box.y >= overlay.y + overlay.height ||
                box.y + box.height <= overlay.y,
            ),
        )
      )
    })
    .toBe(true)
})
