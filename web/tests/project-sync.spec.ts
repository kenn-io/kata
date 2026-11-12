import { spawnSync } from 'node:child_process'

import { expect, test, type BrowserCredentials } from './fixtures'

test('local project readers see native sync status with bounded embedding summaries at narrow widths', async ({
  page,
  kata,
}, testInfo) => {
  await kata.prepareSyncDiagnostic()
  const local = await kata.launch(page, `/kata?scope=${kata.projectUID}`)
  const region = page.getByRole('region', { name: 'Project sync' })
  await expect(region.getByText('Local project')).toBeVisible()
  const statusPath = `/api/v1/projects/${kata.projectID}/federation/status`
  expect((await kata.request(page, local, 'GET', statusPath)).ok()).toBe(true)
  expect((await kata.request(page, local, 'GET', '/api/v1/federation/status')).status()).toBe(403)
  expect(
    (
      await kata.request(
        page,
        local,
        'POST',
        `/api/v1/projects/${kata.projectID}/federation/enable`,
        { actor: 'example-member' },
      )
    ).status(),
  ).toBe(403)
  const login = await page.request.post(`${kata.origin}/api/v1/ui/session/login`, {
    headers: { Origin: kata.origin },
    data: { token: 'example-local-token', return_path: '/kata' },
  })
  expect(login.ok()).toBe(true)
  const owner = (await login.json()) as BrowserCredentials
  const enabled = await kata.request(
    page,
    owner,
    'POST',
    `/api/v1/projects/${kata.projectID}/federation/enable`,
    { actor: 'example-owner' },
  )
  expect(enabled.ok()).toBe(true)
  kata.recordSyncDiagnosticError()
  const statusRead = page.waitForResponse(
    (response) => new URL(response.url()).pathname === statusPath,
  )
  await region.getByRole('button', { name: 'Refresh sync' }).click()
  const browserBody = await (await statusRead).text()
  expect(browserBody).not.toContain('example sync failure')
  expect(browserBody).not.toContain('example held-back batch failure')
  await expect(region.getByText('Sync needs attention')).toBeVisible()
  // This hub/unconfigured state comes from the native production SQLite daemon.
  await expect(region.getByText('hub', { exact: true })).toBeVisible()
  await expect(region.getByText('Embeddings: unconfigured')).toBeVisible()

  // This response fixture covers browser rendering; native artifact reuse,
  // paid-call counts and PG authorization are proved in their backend suites.
  await page.route(`**${statusPath}*`, async (route) => {
    const response = await route.fetch()
    const data = await response.json()
    data.statuses[0].embedding = {
      state: 'reused',
      artifact_limit: 32,
      limited: true,
      producer: {
        producer_instance_uid: '01J00000000000000000000003',
        recipe: { model: 'example-model', dimensions: 2 },
      },
      artifacts: [{ state: 'generated' }, { state: 'reused' }, { state: 'stored_unindexed' }],
    }
    await route.fulfill({ response, json: data })
  })
  await region.getByRole('button', { name: 'Refresh sync' }).click()
  await expect(region.getByText('Reused: 1')).toBeVisible()
  await expect(region.getByText('Run in a CLI connected to this daemon.')).toBeVisible()
  await page
    .context()
    .grantPermissions(['clipboard-read', 'clipboard-write'], { origin: kata.origin })
  for (const [shell, label] of [
    ['bash', 'Bash/Zsh'],
    ['pwsh', 'PowerShell'],
  ] as const) {
    await test.step(`Run the ${label} diagnostic`, async (step) => {
      if (shell === 'pwsh') {
        const probe = spawnSync('pwsh', ['-NoProfile', '-NonInteractive', '-Command', 'exit 0'])
        step.skip(probe.error?.code === 'ENOENT', 'PowerShell is not installed')
        if (probe.error) throw probe.error
        expect(probe.status).toBe(0)
      }
      await region.getByRole('button', { name: `Copy ${label} sync status command` }).click()
      const command = await page.evaluate(() => navigator.clipboard.readText())
      const diagnostic = kata.runSyncDiagnostic(shell, command)
      expect(diagnostic).toContain('example sync failure')
      expect(diagnostic).toContain('example held-back batch failure')
      if (shell === 'pwsh') {
        expect(kata.runSyncDiagnostic(shell, command, true)).toContain('example sync failure')
      }
    })
  }
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 1000 })
    await expect(region.getByText('Retained without index: 1')).toBeVisible()
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(
      width,
    )
    await page.screenshot({
      path: testInfo.outputPath(`project-sync-${width}.png`),
      fullPage: true,
    })
  }
  kata.clearSyncDiagnosticError()
  await region.getByRole('button', { name: 'Refresh sync' }).click()
  await expect(region.getByText('Sync needs attention')).toHaveCount(0)
  await expect(region.getByRole('button', { name: /Copy .* sync status command/ })).toHaveCount(0)
})
