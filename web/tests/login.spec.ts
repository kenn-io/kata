import { expect, test } from '@playwright/test'

test('token login presents its controls as a vertical form', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'dark' })
  await page.route('**/api/v1/ui/session/local', async (route) => {
    await route.fulfill({ status: 404 })
  })
  await page.route('**/api/v1/ui/daemons', async (route) => {
    await route.fulfill({
      status: 401,
      headers: { 'X-Kata-Web-Authentication': 'login' },
    })
  })

  await page.goto('/kata')

  const label = page.getByText('Token', { exact: true })
  const input = page.getByLabel('Token')
  const button = page.getByRole('button', { name: 'Log in' })
  await expect(button).toBeVisible()

  const [labelBox, inputBox, buttonBox] = await Promise.all([
    label.boundingBox(),
    input.boundingBox(),
    button.boundingBox(),
  ])
  expect(labelBox).not.toBeNull()
  expect(inputBox).not.toBeNull()
  expect(buttonBox).not.toBeNull()
  expect(labelBox!.y + labelBox!.height).toBeLessThan(inputBox!.y)
  expect(inputBox!.y + inputBox!.height).toBeLessThan(buttonBox!.y)
})
