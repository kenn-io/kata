import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('production CSP permits the dynamic styles used by the application layout', async ({
  page,
  kata,
}) => {
  const violations: string[] = []
  page.on('console', (message) => {
    if (message.text().includes('Refused to apply inline style')) violations.push(message.text())
  })

  await kata.launch(page)

  const table = page.getByRole('region', { name: 'Issues' }).locator('.table')
  await expect(table).toBeVisible()
  await expect
    .poll(() =>
      table.evaluate((element) =>
        getComputedStyle(element).getPropertyValue('--table-cols-wide').trim(),
      ),
    )
    .not.toBe('')
  expect(violations).toEqual([])
})
