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

test('creates, edits, and stops a repeating task from the project header', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Recurring example task' })
  await page.goto(`${kata.origin}/kata?view=all-open&scope=${kata.projectUID}&issue=${issue.uid}`)
  await page.getByRole('button', { name: 'Repeats…' }).click()
  const create = page.getByRole('dialog', { name: 'New recurrence' })
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
