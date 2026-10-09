import type { UISnapshot } from '../src/lib/state/snapshot'
import { expect, test } from './fixtures'

test('creation attribution distinguishes legacy, pending and verified records at narrow widths', async ({
  page,
  kata,
}, testInfo) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Example attributed issue' })
  const comment = await kata.request(
    page,
    credentials,
    'POST',
    `/api/v1/projects/${kata.projectID}/issues/${issue.uid}/comments`,
    { actor: 'source-agent', teammate: 'example-worker', body: 'Example attributed comment' },
  )
  expect(comment.ok()).toBe(true)
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
  const detail = page.getByRole('region', { name: 'Kata issue detail' })
  // This first state comes from the native daemon, without response overrides.
  await expect(detail.getByText('Creation: legacy')).toHaveCount(2)
  await expect(detail).not.toContainText('Accountable:')

  // The remaining states exercise the browser's projection/rendering boundary.
  // Signed receipt verification is tested separately in the native backend suite.
  let verification: 'pending' | 'verified' = 'pending'
  await page.route('**/api/v1/ui/snapshot?*', async (route) => {
    const response = await route.fetch()
    if (response.status() !== 200) return route.fulfill({ response })
    const snapshot = (await response.json()) as UISnapshot
    const attribution = {
      verification,
      accountable_actor: 'example-member',
      source_actor: 'source-agent',
      teammate: 'example-worker',
    }
    for (const row of snapshot.collection ?? []) {
      if (row.uid === issue.uid) Object.assign(row, attribution)
    }
    if (snapshot.selected?.issue?.uid === issue.uid) {
      Object.assign(snapshot.selected.issue, attribution)
      for (const row of snapshot.selected.comments ?? []) Object.assign(row, attribution)
    }
    await route.fulfill({ response, json: snapshot })
  })
  for (const state of ['pending', 'verified'] as const) {
    verification = state
    await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
    await expect(detail.getByText(`Creation: ${state}`)).toHaveCount(2)
    await expect(detail.getByText('Source: source-agent / example-worker')).toHaveCount(2)
    if (state === 'verified') {
      await expect(detail.getByText('Accountable: example-member')).toHaveCount(2)
    } else {
      await expect(detail).not.toContainText('Accountable:')
      await expect(detail).not.toContainText('example-member')
    }
    for (const width of [1280, 390]) {
      await page.setViewportSize({ width, height: 1000 })
      await expect(detail.getByRole('heading', { name: 'Example attributed issue' })).toBeVisible()
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(
        width,
      )
      await page.screenshot({
        path: testInfo.outputPath(`creation-${state}-${width}.png`),
        fullPage: true,
      })
    }
  }
})
