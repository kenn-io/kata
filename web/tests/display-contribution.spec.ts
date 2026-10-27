import { mkdir } from 'node:fs/promises'
import { join } from 'node:path'
import type { UISnapshot } from '../src/lib/state/snapshot'
import { expect, test, type BrowserCredentials } from './fixtures'

const captureRoot = process.env.KATA_DISPLAY_CAPTURE_DIR
const phase = process.env.KATA_DISPLAY_PHASE ?? 'after'
test.use({ viewport: { width: 1280, height: 1000 }, deviceScaleFactor: 2, reducedMotion: 'reduce' })

test('retained contribution survives refresh and fits the existing history and comment layouts', async ({
  page,
  kata,
}) => {
  test.setTimeout(120_000)
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, {
    title: 'Review display contribution',
    body: 'Read the original contributor after edits and refreshes.',
  })
  const created = await kata.request(
    page,
    credentials,
    'POST',
    `/api/v1/projects/${kata.projectID}/issues/${issue.uid}/comments`,
    { actor: 'coordinator', teammate: 'route', body: 'Original evidence' },
    { 'X-Kata-Teammate': 'writer' },
  )
  expect(created.ok()).toBe(true)
  const comment = (await created.json()).comment as {
    uid: string
    attributed_teammate?: string
    teammate?: string
  }
  expect(comment.attributed_teammate).toBe('writer')
  expect(comment.teammate).toBe('route')
  // Seed the edit with owner authority; owner-local tabs cannot edit comments.
  const login = await page.request.post(`${kata.origin}/api/v1/ui/session/login`, {
    headers: { Origin: kata.origin },
    data: { token: 'example-local-token', return_path: '/kata' },
  })
  expect(login.ok()).toBe(true)
  const owner = (await login.json()) as BrowserCredentials
  const edited = await kata.request(
    page,
    owner,
    'PATCH',
    `/api/v1/projects/${kata.projectID}/issues/${issue.uid}/comments/${comment.uid}`,
    { actor: 'editor', body: 'Revised evidence from the original contributor.' },
    { 'X-Kata-Teammate': 'editor' },
  )
  expect(edited.ok(), await edited.text()).toBe(true)
  expect((await edited.json()).comment.attributed_teammate).toBe('writer')
  await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
  const detail = page.locator('.shared-detail')
  if (phase === 'after') {
    await expect(detail.getByText('coordinator / writer', { exact: true })).toBeVisible()
    await page.reload()
    await expect(detail.getByText('coordinator / writer', { exact: true })).toBeVisible()
  }

  // Stable presentation fixtures follow the existing creation-attribution test:
  // the native read above proves hydration; this boundary isolates layout states.
  let empty = false
  await page.route('**/api/v1/ui/snapshot?*', async (route) => {
    const response = await route.fetch()
    if (response.status() !== 200) return route.fulfill({ response })
    const snapshot = (await response.json()) as UISnapshot
    if (snapshot.selected?.issue?.uid === issue.uid) {
      for (const candidate of [snapshot.selected.issue, ...(snapshot.collection ?? [])]) {
        if (candidate.uid === issue.uid) {
          candidate.short_id = 'abcd'
          candidate.qualified_id = 'example-project#abcd'
          candidate.created_at = '2026-08-01T12:00:00Z'
          candidate.updated_at = '2026-08-01T12:05:00Z'
        }
      }
      snapshot.selected.comments = empty
        ? []
        : [
            {
              id: 1,
              uid: '01K00000000000000000000001',
              issue_id: issue.id,
              handle: 'c:000001',
              author: 'coordinator',
              teammate: 'route',
              attributed_teammate: 'writer',
              body: 'Revised evidence from the original contributor.',
              created_at: '2026-08-01T12:00:00Z',
              edited_at: '2026-08-01T12:05:00Z',
              backlinks: [
                {
                  uid: '01K00000000000000000000004',
                  kind: 'confirm',
                  handle: 'example-project#abcd:000004',
                  status: 'moved',
                  author: 'reviewer',
                  teammate: 'route-review',
                  attributed_teammate: 'checker',
                  body: 'The moved response still identifies its creator.',
                  created_at: '2026-08-01T12:04:00Z',
                },
              ],
            },
            {
              id: 2,
              uid: '01K00000000000000000000002',
              issue_id: issue.id,
              handle: 'c:000002',
              author: 'reviewer',
              teammate: 'legacy',
              body: 'Legacy explicit teammate remains readable.',
              created_at: '2026-08-01T12:01:00Z',
            },
            {
              id: 3,
              uid: '01K00000000000000000000003',
              issue_id: issue.id,
              handle: 'c:000003',
              author: 'coordinator',
              teammate: 'route',
              attributed_teammate: '',
              body: 'Invalid display data has no contributor suffix.',
              created_at: '2026-08-01T12:02:00Z',
            },
          ]
      const event = {
        origin_instance_uid: 'instance-a',
        project_id: kata.projectID,
        project_uid: kata.projectUID,
        project_name: 'example-project',
        issue_uid: issue.uid,
        actor: 'coordinator',
        content_hash: 'fixture',
        hlc_physical_ms: 1,
        hlc_counter: 0,
      }
      snapshot.selected.history = empty
        ? []
        : [
            {
              ...event,
              id: 1,
              uid: 'history-created',
              type: 'issue.created',
              attributed_teammate: 'planner',
              payload: '{}',
              created_at: '2026-08-01T12:00:00Z',
            },
            {
              ...event,
              id: 2,
              uid: 'history-comment',
              type: 'issue.commented',
              attributed_teammate: 'writer',
              payload: '{}',
              created_at: '2026-08-01T12:01:00Z',
            },
            {
              ...event,
              id: 3,
              uid: 'history-edited',
              type: 'issue.comment_edited',
              attributed_teammate: 'editor',
              payload: '{}',
              created_at: '2026-08-01T12:05:00Z',
            },
            {
              ...event,
              id: 4,
              uid: 'history-closed',
              type: 'issue.closed',
              attributed_teammate: 'reviewer',
              payload: JSON.stringify({
                transcript: {
                  session_id: '00000000-0000-4000-8000-000000000001',
                  url: 'https://sessions.example/00000000-0000-4000-8000-000000000001',
                },
              }),
              created_at: '2026-08-01T12:06:00Z',
            },
          ]
    }
    await route.fulfill({ response, json: snapshot })
  })
  if (captureRoot) await mkdir(captureRoot, { recursive: true })
  for (const theme of ['light', 'dark'] as const) {
    await page.emulateMedia({ colorScheme: theme })
    for (const state of ['populated', 'narrow', 'empty'] as const) {
      empty = state === 'empty'
      const width = state === 'narrow' ? 390 : 1280
      await page.setViewportSize({ width, height: 1000 })
      await page.goto(`${kata.origin}/kata?issue=${issue.uid}`)
      await expect(
        detail.getByRole('heading', { name: 'Review display contribution' }),
      ).toBeVisible()
      await expect(detail.locator('.events')).toBeVisible()
      if (phase === 'after' && !empty) {
        await expect(detail.getByText('coordinator / writer', { exact: true })).toBeVisible()
        await expect(detail.locator('.event-label').first()).toContainText('coordinator / planner')
        const row = detail.locator('.event-row').first()
        const layout = await row.evaluate((node) => {
          const style = getComputedStyle(node)
          return { display: style.display, gap: style.gap, minHeight: style.minHeight }
        })
        expect(layout).toEqual({ display: 'grid', gap: '8px', minHeight: '24px' })
        await expect(detail.getByRole('link', { name: /Session:/ })).toHaveCount(1)
      }
      if (!empty) {
        await detail.getByRole('button', { name: 'Show 1 confirmation reply' }).click()
        if (phase === 'after')
          await expect(detail.getByText('reviewer / checker', { exact: true })).toBeVisible()
      }
      if (empty) await expect(detail.getByText('No events', { exact: true })).toBeVisible()
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(
        width,
      )
      if (captureRoot) {
        await page.evaluate(async () => {
          await document.fonts.ready
        })
        const history = detail.locator('.events')
        const historyPath = join(captureRoot, `web-${phase}-${theme}-${state}-history.png`)
        if (state === 'narrow') {
          await history.scrollIntoViewIfNeeded()
          const bounds = await history.boundingBox()
          expect(bounds).not.toBeNull()
          // The larger, measured after-layout defines the shared before/after crop.
          await page.screenshot({ path: historyPath, clip: { ...bounds!, height: 155 } })
        } else {
          await history.screenshot({ path: historyPath })
        }
        await detail.locator('[aria-labelledby="kata-comments-heading"]').screenshot({
          path: join(captureRoot, `web-${phase}-${theme}-${state}-comments.png`),
        })
      }
      if (!empty && state !== 'narrow') {
        await detail.getByRole('button', { name: 'Edit issue' }).click()
        const editor = page.getByRole('region', { name: 'Kata issue editor', exact: true })
        await expect(editor.getByLabel('Comment', { exact: true })).toBeVisible()
        if (phase === 'after')
          await expect(editor.getByText('coordinator / writer', { exact: true })).toBeVisible()
        if (captureRoot)
          await editor
            .locator('.comments')
            .screenshot({ path: join(captureRoot, `web-${phase}-${theme}-editor.png`) })
      }
    }
  }
})
