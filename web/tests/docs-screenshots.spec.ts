import { mkdir, writeFile } from 'node:fs/promises'
import { join } from 'node:path'

import { expect, test } from './fixtures'

const outputRoot = process.env.KATA_DOCS_SCREENSHOT_DIR

test.skip(!outputRoot, 'documentation screenshots run only through the screenshot generator')

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
  const child = await kata.seedIssue(page, credentials, {
    title: 'Document browser workflows',
    owner: 'user-b',
    labels: ['docs'],
    priority: 2,
    links: [{ type: 'parent', to_ref: parent.uid }],
  })
  const blocker = await kata.seedIssue(page, credentials, {
    title: 'Verify example packages',
    labels: ['release'],
    links: [{ type: 'blocks', to_ref: parent.uid }],
  })
  const related = await kata.seedIssue(page, credentials, {
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

  const replacements = [
    issueReplacements(parent, 'prep', '01K0000000000000000000001'),
    issueReplacements(child, 'docs', '01K0000000000000000000002'),
    issueReplacements(blocker, 'pkgs', '01K0000000000000000000003'),
    issueReplacements(related, 'news', '01K0000000000000000000004'),
    [{ generated: kata.projectUID, stable: '01K0000000000000000000000' }],
  ].flat()

  await page.goto(`${kata.origin}/kata?scope=${kata.projectUID}`)
  await expect(page.getByRole('button', { name: /Prepare example release/ })).toBeVisible()
  await page.getByRole('button', { name: /Prepare example release/ }).press('ArrowRight')
  await expect(page.getByRole('button', { name: /Document browser workflows/ })).toBeVisible()
  await expect(page.getByRole('button', { name: /^example-project \d+$/ })).toBeVisible()
  await capture(page, join(outputDir, 'workspace.png'), replacements)

  await page.goto(`${kata.origin}/kata?issue=${parent.uid}`)
  await page.getByRole('button', { name: 'Switch to side-by-side layout' }).click()
  await expect(page.getByRole('button', { name: 'Switch to stacked layout' })).toBeVisible()
  await page.evaluate(() => {
    document.documentElement.style.zoom = '0.875'
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
  await capture(page, join(outputDir, 'issue-detail.png'), replacements)

  const unlink = await kata.request(
    page,
    credentials,
    'PATCH',
    `/api/v1/projects/${kata.projectID}/issues/${parent.uid}`,
    { actor: 'user-a', links_delta: { remove_related: [related.uid] } },
  )
  expect(unlink.ok()).toBe(true)

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
  await capture(page, join(outputDir, 'relationships.png'), replacements)

  await page.goto(`${kata.origin}/kata?scope=${kata.projectUID}`)
  await page.getByRole('button', { name: 'Switch Kata daemon: example-local' }).click()
  await expect(page.getByRole('menuitemradio', { name: /example-local/ })).toBeVisible()
  await expect(page.getByRole('menuitemradio', { name: /example-remote/ })).toBeVisible()
  await capture(page, join(outputDir, 'daemon-switcher.png'), replacements)
})

interface CaptureReplacement {
  generated: string
  stable: string
}

function issueReplacements(
  issue: { uid: string; short_id: string },
  stableShortID: string,
  stableUID: string,
): CaptureReplacement[] {
  return [
    {
      generated: `example-project#${issue.short_id}`,
      stable: `example-project#${stableShortID}`,
    },
    { generated: issue.uid, stable: stableUID },
    { generated: issue.short_id, stable: stableShortID },
  ]
}

async function capture(
  page: import('@playwright/test').Page,
  path: string,
  replacements: CaptureReplacement[],
): Promise<void> {
  await page.mouse.move(1439, 959)
  await page.evaluate(async (dynamicReplacements) => {
    await document.fonts.ready
    window.scrollTo(0, 0)

    const ordered = dynamicReplacements.toSorted(
      (left, right) => right.generated.length - left.generated.length,
    )
    const normalize = (value: string): string =>
      ordered
        .reduce(
          (current, replacement) => current.replaceAll(replacement.generated, replacement.stable),
          value,
        )
        .replace(/\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z/g, '2026-08-08T12:00:00Z')

    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT)
    while (walker.nextNode()) {
      const node = walker.currentNode
      node.nodeValue = normalize(node.nodeValue ?? '')
    }
    for (const element of document.querySelectorAll<HTMLElement>('*')) {
      for (const attribute of element.getAttributeNames()) {
        const value = element.getAttribute(attribute)
        if (value) element.setAttribute(attribute, normalize(value))
      }
    }
    for (const element of document.querySelectorAll<HTMLElement>('.cell-updated')) {
      element.textContent = 'now'
      element.title = '2026-08-08T12:00:00Z'
    }
    for (const element of document.querySelectorAll<HTMLTimeElement>('time')) {
      element.textContent = 'just now'
      element.dateTime = '2026-08-08T12:00:00Z'
      element.title = 'Aug 8, 2026, 12:00 PM'
    }
  }, replacements)
  let previous: Buffer | undefined
  for (let attempt = 0; attempt < 6; attempt += 1) {
    await page.evaluate(
      () =>
        new Promise<void>((resolve) =>
          requestAnimationFrame(() => requestAnimationFrame(() => resolve())),
        ),
    )
    const current = await page.screenshot({ animations: 'disabled', caret: 'hide' })
    if (previous?.equals(current)) {
      await writeFile(path, current)
      return
    }
    previous = current
  }
  throw new Error(`documentation screenshot did not reach a stable frame: ${path}`)
}
