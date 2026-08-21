import { expect, test } from './fixtures'

test.use({ browserName: 'firefox', trace: 'off' })

test('direct loopback tab loads the workspace snapshot', async ({ page, kata }) => {
  await page.goto(`${kata.origin}/kata?view=all-open`)
  await expect(page.getByRole('button', { name: 'New task' })).toBeVisible()
})
