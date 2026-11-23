import { spawnSync } from 'node:child_process'

import { expect, test, type BrowserCredentials } from './fixtures'

test('project readers see sync diagnostics with bounded embedding summaries at narrow widths', async ({
  page,
  diagnosticKata: kata,
}, testInfo) => {
  await kata.prepareSyncDiagnostic()
  const local = await kata.launch(page, `/kata?scope=${kata.projectUID}`)
  const chip = page.getByRole('button', { name: 'Sync failed' })
  const region = page.getByRole('dialog', { name: 'Project sync' })
  await expect(
    page.getByRole('button', { name: /^(Synced|Sync failed|\d+ to sync)$/ }),
  ).toHaveCount(0)
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
  await page.reload()
  const browserBody = await (await statusRead).text()
  expect(browserBody).not.toContain('example sync failure')
  expect(browserBody).not.toContain('example held-back batch failure')
  await chip.click()
  await expect(region.getByText('Sync failed', { exact: true })).toBeVisible()
  await expect(region.getByText('Unconfigured')).toBeVisible()

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
  await region.getByRole('button', { name: 'Refresh' }).click()
  await expect(region.getByText('1 generated · 1 reused · 1 without index')).toBeVisible()
  await expect(
    region.getByText("To see the error, run this on the server's machine:"),
  ).toBeVisible()
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
      await region.getByRole('button', { name: `Copy ${label} command` }).click()
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
    await expect(region.getByText('1 generated · 1 reused · 1 without index')).toBeVisible()
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(
      width,
    )
    await page.screenshot({
      path: testInfo.outputPath(`project-sync-${width}.png`),
      fullPage: true,
    })
  }
  // The embedding fixture ends here, so the last read comes from the daemon.
  await page.unrouteAll({ behavior: 'wait' })
  kata.clearSyncDiagnosticError()
  const clearedRead = page.waitForResponse(
    (response) => new URL(response.url()).pathname === statusPath,
  )
  await region.getByRole('button', { name: 'Refresh' }).click()
  // The attention block also disappears while the read is pending, so wait for
  // the read and the status it renders.
  expect((await clearedRead).ok()).toBe(true)
  await expect(region.getByText('Sync disabled', { exact: true })).toBeVisible()
  await expect(region.getByRole('button', { name: /Copy .* command/ })).toHaveCount(0)
  await page.keyboard.press('Escape')
  await expect(chip).toHaveCount(0)
})
