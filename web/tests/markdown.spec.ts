import type { Locator } from '@playwright/test'
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

test('Kenn links in issue bodies and comments work in read and edit views', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, {
    title: 'Links to Kenn',
    body: '[note](kenn://example/note)',
  })
  await kata.request(
    page,
    credentials,
    'POST',
    `/api/v1/projects/${kata.projectID}/issues/${issue.uid}/comments`,
    { actor: 'example-worker', body: 'See kenn://example/comment' },
  )
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)

  async function expectLinks(scope: Locator) {
    for (const [name, href] of [
      ['note', 'kenn://example/note'],
      ['kenn://example/comment', 'kenn://example/comment'],
    ]) {
      await expect(scope.getByRole('link', { name, exact: true })).toHaveAttribute('href', href)
    }
  }

  await expectLinks(page.getByRole('region', { name: 'Kata issue detail', exact: true }))
  await page.getByRole('button', { name: 'Edit issue' }).click()
  await expectLinks(page.getByRole('region', { name: 'Kata issue editor', exact: true }))
})
