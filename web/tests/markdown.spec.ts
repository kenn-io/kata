import { expect, test } from './fixtures'

test('fenced code highlights in the read-only issue description under the production CSP', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, {
    title: 'Code example task',
    body: ['```ts', 'const answer = 42', '```'].join('\n'),
  })
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)

  const detail = page.getByRole('region', { name: 'Kata issue detail', exact: true })
  const code = detail.locator('.kit-markdown').first().locator('pre code')
  await expect(code).toBeVisible()
  await expect
    .poll(() =>
      code
        .locator('span')
        .evaluateAll((spans) => new Set(spans.map((span) => getComputedStyle(span).color)).size),
    )
    .toBeGreaterThan(1)
})
