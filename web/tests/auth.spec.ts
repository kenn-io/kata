import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('direct loopback tabs create independent browser sessions', async ({ browser, kata }) => {
  const firstContext = await browser.newContext()
  const firstPage = await firstContext.newPage()
  const firstCredentials = await kata.launch(firstPage)

  const secondContext = await browser.newContext()
  const secondPage = await secondContext.newPage()
  const secondCredentials = await kata.launch(secondPage)

  expect(firstCredentials.session).not.toEqual(secondCredentials.session)
  expect(firstCredentials.csrf).not.toEqual(secondCredentials.csrf)

  await firstContext.close()
  await secondContext.close()
})
