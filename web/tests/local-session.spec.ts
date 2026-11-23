import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('token-configured direct loopback tab loads the workspace snapshot', async ({
  page,
  kata,
}) => {
  await page.goto(`${kata.origin}/kata?view=all-open`)
  await expect(page.getByRole('button', { name: 'New task' })).toBeVisible()
  const session = await page.evaluate(() =>
    JSON.parse(sessionStorage.getItem('kata.web.session.v1')!),
  )
  const response = await page.request.get(
    `${kata.origin}/api/v1/projects/${kata.projectID}/recurrences`,
    { headers: { 'X-Kata-Web-Session': session.session } },
  )
  expect(response.status()).toBe(200)
})
