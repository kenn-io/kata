import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('views, projects, filters, columns, hierarchy, and keyboard stay first-class', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const parent = await kata.seedIssue(page, credentials, {
    title: 'Example parent task',
    owner: 'user-a',
    labels: ['planning'],
    priority: 1,
  })
  await kata.seedIssue(page, credentials, {
    title: 'Example child task',
    owner: 'user-b',
    labels: ['delivery'],
    links: [{ type: 'parent', to_ref: parent.uid }],
  })

  await expect(page.getByRole('button', { name: /Example parent task/ })).toBeVisible()
  await expect(page.getByRole('button', { name: /Example child task/ })).toHaveCount(0)
  await page.getByRole('button', { name: /Example parent task/ }).press('ArrowRight')
  await expect(page.getByRole('button', { name: /Example child task/ })).toBeVisible()

  await page.getByRole('button', { name: 'Columns' }).click()
  await expect(page.getByText('Shown when space allows')).toBeVisible()
  await expect(page.getByRole('checkbox', { name: 'Owner' })).toBeChecked()
  await page.keyboard.press('Escape')

  const parentRow = page.getByRole('button', { name: /Example parent task/ })
  await parentRow.focus()
  await parentRow.press('ArrowDown')
  await expect(page.getByRole('button', { name: /Example child task/ })).toBeFocused()

  await page.getByRole('searchbox', { name: 'Search tasks' }).fill('child')
  await expect(page).toHaveURL(/text=child/)
  await expect(page.getByRole('button', { name: /Example child task/ })).toBeVisible()

  for (const view of ['Inbox', 'Today', 'Upcoming', 'Deadlines', 'All Open', 'Logbook']) {
    await page.getByRole('button', { name: new RegExp(`^${view}\\b`) }).click()
    await expect(page.locator('[aria-label="Issues"]')).toBeVisible()
  }
  await page.getByRole('button', { name: /^example-project \d+$/ }).click()
  await expect(page).toHaveURL(`${kata.origin}/kata?scope=${kata.projectUID}`)
})

test('project creation and quick capture use the designated inbox project', async ({
  page,
  kata,
}) => {
  await kata.launch(page)

  await page.getByRole('button', { name: 'New project' }).click()
  await page.getByRole('textbox', { name: 'New project name' }).fill('example-workspace')
  await page.getByRole('textbox', { name: 'New project name' }).press('Enter')
  await expect(page.getByRole('button', { name: /^example-workspace\b/ })).toBeVisible()

  await page.getByRole('button', { name: 'New task' }).click()
  await page.getByRole('textbox', { name: 'Quick capture' }).fill('Captured example task')
  await page.getByRole('textbox', { name: 'Quick capture' }).press('Enter')
  await expect(page.getByRole('button', { name: /Captured example task/ })).toBeVisible()
})
