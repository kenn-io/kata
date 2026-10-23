import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('direct loopback tabs create independent browser sessions', async ({ browser, kata }) => {
  const firstContext = await browser.newContext()
  const firstPage = await firstContext.newPage()
  await firstPage.goto(`${kata.origin}/kata?view=all-open`)
  await expect(firstPage.getByRole('button', { name: 'New task' })).toBeVisible()

  const secondContext = await browser.newContext()
  const secondPage = await secondContext.newPage()
  await secondPage.goto(`${kata.origin}/kata?view=all-open`)
  await expect(secondPage.getByRole('button', { name: 'New task' })).toBeVisible()

  await firstContext.close()
  await secondContext.close()
})

test('loopback launch stores tab credentials and reaches writable authority', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  expect(credentials.session).not.toEqual(credentials.csrf)
  const snapshot = await kata.snapshot(page, credentials)
  expect(snapshot).toEqual(expect.objectContaining({ capabilities: expect.any(Object) }))
})
