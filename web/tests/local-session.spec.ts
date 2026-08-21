import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('token-configured direct loopback tab loads the workspace snapshot', async ({ page, kata }) => {
  await page.goto(`${kata.origin}/kata?view=all-open`)
  await expect(page.getByRole('button', { name: 'New task' })).toBeVisible()
})
