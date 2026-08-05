import { readFile } from 'node:fs/promises'
import { join } from 'node:path'

import { expect, test } from '@playwright/test'

interface DevRuntime {
  publicOrigin: string
  runtimeFile: string
}

test.use({ trace: 'off' })

test('Vite proxy flushes authenticated SSE', async ({ page }) => {
  const runtime = JSON.parse(
    await readFile(join(process.cwd(), '..', '.kata-web-dev', 'active.json'), 'utf8'),
  ) as DevRuntime
  await page.goto(`${runtime.publicOrigin}/kata?view=all-open`)
  await expect(page.getByRole('button', { name: 'New task' })).toBeVisible()

  const credentials = await page.evaluate(() => {
    const value = sessionStorage.getItem('kata.web.session.v1')
    if (!value) return null
    const parsed = JSON.parse(value) as { session?: unknown; csrf?: unknown }
    return {
      session: typeof parsed.session === 'string' ? parsed.session : '',
      csrf: typeof parsed.csrf === 'string' ? parsed.csrf : '',
    }
  })
  expect(credentials?.session.length).toBeGreaterThan(0)
  expect(credentials?.csrf.length).toBeGreaterThan(0)

  const snapshotResponse = await page.request.get(
    `${runtime.publicOrigin}/api/v1/ui/snapshot?view=all-open`,
    { headers: { 'X-Kata-Web-Session': credentials!.session } },
  )
  expect(snapshotResponse.ok()).toBe(true)
  const snapshot = (await snapshotResponse.json()) as {
    catalog: Array<{ project: { id: number; name: string } }>
  }
  const project = snapshot.catalog.find(
    ({ project }) => project.name === 'example-project',
  )?.project
  expect(project).toBeTruthy()
  const mutationURL = `${runtime.publicOrigin}/api/v1/projects/${project!.id}/issues`
  const body = { title: 'Proxy stream issue', actor: 'user-a' }
  const baseHeaders = {
    Origin: runtime.publicOrigin,
    'X-Kata-Web-Session': credentials!.session,
    'Idempotency-Key': crypto.randomUUID(),
  }

  const missingCSRF = await page.request.post(mutationURL, { headers: baseHeaders, data: body })
  expect(missingCSRF.status()).toBe(403)
  expect(((await missingCSRF.json()) as { error: { code: string } }).error.code).toBe(
    'csrf_invalid',
  )

  const wrongOrigin = await page.request.post(mutationURL, {
    headers: {
      ...baseHeaders,
      Origin: 'http://daemon.example',
      'X-Kata-CSRF': credentials!.csrf,
      'Idempotency-Key': crypto.randomUUID(),
    },
    data: body,
  })
  expect(wrongOrigin.status()).toBe(403)
  expect(((await wrongOrigin.json()) as { error: { code: string } }).error.code).toBe(
    'origin_forbidden',
  )

  const accepted = await page.request.post(mutationURL, {
    headers: {
      ...baseHeaders,
      'X-Kata-CSRF': credentials!.csrf,
      'Idempotency-Key': crypto.randomUUID(),
    },
    data: body,
  })
  expect(accepted.ok()).toBe(true)
  const acceptedIssue = (await accepted.json()) as { issue: { uid: string } }

  await expect(page.locator(`[data-uid="${acceptedIssue.issue.uid}"]`)).toBeVisible({
    timeout: 10_000,
  })
})
