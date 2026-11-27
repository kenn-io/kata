import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('a card opened on a phone fills the screen with a finger-sized close button', async ({
  page,
  kata,
}) => {
  await page.setViewportSize({ width: 390, height: 844 })
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Phone-sized board card' })
  await page.goto(`${kata.origin}/kata?scope=${kata.projectUID}&layout=board`)

  // The card's move menu needs a finger-sized target too.
  const move = page.getByRole('button', { name: `Actions for ${issue.title}` })
  await expect(move).toBeVisible()
  const moveBox = await move.boundingBox()
  expect(moveBox?.width).toBeGreaterThanOrEqual(44)
  expect(moveBox?.height).toBeGreaterThanOrEqual(44)

  await page.getByRole('button', { name: issue.title, exact: true }).click()
  const drawer = page.getByRole('dialog', { name: 'Issue detail' })
  await expect(drawer).toBeVisible()

  // Wait for the slide-in animation to finish before measuring.
  await expect
    .poll(async () => {
      const box = await drawer.boundingBox()
      return box && [box.x, box.y, box.width, box.height].map(Math.round)
    })
    .toEqual([0, 0, 390, 844])

  // 44×44 CSS pixels is the minimum comfortable touch target.
  const close = drawer.getByRole('button', { name: 'Close' })
  const closeBox = await close.boundingBox()
  expect(closeBox?.width).toBeGreaterThanOrEqual(44)
  expect(closeBox?.height).toBeGreaterThanOrEqual(44)

  // The list layout's divider must not show as a stray line inside the drawer.
  const divider = await drawer
    .locator('.detail-column')
    .evaluate((element) => getComputedStyle(element).borderLeftWidth)
  expect(divider).toBe('0px')
  // Full screen, the drawer's side-panel edge would also read as a stray line.
  expect(await drawer.evaluate((element) => getComputedStyle(element).borderLeftWidth)).toBe('0px')

  await close.click()
  await expect(drawer).toBeHidden()
})

test('a keyboard-focused board card shows the focus ring', async ({ page, kata }) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Focusable board card' })
  await page.goto(`${kata.origin}/kata?scope=${kata.projectUID}&layout=board`)

  const card = page.getByRole('button', { name: issue.title, exact: true })
  await expect(card).toBeVisible()
  await page.keyboard.press('Tab')
  await card.focus()
  await page.keyboard.press('ArrowDown')

  // An invalid outline value is dropped, leaving keyboard users no ring.
  await expect
    .poll(() => card.evaluate((element) => getComputedStyle(element).outlineStyle))
    .not.toBe('none')
})

test('the phone column strip keeps the column in view named on screen', async ({ page, kata }) => {
  await page.setViewportSize({ width: 390, height: 844 })
  const credentials = await kata.launch(page)
  await kata.seedIssue(page, credentials, { title: 'Phone strip card' })
  await page.goto(`${kata.origin}/kata?scope=${kata.projectUID}&layout=board`)
  const strip = page.getByRole('navigation', { name: 'Board columns' })
  await expect(strip.getByRole('button', { name: /^Open/ })).toHaveAttribute('aria-current', 'true')

  // Scroll the board, not the strip, all the way to Done.
  await page.locator('.board-columns').evaluate((columns) => {
    columns.scrollTo({ left: columns.scrollWidth, behavior: 'instant' })
  })

  const done = strip.getByRole('button', { name: /^Done/ })
  await expect(done).toHaveAttribute('aria-current', 'true')
  await expect
    .poll(() => done.evaluate((element) => element.getBoundingClientRect().right))
    .toBeLessThanOrEqual(390)
})
