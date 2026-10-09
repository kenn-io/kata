import { readFile } from 'node:fs/promises'
import { join } from 'node:path'
import { expect, test } from '@playwright/test'

test('default detail composes typed replies and follows a backlink to its comment', async ({
  page,
}) => {
  const runtime = JSON.parse(
    await readFile(join(process.cwd(), '..', '.kata-web-dev', 'active.json'), 'utf8'),
  ) as { publicOrigin: string }
  await page.goto(`${runtime.publicOrigin}/kata?view=all-open`)
  await expect(page.getByRole('button', { name: 'New task' })).toBeVisible()
  const credentials = await page.evaluate(
    () =>
      JSON.parse(sessionStorage.getItem('kata.web.session.v1')!) as {
        session: string
        csrf: string
      },
  )
  const headers = {
    Origin: runtime.publicOrigin,
    'X-Kata-Web-Session': credentials.session,
    'X-Kata-CSRF': credentials.csrf,
  }
  const snapshot = await page.request.get(
    `${runtime.publicOrigin}/api/v1/ui/snapshot?view=all-open`,
    { headers },
  )
  const catalog = (await snapshot.json()) as {
    catalog: Array<{ project: { id: number; name: string } }>
  }
  const pid = catalog.catalog.find((p) => p.project.name === 'example-project')!.project.id
  async function create(title: string) {
    const response = await page.request.post(
      `${runtime.publicOrigin}/api/v1/projects/${pid}/issues`,
      { headers, data: { title, actor: 'worker' } },
    )
    expect(response.ok()).toBe(true)
    return ((await response.json()) as { issue: { uid: string; short_id: string } }).issue
  }
  async function comment(
    issue: string,
    body: string,
    replyTo?: string,
    kind = 'reply',
    actor = 'worker',
  ) {
    const response = await page.request.post(
      `${runtime.publicOrigin}/api/v1/projects/${pid}/issues/${issue}/comments`,
      {
        headers,
        data: { actor, body, ...(replyTo ? { reply_to: replyTo, kind } : {}) },
      },
    )
    expect(response.ok()).toBe(true)
    return ((await response.json()) as { comment: { uid: string } }).comment
  }
  const rootIssue = await create('Comment link finding')
  const target = await comment(rootIssue.uid, 'Finding evidence')
  const replyIssue = await create('Comment link response')
  const inbound = await comment(replyIssue.uid, 'Cross-issue response', target.uid)
  await comment(
    replyIssue.uid,
    'I reproduced the finding using the documented steps and observed the same result.',
    target.uid,
    'confirm',
    'verifier',
  )
  await comment(
    replyIssue.uid,
    'I followed the same steps with the current build and could not reproduce this result.',
    target.uid,
    'refute',
    'investigator',
  )
  await comment(replyIssue.uid, 'Replacement evidence remains available.', target.uid, 'supersede')
  await page.goto(`${runtime.publicOrigin}/kata?issue=${rootIssue.uid}`)
  const detail = page.getByRole('region', { name: 'Kata issue detail', exact: true })
  const root = detail.locator(`#comment-${target.uid}`)
  await expect(root).toContainText('Finding evidence')
  await root.getByRole('button', { name: 'Confirm', exact: true }).click()
  const evidence = page.getByRole('textbox', { name: 'Reply evidence' })
  await evidence.fill('é'.repeat(39))
  await expect(page.getByRole('button', { name: 'Add comment', exact: true })).toBeDisabled()
  await evidence.fill('é'.repeat(40))
  const created = page.waitForResponse(
    (r) => r.request().method() === 'POST' && new URL(r.url()).pathname.endsWith('/comments'),
  )
  await page.getByRole('button', { name: 'Add comment', exact: true }).click()
  const response = await created
  expect(response.ok()).toBe(true)
  expect(response.request().postDataJSON()).toMatchObject({ reply_to: target.uid, kind: 'confirm' })
  await expect(detail.getByText('é'.repeat(40), { exact: true })).toBeVisible()
  await expect(root.getByRole('button', { name: 'Show 2 confirmation replies' })).toBeVisible()
  await expect(root.getByRole('button', { name: 'Show 1 refutation reply' })).toBeVisible()
  await expect(root.getByRole('button', { name: 'Show 1 superseding reply' })).toBeVisible()
  await root.getByRole('button', { name: 'Show 1 refutation reply' }).focus()
  await page.keyboard.press('Enter')
  await expect(root.getByRole('region', { name: 'Refutation replies', exact: true })).toContainText(
    'could not reproduce this result',
  )
  await root.getByRole('button', { name: 'Show 1 reply' }).click()
  await expect(root.getByRole('region', { name: 'Replies', exact: true })).toContainText(
    'Cross-issue response',
  )
  await root.getByRole('button', { name: new RegExp(`Open ${replyIssue.short_id}:`) }).click()
  await expect(detail.locator(`#comment-${inbound.uid}`)).toBeFocused()
  await expect(detail.locator(`#comment-${inbound.uid}`)).toContainText('Cross-issue response')
  await expect(detail.locator(`#comment-${inbound.uid}`)).toContainText('Replies to')
  await page.getByRole('button', { name: 'Return to original comment' }).click()
  await expect(detail.locator(`#comment-${target.uid}`)).toBeFocused()
  await page.setViewportSize({ width: 390, height: 844 })
  await expect(root.getByRole('button', { name: 'Show 2 confirmation replies' })).toBeVisible()
  await root.getByRole('button', { name: 'Show 2 confirmation replies' }).click()
  await page.screenshot({ path: '/tmp/comment-b-relations-browser.png', fullPage: true })
})
