import type { Page } from '@playwright/test'
import { expect, test, type BrowserCredentials, type KataFixture } from './fixtures'

// Exercise owner settings against the production daemon and its native store.
test('owner settings manage teams and persist project visibility', async ({
  page,
  kata,
}, testInfo) => {
  const local = await kata.launch(page)
  const forbidden = await kata.request(page, local, 'POST', '/api/v1/teams', {
    name: 'Forbidden team',
  })
  expect(forbidden.status()).toBe(403)

  const credentials = await kata.loginOwner(page, '/kata?view=settings&section=access')
  await expect(page.getByRole('heading', { name: 'Teams', exact: true })).toBeVisible()
  await createTeam(page, 'Example team')
  await openTeam(page, 'Example team')
  await addMember(page, 'example-member')
  await chooseProject(page, 'example-project')
  await chooseVisibility(page, 'Selected teams')
  await page.getByRole('checkbox', { name: 'Example team' }).check()
  const teams = await kata.request(page, credentials, 'GET', '/api/v1/teams')
  const teamUID = (
    (await teams.json()) as { teams: Array<{ uid: string; name: string }> }
  ).teams.find((team) => team.name === 'Example team')?.uid
  expect(teamUID).toBeTruthy()
  // Visibility saves on change; poll until the daemon holds the pick.
  await expect
    .poll(() => accessPolicy(page, credentials, kata))
    .toMatchObject({ visibility: 'teams', team_uids: [teamUID] })

  await expect(page.getByRole('checkbox', { name: 'Example team' })).toBeEnabled()
  await page.getByRole('checkbox', { name: 'Example team' }).uncheck()
  await expect
    .poll(() => accessPolicy(page, credentials, kata))
    .toMatchObject({ visibility: 'teams', team_uids: [] })
  await page.reload()
  await chooseProject(page, 'example-project')
  await openTeam(page, 'Example team')
  await expect(page.getByRole('checkbox', { name: 'Example team' })).not.toBeChecked()

  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 1000 })
    await expect(page.getByRole('heading', { name: 'Project visibility' })).toBeVisible()
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
  await expect(teamMembers(page, 'Example team')).not.toContainText('example-member')
  page.once('dialog', (dialog) => void dialog.accept())
  await page.getByRole('button', { name: 'Delete Example team' }).click()
  await expect(teamRow(page, 'Example team')).toHaveCount(0)
})

test('Teams and Project visibility share one content column and fit narrow screens', async ({
  page,
  kata,
}, testInfo) => {
  await kata.launch(page)
  await kata.loginOwner(page, '/kata?view=settings&section=access')
  const teams = page
    .locator('.kit-settings-section')
    .filter({ has: page.getByRole('heading', { name: 'Teams', exact: true }) })
  const visibility = page
    .locator('.kit-settings-section')
    .filter({ has: page.getByRole('heading', { name: 'Project visibility' }) })
  await expect(teams).toBeVisible()
  await expect(visibility).toBeVisible()
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 1000 })
    const teamsBox = await teams.boundingBox()
    const visibilityBox = await visibility.boundingBox()
    expect(teamsBox).not.toBeNull()
    expect(visibilityBox).not.toBeNull()
    expect(visibilityBox!.x).toBeCloseTo(teamsBox!.x, 0)
    expect(visibilityBox!.width).toBeCloseTo(teamsBox!.width, 0)
    expect(visibilityBox!.y).toBeGreaterThan(teamsBox!.y + teamsBox!.height - 1)
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
    const credentials = await kata.loginOwner(page, '/kata?view=settings&section=access')
    await expect(page.getByRole('heading', { name: 'Teams', exact: true })).toBeVisible()
    const snapshot = await kata.snapshot(page, credentials)
    expect(snapshot.capabilities).toMatchObject({ writable: false, access_admin: true })
    await createTeam(page, 'Identity team')
    await openTeam(page, 'Identity team')
    await addMember(page, 'example-member')
    await chooseProject(page, 'example-project')
    await chooseVisibility(page, 'Selected teams')
    await page.getByRole('checkbox', { name: 'Identity team' }).check()
    await expect
      .poll(() => accessPolicy(page, credentials, kata))
      .toMatchObject({ visibility: 'teams' })
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
      await expect(page.getByRole('heading', { name: 'Project visibility' })).toBeVisible()
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(
        width,
      )
      await page.screenshot({
        path: testInfo.outputPath(`identity-access-${width}.png`),
        fullPage: true,
      })
    }
    await page.getByRole('button', { name: 'Remove example-member' }).click()
    await expect(teamMembers(page, 'Identity team')).not.toContainText('example-member')
    page.once('dialog', (dialog) => void dialog.accept())
    await page.getByRole('button', { name: 'Delete Identity team' }).click()
    await expect(teamRow(page, 'Identity team')).toHaveCount(0)
  } finally {
    await kata.restartIdentity(false)
  }
})

async function accessPolicy(page: Page, credentials: BrowserCredentials, kata: KataFixture) {
  const response = await kata.request(
    page,
    credentials,
    'GET',
    `/api/v1/projects/${kata.projectID}/access`,
  )
  return ((await response.json()) as { policy: unknown }).policy
}

function teamRow(page: Page, name: string) {
  return page.locator('.team-name', { hasText: name })
}

function teamMembers(page: Page, name: string) {
  return page
    .locator('details')
    .filter({ has: teamRow(page, name) })
    .getByRole('list', { name: 'Team members' })
}

async function createTeam(page: Page, name: string): Promise<void> {
  await page.getByRole('button', { name: 'New team' }).click()
  const field = page.getByLabel('New team name')
  await field.fill(name)
  await page.getByRole('button', { name: 'Create team' }).click()
  await expect(teamRow(page, name)).toHaveCount(1)
}

async function openTeam(page: Page, name: string): Promise<void> {
  await teamRow(page, name).click()
  await expect(page.getByRole('button', { name: `Delete ${name}` })).toBeVisible()
}

async function addMember(page: Page, account: string): Promise<void> {
  await page.getByRole('button', { name: 'Add member' }).click()
  const field = page.getByLabel('Member account')
  await field.fill(account)
  await page.getByRole('button', { name: 'Add member', exact: true }).click()
  await expect(page.getByRole('list', { name: 'Team members' }).first()).toContainText(account)
}

async function chooseProject(page: Page, name: string): Promise<void> {
  await page.getByRole('combobox', { name: /^Project:/ }).click()
  await page.getByRole('option', { name, exact: true }).click()
  await expect(page.getByRole('combobox', { name: `Project: ${name}` })).toBeVisible()
  await expect(page.getByRole('combobox', { name: /^Visibility:/ })).toBeEnabled()
}

async function chooseVisibility(page: Page, label: 'Everyone' | 'Selected teams'): Promise<void> {
  await page.getByRole('combobox', { name: /^Visibility:/ }).click()
  await page.getByRole('option', { name: label }).click()
}
