import type { Locator, Page } from '@playwright/test'

import { expect, test } from './fixtures'

const snippet = 'const answer = 42'
const literal = '<img src="missing" onerror="alert(1)"> & literal'
const markdown = [
  '```ts',
  snippet,
  '```',
  '',
  '```unknown-example-language',
  literal,
  '```',
  '',
  '```',
  literal,
  '```',
  '',
  '`inline example`',
].join('\n')

test('fenced code highlights in descriptions and comments under the production CSP', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, {
    title: 'Code example task',
    body: markdown,
  })
  const response = await kata.request(
    page,
    credentials,
    'POST',
    `/api/v1/projects/${kata.projectID}/issues/${issue.uid}/comments`,
    { actor: 'example-author', body: markdown },
  )
  expect(response.ok()).toBe(true)
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)

  const detail = page.getByRole('region', { name: 'Kata issue detail', exact: true })
  const description = detail.locator('.kit-markdown').first()
  const comment = detail.locator('.comment-body').first()
  await expect(description).toBeVisible()
  await expect(comment).toBeVisible()
  await expectHighlighted(page, [description, comment])
  await expectPlainFallback([description, comment])

  await page.getByRole('button', { name: 'Edit issue', exact: true }).click()
  const editor = page.getByRole('region', { name: 'Kata issue editor', exact: true })
  const editableDescription = editor
    .getByRole('region', { name: 'Description', exact: true })
    .locator('.kit-markdown')
  const editableComment = editor.locator('.comment-body').first()
  await expect(editableDescription).toBeVisible()
  await expect(editableComment).toBeVisible()
  await expectHighlighted(page, [editableDescription, editableComment])
  await expectPlainFallback([editableDescription, editableComment])
})

async function expectHighlighted(page: Page, documents: Locator[]): Promise<void> {
  await page.emulateMedia({ colorScheme: 'light' })
  const lightColors: string[] = []
  for (const document of documents) {
    const code = document.locator('pre').first().locator('code')
    await expect(code).toHaveText(snippet)
    // Distinct computed colors prove the browser applied syntax styles.
    await expect
      .poll(() =>
        code
          .locator('span')
          .evaluateAll((spans) => new Set(spans.map((span) => getComputedStyle(span).color)).size),
      )
      .toBeGreaterThan(1)
    lightColors.push(
      await code
        .locator('span')
        .filter({ hasText: /^const$/ })
        .evaluate((span) => getComputedStyle(span).color),
    )
  }
  await page.emulateMedia({ colorScheme: 'dark' })
  for (const [index, document] of documents.entries()) {
    const code = document.locator('pre').first().locator('code')
    await expect
      .poll(() =>
        code
          .locator('span')
          .filter({ hasText: /^const$/ })
          .evaluate((span) => getComputedStyle(span).color),
      )
      .not.toBe(lightColors[index])
    await expect(code).toHaveText(snippet)
  }
}

async function expectPlainFallback(documents: Locator[]): Promise<void> {
  for (const document of documents) {
    const blocks = document.locator('pre code')
    await expect(blocks).toHaveCount(3)
    await expect(blocks.nth(1)).toHaveText(literal)
    await expect(blocks.nth(2)).toHaveText(literal)
    await expect(document.locator('img, script')).toHaveCount(0)
    const inline = document.locator('p > code')
    await expect(inline).toHaveText('inline example')
    await expect(inline.locator('span')).toHaveCount(0)
  }
}
