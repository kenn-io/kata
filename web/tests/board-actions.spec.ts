import AxeBuilder from '@axe-core/playwright'
import type { Page } from '@playwright/test'

import { expect, test, type BrowserCredentials, type KataFixture } from './fixtures'

test.use({ trace: 'off' })

interface IssueState {
  uid: string
  status: string
  owner?: string
  metadata: Record<string, unknown>
}

// What the daemon recorded, read back through a fresh snapshot of every issue.
async function issueState(
  page: Page,
  kata: KataFixture,
  credentials: BrowserCredentials,
  uid: string,
): Promise<IssueState> {
  const snapshot = await kata.snapshot(page, credentials, 'view=all-open&status=all')
  const issue = (snapshot.collection as IssueState[]).find((candidate) => candidate.uid === uid)
  expect(issue).toBeDefined()
  return issue!
}

function column(page: Page, name: string) {
  return page.getByRole('region', { name: new RegExp(`^${name},`) })
}

async function openBoard(page: Page, kata: KataFixture): Promise<void> {
  await page.goto(`${kata.origin}/kata?scope=${kata.projectUID}&layout=board`)
  await expect(column(page, 'Open')).toBeVisible()
}

test('claims an Open card from the keyboard, and the daemon records the owner', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Claim me from the board' })
  await openBoard(page, kata)

  await column(page, 'Open').getByRole('button', { name: issue.title, exact: true }).focus()
  await page.keyboard.press('Alt+ArrowRight')

  await expect(
    column(page, 'In progress').getByRole('button', { name: issue.title, exact: true }),
  ).toBeVisible()
  await expect
    .poll(async () => (await issueState(page, kata, credentials, issue.uid)).owner)
    .toBeTruthy()
})

test('moving a card out of Needs attention records that a person handled it', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, {
    title: 'Pick a release date',
    owner: 'agent-a',
    metadata: { 'work.attention': 'needs-human', 'work.attention_msg': 'Pick one of two dates' },
  })
  await openBoard(page, kata)

  const waiting = column(page, 'Needs attention')
    .getByRole('listitem')
    .filter({ hasText: issue.title })
  await expect(waiting.getByText('Needs you')).toBeVisible()
  await expect(waiting.getByText('Needs a human: Pick one of two dates')).toBeVisible()
  await waiting.getByRole('button', { name: issue.title, exact: true }).focus()
  await page.keyboard.press('Alt+ArrowLeft')

  await expect(
    column(page, 'In progress').getByRole('button', { name: issue.title, exact: true }),
  ).toBeVisible()
  await expect
    .poll(async () => (await issueState(page, kata, credentials, issue.uid)).metadata)
    .toMatchObject({ 'work.attention': 'ok' })
  expect((await issueState(page, kata, credentials, issue.uid)).metadata).not.toHaveProperty(
    'work.attention_msg',
  )
})

test('Ask… sends the owner a kata notify request and leaves the card where it is', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, {
    title: 'Ship the parser',
    owner: 'agent-b',
  })
  await openBoard(page, kata)

  await page.getByRole('button', { name: `Actions for ${issue.title}` }).click()
  await page.getByRole('menuitem', { name: 'Ask agent-b…' }).click()
  const dialog = page.getByRole('dialog', { name: 'Ask agent-b' })
  await dialog.getByRole('textbox', { name: 'What do you need?' }).fill('Is this ready for review?')
  await dialog.getByRole('button', { name: 'Send request' }).click()

  await expect(page.getByText(`Asked agent-b about ${issue.title}.`)).toBeVisible()
  await expect(
    column(page, 'In progress').getByRole('button', { name: issue.title, exact: true }),
  ).toBeVisible()
  // kata notify stores the request under notify.<unpadded base64url recipient>.
  const key = `notify.${Buffer.from('agent-b').toString('base64url')}`
  await expect
    .poll(async () => (await issueState(page, kata, credentials, issue.uid)).metadata[key])
    .toMatchObject({ message: 'Is this ready for review?' })
})

test('Done asks for the usual completion evidence before it closes a card', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Write the board docs' })
  await openBoard(page, kata)

  await page.getByRole('button', { name: `Actions for ${issue.title}` }).click()
  await page.getByRole('menuitem', { name: 'Move to Done' }).click()
  const dialog = page.getByRole('dialog', { name: 'Complete task' })
  await expect(dialog.getByRole('button', { name: 'Complete' })).toBeDisabled()
  await dialog.getByRole('textbox', { name: 'Evidence value' }).fill('make docs-check')
  await dialog
    .getByRole('textbox', { name: /Completion note/ })
    .fill('Documented the board columns and moves; the docs check passes.')
  await dialog.getByRole('button', { name: 'Complete' }).click()

  await expect(
    column(page, 'Done').getByRole('button', { name: issue.title, exact: true }),
  ).toBeVisible()
  await expect
    .poll(async () => (await issueState(page, kata, credentials, issue.uid)).status)
    .toBe('closed')
})

for (const colorScheme of ['light', 'dark'] as const) {
  test(`the board has no accessibility violations in ${colorScheme} mode`, async ({
    page,
    kata,
  }) => {
    await page.emulateMedia({ colorScheme })
    const credentials = await kata.launch(page)
    await kata.seedIssue(page, credentials, { title: 'Unclaimed task', labels: ['bug'] })
    await kata.seedIssue(page, credentials, { title: 'Claimed task', owner: 'agent-a' })
    await kata.seedIssue(page, credentials, {
      title: 'Stuck task',
      metadata: { 'work.attention': 'stuck', 'work.attention_msg': 'Missing a credential' },
    })
    await openBoard(page, kata)
    await expect(column(page, 'Needs attention').getByText('Stuck task')).toBeVisible()

    const results = await new AxeBuilder({ page }).analyze()
    expect(results.violations).toEqual([])
  })
}

test('the card actions menu, and Ask… in it, can be reached from the keyboard', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, { title: 'Keyboard reachable card' })
  await openBoard(page, kata)

  await column(page, 'Open').getByRole('button', { name: issue.title, exact: true }).focus()
  await page.keyboard.press('Tab')
  await expect(page.getByRole('button', { name: `Actions for ${issue.title}` })).toBeFocused()
  await page.keyboard.press('Enter')
  await expect(page.getByRole('menuitem', { name: 'Ask user-a…' })).toBeVisible()
})

test('a move that takes two kata actions finishes both', async ({ page, kata }) => {
  const credentials = await kata.launch(page)
  const issue = await kata.seedIssue(page, credentials, {
    title: 'Nobody has claimed this stuck task',
    metadata: { 'work.attention': 'needs-human' },
  })
  await openBoard(page, kata)

  // Clearing attention and claiming are two writes; the live stream reports
  // the first while the second is about to start.
  await column(page, 'Needs attention')
    .getByRole('button', { name: issue.title, exact: true })
    .focus()
  await page.keyboard.press('Alt+ArrowLeft')

  await expect
    .poll(async () => (await issueState(page, kata, credentials, issue.uid)).owner)
    .toBeTruthy()
  expect((await issueState(page, kata, credentials, issue.uid)).metadata).toMatchObject({
    'work.attention': 'ok',
  })
})
