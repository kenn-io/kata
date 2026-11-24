import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('the attention badge leaves a title as wide as an unbadged one on a phone', async ({
  page,
  kata,
}) => {
  await page.setViewportSize({ width: 390, height: 844 })
  const credentials = await kata.launch(page)
  const stuck = await kata.seedIssue(page, credentials, {
    title: 'Design the settings page layout',
    metadata: { 'work.attention': 'stuck', 'work.attention_msg': 'Waiting on a decision' },
  })
  const quiet = await kata.seedIssue(page, credentials, { title: 'Write the release notes' })
  await page.goto(`${kata.origin}/kata?view=all-open`)

  const stuckRow = page.locator(`[data-uid="${stuck.uid}"]`)
  await expect(stuckRow.locator('.attention-badge')).toHaveText('Stuck')
  const width = (uid: string) =>
    page
      .locator(`[data-uid="${uid}"] .title-text`)
      .evaluate((el) => Math.round(el.getBoundingClientRect().width))

  // The badge wraps below the title instead of squeezing it.
  expect(await width(stuck.uid)).toBeGreaterThanOrEqual(await width(quiet.uid))
})
