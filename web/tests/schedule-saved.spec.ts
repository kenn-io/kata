import { expect, test } from './fixtures'

test.use({ timezoneId: 'Asia/Tokyo' })

test('saved schedule stays visible in the date editor after reload', async ({
  page,
  kata,
}, testInfo) => {
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, {
    title: 'Scheduled example',
    metadata: {
      timezone: 'America/New_York',
      scheduled_on: '2026-01-01T02:00:00Z',
      deadline_on: '2026-01-01T02:00:00Z',
    },
  })
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
  await page.getByRole('button', { name: 'Edit issue' }).click()
  const scheduled = page.getByRole('button', { name: 'Edit scheduled' })
  await expect(scheduled).toContainText('Dec 31')
  await expect(page.getByRole('button', { name: 'Edit due date' })).toContainText('Dec 31')
  await scheduled.click()
  const editor = page.getByRole('button', { name: /Scheduled: 2026-01-01T02:00:00Z/ })
  await expect(editor).toBeVisible()
  expect(await editor.boundingBox()).not.toBeNull()
  await page.screenshot({ path: testInfo.outputPath('schedule.png') })
  await page.reload()
  await page.getByRole('button', { name: 'Edit issue' }).click()
  await expect(page.getByRole('button', { name: 'Edit scheduled' })).toContainText('Dec 31')
  expect(errors).toEqual([])
})
