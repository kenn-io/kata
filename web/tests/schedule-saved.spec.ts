import { expect, test } from './fixtures'

test('saved schedule stays visible in the date editor after reload', async ({
  page,
  kata,
}, testInfo) => {
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, {
    title: 'Scheduled example',
    metadata: { scheduled_on: '2026-11-12' },
  })
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
  await page.getByRole('button', { name: 'Edit issue' }).click()
  const scheduled = page.getByRole('button', { name: 'Edit scheduled' })
  await expect(scheduled).toContainText('Nov 12')
  await scheduled.click()
  const editor = page.getByRole('button', { name: /Scheduled: Nov 12/ })
  await expect(editor).toBeVisible()
  expect(await editor.boundingBox()).not.toBeNull()
  await page.screenshot({ path: testInfo.outputPath('schedule.png') })
  await page.reload()
  await page.getByRole('button', { name: 'Edit issue' }).click()
  await expect(page.getByRole('button', { name: 'Edit scheduled' })).toContainText('Nov 12')
  expect(errors).toEqual([])
})
