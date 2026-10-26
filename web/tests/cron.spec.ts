import type { Page } from '@playwright/test'
import { expect, test } from './fixtures'

test.setTimeout(60_000)

const runUID = '01ARZ3NDEKTSV4RRFFQ69G5FAV'
const secondRunUID = '01ARZ3NDEKTSV4RRFFQ69G5FAW'
const jobUID = '01ARZ3NDEKTSV4RRFFQ69G5FAX'
const workflowUID = '01ARZ3NDEKTSV4RRFFQ69G5FAY'
const issueUID = '01ARZ3NDEKTSV4RRFFQ69G5FAZ'

// These are API response fixtures. The daemon, browser session, daemon switcher,
// snapshots, and credentialed fetch path are the production isolated runtime.
async function cronReads(page: Page, project: { projectID: number; projectUID: string }) {
  const run = {
    id: 1,
    uid: runUID,
    project_id: project.projectID,
    revision: 1,
    actor: 'reader',
    created_at: '2026-10-24T10:00:00Z',
    updated_at: '2026-10-24T10:01:00Z',
    status: 'failed',
    summary: { version: 1, message: 'Runner could not complete the inspection' },
    job_uid: jobUID,
    issue_uid: issueUID,
  }
  const job = {
    id: 1,
    uid: jobUID,
    name: 'Inspect workspace',
    project_id: project.projectID,
    definition_event_uid: workflowUID,
    definition_hlc: {},
    created_at: run.created_at,
    updated_at: run.updated_at,
    author: 'reader',
    revision: 1,
    last_run: run,
    definition: {
      version: 1,
      kind: 'job',
      enabled: true,
      trigger: { kind: 'interval', interval_seconds: 60 },
      action: { kind: 'execute', prompt: 'Inspect' },
      overlap: 'forbid',
      catchup: 'all',
      options: { tags: ['inspection'] },
    },
  }
  const workflow = {
    id: 1,
    uid: workflowUID,
    name: 'Inspection workflow',
    project_id: project.projectID,
    definition: {
      version: 1,
      about: 'Inspect and summarize',
      input: 'workspace',
      options: {},
      steps: [{ key: 'inspect', kind: 'command', command: 'echo inspection', retries: 0 }],
    },
  }
  await page.route('**/api/v1/projects/*/cron/**', async (route) => {
    const url = new URL(route.request().url()),
      tail = url.pathname.split('/cron/')[1] ?? ''
    // Reads addressed to the remote daemon reach its real, empty cron store.
    if (route.request().headers()['x-kata-web-daemon'] === 'example-remote') {
      await route.continue()
      return
    }
    const scope = Number(/projects\/(\d+)\/cron/.exec(url.pathname)?.[1])
    if (scope !== project.projectID) {
      await route.fulfill({
        json:
          tail === 'summary'
            ? { failed_runs_24h: 0 }
            : tail === 'jobs'
              ? { jobs: [] }
              : tail === 'workflows'
                ? { workflows: [] }
                : { runs: [] },
      })
      return
    }
    const body =
      tail === 'summary'
        ? { failed_runs_24h: 37, window_start: run.created_at, window_end: run.updated_at }
        : tail === 'jobs'
          ? { jobs: [job] }
          : tail.startsWith('jobs/')
            ? { job }
            : tail === 'workflows'
              ? { workflows: [workflow] }
              : tail.startsWith('workflows/')
                ? { workflow }
                : tail === 'runs'
                  ? {
                      runs: [
                        run,
                        {
                          ...run,
                          uid: secondRunUID,
                          issue_uid: undefined,
                          status: 'succeeded',
                          summary: { version: 1, message: 'Second run complete' },
                        },
                      ],
                      next_before_uid: '',
                    }
                  : {
                      run: {
                        ...run,
                        uid: tail.split('/')[1],
                        ...(tail.endsWith(secondRunUID)
                          ? {
                              issue_uid: undefined,
                              status: 'succeeded',
                              summary: { version: 1, message: 'Second run complete' },
                            }
                          : {}),
                      },
                    }
    await route.fulfill({ json: body })
  })
  await page.route(`**/api/v1/projects/*/issues/${issueUID}*`, (route) =>
    route.fulfill({ status: 403, json: { error: 'not readable' } }),
  )
  return { run, job, workflow }
}

test('cron reads and copied navigation', async ({ page, kata }, testInfo) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await kata.launch(page)
  await cronReads(page, kata)
  await page.goto(`${kata.origin}/kata?view=cron`)
  await expect(page.getByRole('heading', { name: 'Cron', exact: true })).toBeVisible()
  const pageBounds = await page.getByRole('region', { name: 'Cron', exact: true }).boundingBox()
  expect(pageBounds?.width).toBeGreaterThan(1100)
  await expect(page.getByRole('button', { name: 'Cron 37' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Inspect workspace', exact: true })).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('jobs.png'), fullPage: true })
  await page.getByRole('radio', { name: 'Jobs', exact: true }).focus()
  await page.keyboard.press('ArrowRight')
  await expect(page).toHaveURL(/cron_tab=runs/)
  await page.screenshot({ path: testInfo.outputPath('runs.png'), fullPage: true })
  await page
    .getByRole('row', { name: /^failed/ })
    .getByRole('button', { name: 'Inspect workspace', exact: true })
    .click()
  await expect(page.getByRole('dialog', { name: 'Run detail' })).toBeVisible()
  await expect(page.getByText('Issue not readable', { exact: true })).toBeVisible()
  await expect(
    page.getByText('Runner could not complete the inspection', { exact: true }),
  ).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('drawer.png'), fullPage: true })
  await page.keyboard.press('Escape')
  await expect(page.getByRole('dialog')).toHaveCount(0)
  await page.getByRole('radio', { name: 'Workflows', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Inspection workflow' })).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('workflows.png'), fullPage: true })
  await page.goBack()
  await expect(page.getByRole('radio', { name: 'Runs', exact: true })).toHaveAttribute(
    'aria-checked',
    'true',
  )
  await page.reload()
  await expect(page.getByRole('radio', { name: 'Runs', exact: true })).toHaveAttribute(
    'aria-checked',
    'true',
  )
  await expect(page.getByRole('row', { name: /^failed/ })).toBeVisible()
  await page.setViewportSize({ width: 390, height: 844 })
  await page.screenshot({ path: testInfo.outputPath('mobile.png'), fullPage: true })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(
    true,
  )
  await page
    .getByRole('row', { name: /^failed/ })
    .getByRole('button', { name: 'Inspect workspace', exact: true })
    .click()
  await expect(page.getByRole('dialog', { name: 'Run detail' })).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('mobile-drawer.png'), fullPage: true })
  await page.keyboard.press('Escape')
  await expect(page.getByRole('dialog')).toHaveCount(0)
})

test('old issue reads cannot populate a newer run drawer', async ({ page, kata }) => {
  await kata.launch(page)
  await cronReads(page, kata)
  let release!: () => void
  let issueRequested = false
  const delayed = new Promise<void>((resolve) => (release = resolve))
  await page.route(`**/api/v1/projects/*/issues/${issueUID}*`, async (route) => {
    issueRequested = true
    await delayed
    await route
      .fulfill({
        json: {
          issue: { uid: issueUID, title: 'Old issue' },
          comments: [
            {
              uid: issueUID,
              author: 'reader',
              created_at: '2026-10-24T10:00:00Z',
              body: 'Old comment',
            },
          ],
        },
      })
      .catch(() => {})
  })
  await page.goto(
    `${kata.origin}/kata?view=cron&cron_tab=runs&cron_kind=run&cron_scope=${kata.projectUID}&cron_uid=${runUID}`,
  )
  await expect(page.getByText('Loading latest on the issue…')).toBeVisible()
  await expect.poll(() => issueRequested).toBe(true)
  await page.keyboard.press('Escape')
  await page
    .getByRole('row', { name: /^succeeded/ })
    .getByRole('button', { name: 'Inspect workspace', exact: true })
    .click()
  await expect(page.getByText('Second run complete', { exact: true })).toBeVisible()
  release()
  await expect(page.getByText('Old comment', { exact: true })).toHaveCount(0)
  await expect(
    page.getByRole('dialog', { name: 'Run detail' }).getByText('No issue', { exact: true }).first(),
  ).toBeVisible()
})

test('a denied project keeps readable rows and marks the failure count partial', async ({
  page,
  kata,
}) => {
  await kata.launch(page)
  await cronReads(page, kata)
  await page.route('**/api/v1/projects/*/cron/**', async (route) => {
    const id = Number(/projects\/(\d+)\/cron/.exec(route.request().url())?.[1])
    if (id !== kata.projectID)
      await route.fulfill({ status: 403, json: { error: 'Project not readable' } })
    else await route.fallback()
  })
  await page.goto(`${kata.origin}/kata?view=cron`)
  await expect(page.getByRole('button', { name: 'Inspect workspace', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Cron 37 (partial)' })).toBeVisible()
  await expect(page.getByText(/Project not readable\. Other readable projects/)).toBeVisible()
})

test('daemon switching fences old cron rows and selected detail', async ({ page, kata }) => {
  await kata.launch(page)
  await cronReads(page, kata)
  // The modal drawer's overlay would dismiss it before a pointer reached the
  // switcher, so dispatch the clicks directly: the fence must not rely on that.
  const switchTo = async (from: string, to: string) => {
    await page.getByRole('button', { name: `Switch Kata daemon: ${from}` }).dispatchEvent('click')
    await page.getByRole('menuitemradio', { name: new RegExp(to) }).dispatchEvent('click')
    await expect(page.getByRole('button', { name: `Switch Kata daemon: ${to}` })).toBeVisible()
  }
  // Leave the remote daemon on its Cron page so the final switch lands there.
  await page.goto(`${kata.origin}/kata?view=all-open`)
  await switchTo('example-local', 'example-remote')
  await page.getByRole('button', { name: /^Cron/ }).click()
  await expect(page.getByText('No cron records in these projects.')).toBeVisible()
  await switchTo('example-remote', 'example-local')
  await page.getByRole('button', { name: /^Cron/ }).click()
  await page.getByRole('radio', { name: 'Runs', exact: true }).click()
  await page
    .getByRole('row', { name: /^failed/ })
    .getByRole('button', { name: 'Inspect workspace', exact: true })
    .click()
  const detail = page.getByRole('dialog', { name: 'Run detail' })
  await expect(detail.getByText('Runner could not complete the inspection')).toBeVisible()
  await switchTo('example-local', 'example-remote')
  await expect(page).toHaveURL(/view=cron/)
  await expect(page.getByText('No cron records in these projects.')).toBeVisible()
  await expect(page.getByRole('row', { name: /^failed/ })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Inspect workspace', exact: true })).toHaveCount(0)
  await expect(
    page.getByText('Runner could not complete the inspection', { exact: true }),
  ).toHaveCount(0)
  await expect(detail).toHaveCount(0)
})

test('read-only browser authority can read cron', async ({ page, kata }) => {
  await kata.restartReadonly()
  try {
    await cronReads(page, kata)
    await page.goto(`${kata.origin}/kata?view=cron`)
    await expect(page.getByRole('status', { name: 'Active connection' })).toContainText(
      'Credential: Read-only',
    )
    await expect(page.getByRole('button', { name: 'Inspect workspace', exact: true })).toBeVisible()
  } finally {
    await kata.restart()
  }
})

test('authentication rejection clears protected cron state', async ({ page, kata }) => {
  await kata.launch(page)
  await cronReads(page, kata)
  await page.goto(
    `${kata.origin}/kata?view=cron&cron_tab=runs&cron_kind=run&cron_scope=${kata.projectUID}&cron_uid=${runUID}`,
  )
  await expect(
    page.getByText('Runner could not complete the inspection', { exact: true }),
  ).toBeVisible()
  await page.keyboard.press('Escape')
  let rejected = false
  await page.route('**/api/v1/projects/*/cron/**', async (route) => {
    const status = rejected ? 403 : 401
    rejected = true
    await route.fulfill({ status, json: { error: 'not readable' } })
  })
  await page.getByRole('button', { name: 'Refresh', exact: true }).click()
  await expect.poll(() => rejected).toBe(true)
  await expect(page.getByRole('button', { name: 'Inspect workspace', exact: true })).toHaveCount(0)
  await expect(
    page.getByText('Runner could not complete the inspection', { exact: true }),
  ).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Cron 37' })).toHaveCount(0)
})
