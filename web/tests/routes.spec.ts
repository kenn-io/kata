import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('the root route visibly normalizes to Inbox', async ({ page, kata }) => {
  await kata.launch(page, '/')
  await expect(page).toHaveURL(`${kata.origin}/kata`)
  await expect(page.getByRole('heading', { name: 'Inbox', exact: true })).toBeVisible()
})

test('canonical routes survive reload and UID issue deep links', async ({ page, kata }) => {
  const credentials = await kata.launch(page, '/kata?view=all-open')
  const issue = await kata.seedIssue(page, credentials, { title: 'Deep link task' })

  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
  await expect(page.getByRole('region', { name: 'Task detail' })).toContainText('Deep link task')
  await page.reload()
  await expect(page).toHaveURL(`${kata.origin}/kata?issue=${issue.uid}`)
  await expect(page.getByRole('region', { name: 'Task detail' })).toContainText('Deep link task')
})

test('issue selection preserves the active Forge project scope', async ({ page, kata }) => {
  const credentials = await kata.launch(page, '/kata?view=all-open')
  const issue = await kata.seedIssue(page, credentials, { title: 'Scoped route task' })

  await page.getByRole('button', { name: /^example-project \d+$/ }).click()
  await expect(page).toHaveURL(`${kata.origin}/kata?scope=${kata.projectUID}`)
  await page.getByRole('button', { name: /Scoped route task/ }).click()

  await expect(page).toHaveURL(`${kata.origin}/kata?scope=${kata.projectUID}&issue=${issue.uid}`)
  await expect(page.getByRole('heading', { name: 'example-project', exact: true })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Task detail' })).toContainText('Scoped route task')
})

test('short issue references remain invalid routes with an explicit search recovery', async ({
  page,
  kata,
}) => {
  await kata.launch(page)
  const issue = await kata.seedIssue(page, await storedCredentials(page), {
    title: 'Short reference search task',
  })
  await page.goto(`${kata.origin}/kata?issue=${issue.short_id}`)
  await expect(page.getByRole('heading', { name: 'This Kata route is not valid' })).toBeVisible()
  await page.getByRole('button', { name: `Search for ${issue.short_id}` }).click()
  await expect(page).toHaveURL(
    new RegExp(`/kata\\?view=all-open&status=all&text=${encodeURIComponent(issue.short_id)}`),
  )
})

async function storedCredentials(page: import('@playwright/test').Page) {
  return page.evaluate(() =>
    JSON.parse(sessionStorage.getItem('kata.web.session.v1')!),
  ) as Promise<{
    session: string
    csrf: string
  }>
}
