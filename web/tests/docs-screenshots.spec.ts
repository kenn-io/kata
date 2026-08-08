import { mkdir } from 'node:fs/promises'
import { join } from 'node:path'

import { expect, test } from './fixtures'

const outputRoot = process.env.KATA_DOCS_SCREENSHOT_DIR

test.use({
  trace: 'off',
  colorScheme: 'dark',
  reducedMotion: 'reduce',
  viewport: { width: 1440, height: 960 },
})

test('captures stable Web UI documentation states from synthetic issues', async ({
  page,
  kata,
}) => {
  test.setTimeout(60_000)
  if (!outputRoot) throw new Error('KATA_DOCS_SCREENSHOT_DIR is required')
  const outputDir = join(outputRoot, 'web-ui')
  await mkdir(outputDir, { recursive: true })

  const credentials = await kata.launch(page)
  const parent = await kata.seedIssue(page, credentials, {
    title: 'Prepare example release',
    body: [
      '## Release checklist',
      '',
      'Coordinate the **example** rollout across the CLI and browser application.',
      '',
      '- Confirm the synthetic build',
      '- Review the public guide',
    ].join('\n'),
    owner: 'user-a',
    labels: ['release', 'planning'],
    priority: 1,
    metadata: { checklist: [] },
  })
  await kata.seedIssue(page, credentials, {
    title: 'Document browser workflows',
    owner: 'user-b',
    labels: ['docs'],
    priority: 2,
    links: [{ type: 'parent', to_ref: parent.uid }],
  })
  await kata.seedIssue(page, credentials, {
    title: 'Verify example packages',
    labels: ['release'],
    links: [{ type: 'blocks', to_ref: parent.uid }],
  })
  await kata.seedIssue(page, credentials, {
    title: 'Coordinate example announcement',
    labels: ['communication'],
    links: [{ type: 'related', to_ref: parent.uid }],
  })

  const recurrence = await kata.request(
    page,
    credentials,
    'POST',
    `/api/v1/projects/${kata.projectID}/recurrences`,
    {
      actor: 'user-a',
      rrule: 'FREQ=WEEKLY;COUNT=4',
      dtstart: '2026-08-03',
      timezone: 'America/Chicago',
      template: {
        title: 'Weekly example review',
        body: 'Review the neutral example release.',
        labels: ['routine'],
        metadata: {},
      },
    },
  )
  expect(recurrence.status()).toBe(201)

  await page.goto(`${kata.origin}/kata?scope=${kata.projectUID}`)
  await expect(page.getByRole('button', { name: /Prepare example release/ })).toBeVisible()
  await page.getByRole('button', { name: /Prepare example release/ }).press('ArrowRight')
  await expect(page.getByRole('button', { name: /Document browser workflows/ })).toBeVisible()
  await expect(page.getByRole('button', { name: /^example-project \d+$/ })).toBeVisible()
  await capture(page, join(outputDir, 'workspace.png'))

  await page.goto(`${kata.origin}/kata?issue=${parent.uid}`)
  await page.getByRole('button', { name: 'Switch to side-by-side layout' }).click()
  await expect(page.getByRole('button', { name: 'Switch to stacked layout' })).toBeVisible()
  await page.evaluate(() => {
    document.documentElement.style.zoom = '0.9'
  })
  await page
    .getByRole('textbox', { name: 'Comment' })
    .fill('The synthetic release is ready for documentation review.')
  await page.getByRole('button', { name: 'Add comment' }).click()
  await expect(
    page.getByText('The synthetic release is ready for documentation review.'),
  ).toBeVisible()
  await page.getByRole('button', { name: 'More actions' }).click()
  await page.getByRole('menuitem', { name: 'Add checklist' }).click()
  await page.getByRole('textbox', { name: 'New checklist item' }).fill('Verify example build')
  await page.getByRole('textbox', { name: 'New checklist item' }).press('Enter')
  await expect(page.getByRole('checkbox', { name: 'Verify example build' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Description' })).toContainText('Release checklist')
  await expect(page.getByRole('region', { name: 'Links' })).toContainText(
    /Document browser workflows|Verify example packages|Coordinate example announcement/,
  )
  await expect(page.getByRole('region', { name: 'Recurrences' })).toContainText(
    'Weekly example review',
  )
  await capture(page, join(outputDir, 'issue-detail.png'))

  await page.evaluate(() => {
    document.documentElement.style.zoom = ''
  })
  await page.getByRole('button', { name: 'Switch to stacked layout' }).click()
  await page.goto(`${kata.origin}/kata?issue=${parent.uid}&graph=1`)
  const graph = page.getByRole('region', { name: 'Reachable task graph' })
  await expect(graph).toBeVisible()
  await expect(graph.getByRole('button', { name: /Prepare example release/ })).toBeVisible()
  await expect(graph.getByRole('button', { name: /Document browser workflows/ })).toBeVisible()
  await expect(graph.getByRole('button', { name: /Verify example packages/ })).toBeVisible()
  await capture(page, join(outputDir, 'relationships.png'))

  await page.goto(`${kata.origin}/kata?scope=${kata.projectUID}`)
  await page.getByRole('button', { name: 'Switch Kata daemon: example-local' }).click()
  await expect(page.getByRole('menuitemradio', { name: /example-local/ })).toBeVisible()
  await expect(page.getByRole('menuitemradio', { name: /example-remote/ })).toBeVisible()
  await capture(page, join(outputDir, 'daemon-switcher.png'))
})

async function capture(page: import('@playwright/test').Page, path: string): Promise<void> {
  await page.evaluate(async () => {
    await document.fonts.ready
    window.scrollTo(0, 0)
  })
  await page.screenshot({ path, animations: 'disabled', caret: 'hide' })
}
