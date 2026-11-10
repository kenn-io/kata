import { expect, test } from './fixtures'

test.use({ timezoneId: 'Asia/Tokyo' })

test('saved schedules use their zone and expose the due date to the editor', async ({
  page,
  kata,
}) => {
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
  await expect(page.getByText('Scheduled: 2025-12-31')).toBeVisible()
  await expect(page.getByText('Deadline: 2025-12-31')).toBeVisible()
  await page.getByRole('button', { name: 'Edit issue' }).click()
  const scheduled = page.getByRole('button', { name: 'Edit scheduled' })
  await expect(scheduled).toContainText('Dec 31')
  await expect(page.getByRole('button', { name: 'Edit due date' })).toContainText('Dec 31')
  await scheduled.click()
  await page.getByRole('button', { name: /Scheduled: Dec 31/ }).click()
  const selected = page.getByRole('dialog', { name: 'Scheduled' }).locator('[aria-pressed="true"]')
  await expect(selected).toHaveText('31')
  const writes: string[] = []
  page.on('request', (request) => {
    if (request.method() === 'PATCH' && request.url().endsWith('/metadata'))
      writes.push(request.url())
  })
  await selected.click()
  await expect(scheduled).toBeVisible()
  expect(writes).toEqual([])
  await page.getByRole('button', { name: 'Edit due date' }).click()
  await expect(page.getByRole('button', { name: /Due: Dec 31/ })).toBeVisible()
  expect(errors).toEqual([])
})
