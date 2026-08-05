import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('reachable graph preserves relationship context and returns to detail', async ({
  page,
  kata,
}) => {
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
  await graph.getByRole('button', { name: 'Back to task list' }).click()
  await expect(page).toHaveURL(`${kata.origin}/kata?issue=${source.uid}`)
})

test('recurrence authority is shown and can be deleted from issue detail', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Recurring example task' })
  const response = await kata.request(
    page,
    credentials,
    'POST',
    `/api/v1/projects/${kata.projectID}/recurrences`,
    {
      actor: 'user-a',
      rrule: 'FREQ=WEEKLY;COUNT=2',
      dtstart: '2026-08-03',
      timezone: 'America/Chicago',
      template: {
        title: 'Weekly example review',
        body: 'Review the neutral example.',
        labels: ['routine'],
        metadata: {},
      },
    },
  )
  expect(response.status()).toBe(201)
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
  await expect(page.getByRole('region', { name: 'Recurrences' })).toContainText(
    'Weekly example review',
  )
  await page.getByRole('button', { name: 'Delete recurrence' }).click()
  await page
    .getByRole('dialog', { name: 'Delete recurrence' })
    .getByRole('button', { name: 'Delete' })
    .click()
  await expect(page.getByText('Weekly example review')).toHaveCount(0)
})
