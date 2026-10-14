import type { Page } from '@playwright/test'
import { expect, test, type BrowserCredentials } from './fixtures'

// Exercise owner settings against the production daemon and its native store.
test('owner credential settings manage teams and persist project visibility', async ({
  page,
  kata,
}, testInfo) => {
  const local = await kata.launch(page)
  const forbidden = await kata.request(page, local, 'POST', '/api/v1/teams', {
    name: 'Forbidden team',
  })
  expect(forbidden.status()).toBe(403)

  const credentials = await loginOwner(page, kata.origin)
  await expect(page.getByRole('heading', { name: 'Teams and visibility' })).toBeVisible()
  await page.getByLabel('New team name').fill('Example team')
  await page.getByRole('button', { name: 'Create team' }).click()
  const option = page.getByRole('option', { name: 'Example team' })
  await expect(option).toHaveCount(1)
  const teamUID = await option.getAttribute('value')
  expect(teamUID).toBeTruthy()
  await page.getByLabel('Team', { exact: true }).selectOption(teamUID!)
  await page.getByLabel('Member account').fill('example-member')
  await page.getByRole('button', { name: 'Add member' }).click()
  await expect(page.getByRole('list', { name: 'Team members' })).toContainText('example-member')
  await page.getByLabel('Project visibility').selectOption(String(kata.projectID))
  await expect(page.getByLabel('Visibility', { exact: true })).toBeEnabled()
  await page.getByLabel('Visibility', { exact: true }).selectOption('teams')
  await page.getByRole('checkbox', { name: 'Example team' }).check()
  await page.getByRole('button', { name: 'Save visibility' }).click()
  await expect(page.getByRole('button', { name: 'Save visibility' })).toBeEnabled()
  const access = await kata.request(
    page,
    credentials,
    'GET',
    `/api/v1/projects/${kata.projectID}/access`,
  )
  expect(access.ok()).toBe(true)
  expect(await access.json()).toMatchObject({
    policy: { visibility: 'teams', team_uids: [teamUID] },
  })

  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 1000 })
    await expect(page.getByRole('heading', { name: 'Teams and visibility' })).toBeVisible()
    const layout = await page.evaluate(() => ({
      width: window.innerWidth,
      content: document.documentElement.scrollWidth,
    }))
    expect(layout.content).toBeLessThanOrEqual(layout.width)
    await page.screenshot({
      path: testInfo.outputPath(`project-access-${width}.png`),
      fullPage: true,
    })
  }
  await page.getByRole('button', { name: 'Remove example-member' }).click()
  await expect(page.getByRole('list', { name: 'Team members' })).not.toContainText('example-member')
  page.once('dialog', (dialog) => void dialog.accept())
  await page.getByRole('button', { name: 'Delete team' }).click()
  await expect(page.getByRole('option', { name: 'Example team' })).toHaveCount(0)
})

test('owner settings share the credential panel width and fit narrow screens', async ({
  page,
  kata,
}, testInfo) => {
  await kata.launch(page)
  await loginOwner(page, kata.origin)
  const management = page.getByRole('region', { name: 'Teams and visibility' })
  await expect(management).toBeVisible()
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 1000 })
    const accessBox = await management.boundingBox()
    const credentialBox = await page
      .getByRole('region', { name: 'Credentials', exact: true })
      .boundingBox()
    expect(accessBox).not.toBeNull()
    expect(credentialBox).not.toBeNull()
    expect(accessBox!.x).toBeCloseTo(credentialBox!.x, 0)
    expect(accessBox!.width).toBeGreaterThanOrEqual(credentialBox!.width - 2)
    const content = await page.evaluate(() => document.documentElement.scrollWidth)
    expect(content).toBeLessThanOrEqual(width)
    await page.screenshot({
      path: testInfo.outputPath(`project-access-layout-${width}.png`),
      fullPage: true,
    })
  }
})

test('identity bootstrap manages access without ordinary write authority', async ({
  page,
  kata,
}, testInfo) => {
  await kata.launch(page)
  await kata.restartIdentity()
  try {
    const credentials = await loginOwner(page, kata.origin)
    await expect(page.getByRole('heading', { name: 'Teams and visibility' })).toBeVisible()
    await expect(page.getByRole('button', { name: 'New task' })).toHaveCount(0)
    const snapshot = await kata.snapshot(page, credentials)
    expect(snapshot.capabilities).toMatchObject({ writable: false, access_admin: true })
    await page.getByLabel('New team name').fill('Identity team')
    await page.getByRole('button', { name: 'Create team' }).click()
    const option = page.getByRole('option', { name: 'Identity team' })
    await expect(option).toHaveCount(1)
    const teamUID = await option.getAttribute('value')
    await page.getByLabel('Team', { exact: true }).selectOption(teamUID!)
    await page.getByLabel('Member account').fill('example-member')
    await page.getByRole('button', { name: 'Add member' }).click()
    await expect(page.getByRole('list', { name: 'Team members' })).toContainText('example-member')
    await page.getByLabel('Project visibility').selectOption(String(kata.projectID))
    await expect(page.getByLabel('Visibility', { exact: true })).toBeEnabled()
    await page.getByLabel('Visibility', { exact: true }).selectOption('teams')
    await page.getByRole('checkbox', { name: 'Identity team' }).check()
    await page.getByRole('button', { name: 'Save visibility' }).click()
    await expect(page.getByRole('button', { name: 'Save visibility' })).toBeEnabled()
    const denied = await kata.request(
      page,
      credentials,
      'POST',
      `/api/v1/projects/${kata.projectID}/issues`,
      { title: 'Forbidden issue', actor: 'forged' },
    )
    expect(denied.status()).toBe(403)
    for (const width of [1280, 390]) {
      await page.setViewportSize({ width, height: 1000 })
      await expect(page.getByRole('heading', { name: 'Teams and visibility' })).toBeVisible()
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(
        width,
      )
      await page.screenshot({
        path: testInfo.outputPath(`identity-access-${width}.png`),
        fullPage: true,
      })
    }
    await page.getByRole('button', { name: 'Remove example-member' }).click()
    await expect(page.getByRole('list', { name: 'Team members' })).not.toContainText(
      'example-member',
    )
    page.once('dialog', (dialog) => void dialog.accept())
    await page.getByRole('button', { name: 'Delete team' }).click()
    await expect(page.getByRole('option', { name: 'Identity team' })).toHaveCount(0)
  } finally {
    await kata.restartIdentity(false)
  }
})

async function loginOwner(page: Page, origin: string): Promise<BrowserCredentials> {
  const login = await page.request.post(`${origin}/api/v1/ui/session/login`, {
    headers: { Origin: origin },
    data: { token: 'example-local-token', return_path: '/kata?view=credentials' },
  })
  expect(login.ok()).toBe(true)
  const credentials = (await login.json()) as BrowserCredentials
  await page.evaluate((value) => {
    sessionStorage.setItem(
      'kata.web.session.v1',
      JSON.stringify({ session: value.session, csrf: value.csrf }),
    )
  }, credentials)
  await page.goto(`${origin}/kata?view=credentials`)
  return credentials
}
