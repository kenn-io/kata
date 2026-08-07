import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('switches between configured daemons without leaving the Kata workspace', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  await kata.seedIssue(page, credentials, { title: 'Local daemon task' })
  await page.reload()
  await expect(page.getByRole('button', { name: /Local daemon task/ })).toBeVisible()

  await page.getByRole('button', { name: 'Switch Kata daemon: example-local' }).click()
  await page.getByRole('menuitemradio', { name: /example-remote/ }).click()

  await expect(page.getByRole('button', { name: /Remote daemon task/ })).toBeVisible()
  await expect(page.getByRole('button', { name: /Local daemon task/ })).toHaveCount(0)
  expect(new URL(page.url()).origin).toBe(kata.origin)
})
