import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('detail editing and close reasons round-trip through live Kata authority', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, {
    title: 'Editable example task',
    body: 'Original description',
  })
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)

  const title = page
    .getByRole('region', { name: 'Task detail' })
    .getByRole('heading', { name: 'Editable example task' })
  await title.getByRole('button', { name: 'Edit title' }).click()
  await page.getByRole('textbox', { name: 'Edit title' }).fill('Edited example task')
  await page.getByRole('textbox', { name: 'Edit title' }).press('Enter')
  await expect(page.getByRole('region', { name: 'Task detail' })).toContainText(
    'Edited example task',
  )

  await page.getByRole('button', { name: 'Edit description' }).click()
  await page.getByRole('textbox', { name: 'Edit description' }).fill('Updated description')
  await page.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByRole('region', { name: 'Description' })).toContainText(
    'Updated description',
  )

  await page.getByRole('button', { name: 'Complete…' }).click()
  const dialog = page.getByRole('dialog', { name: 'Complete task' })
  await dialog
    .getByLabel('Completion note')
    .fill('Completed the example workflow and confirmed the browser behavior works as intended.')
  await dialog.getByLabel('Evidence value').fill('go test ./internal/example')
  await dialog.getByRole('button', { name: 'Complete' }).click()
  await expect(page.getByRole('button', { name: 'Reopen' })).toBeVisible()

  await page.getByRole('button', { name: 'Reopen' }).click()
  await expect(page.getByRole('button', { name: 'Complete…' })).toBeVisible()
  await page.getByRole('button', { name: 'Complete…' }).click()
  const wontDo = page.getByRole('dialog', { name: 'Complete task' })
  await wontDo.getByRole('radio', { name: /Won't do/ }).click()
  await wontDo
    .getByLabel('Completion note')
    .fill('This example task is no longer needed after the completed workflow was verified.')
  await wontDo.getByRole('button', { name: 'Complete' }).click()
  await expect(page.getByRole('button', { name: 'Reopen' })).toBeVisible()
})

test('comments, links, checklist, and history update without API fan-out', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const peer = await kata.seedIssue(page, credentials, { title: 'Related example task' })
  const issue = await kata.seedIssue(page, credentials, {
    title: 'Collaboration example task',
    metadata: { checklist: [] },
  })
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)

  await page.getByRole('textbox', { name: 'Comment' }).fill('A neutral collaboration note')
  await page.getByRole('button', { name: 'Add comment' }).click()
  await expect(page.getByText('A neutral collaboration note')).toBeVisible()

  await page.getByRole('button', { name: 'Add item' }).click()
  await page.getByRole('textbox', { name: 'New checklist item' }).fill('Verify example')
  await page.getByRole('textbox', { name: 'New checklist item' }).press('Enter')
  await expect(page.getByRole('checkbox', { name: 'Verify example' })).toBeVisible()

  await page.getByRole('region', { name: 'Links' }).getByRole('button', { name: 'Add' }).click()
  await page.getByRole('button', { name: 'Search tasks: Find task' }).click()
  await page.getByRole('combobox', { name: 'Search tasks' }).fill(peer.short_id)
  await page.getByRole('option', { name: /Related example task/ }).click()
  await expect(page.getByRole('region', { name: 'Links' })).toContainText(peer.short_id)
  await expect(page.getByRole('heading', { name: 'History' })).toBeVisible()
})

test('task status circle opens completion for the selected task', async ({ page, kata }) => {
  const credentials = await kata.launch(page)
  const first = await kata.seedIssue(page, credentials, { title: 'First task' })
  const second = await kata.seedIssue(page, credentials, { title: 'Second task' })
  await page.goto(`${kata.origin}/kata?issue=${first.uid}`)
  await page.getByRole('button', { name: `Complete ${second.short_id}` }).click()
  const dialog = page.getByRole('dialog', { name: 'Complete task' })
  await expect(dialog).toContainText('Second task')
  await dialog
    .getByLabel('Completion note')
    .fill('Completed the second task and checked the result in the browser.')
  await dialog.getByLabel('Evidence value').fill('browser check')
  await dialog.getByRole('button', { name: 'Complete' }).click()
  await expect(page.getByRole('button', { name: 'Reopen', exact: true })).toBeVisible()
  await page.goto(`${kata.origin}/kata?view=logbook`)
  await page.getByRole('button', { name: `Reopen ${second.short_id}` }).click()
  await expect(page.getByRole('button', { name: 'Complete…' })).toBeVisible()
})
