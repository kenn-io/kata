import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('navigates the server menu by keyboard and closes it with Escape', async ({ page, kata }) => {
  await kata.launch(page)
  const trigger = page.getByRole('button', { name: 'Switch Kata daemon: example-local' })
  await trigger.focus()
  await page.keyboard.press('Enter')
  const local = page.getByRole('menuitemradio', { name: /example-local/ })
  const remote = page.getByRole('menuitemradio', { name: /example-remote/ })
  await expect(local).toBeFocused()
  await page.keyboard.press('ArrowDown')
  await expect(remote).toBeFocused()
  await page.keyboard.press('Escape')
  await expect(remote).toBeHidden()
  await expect(trigger).toBeFocused()
})

test('keeps daemon names readable alongside health and recovery hints', async ({ page, kata }) => {
  await kata.launch(page)
  await page.route('**/api/v1/ui/daemons', (route) =>
    route.fulfill({
      json: {
        daemons: [
          { id: 'example-local', url: '', default: true, auth: 'none', health: 'connected' },
          {
            id: 'example-down-server',
            url: 'https://daemon.example',
            local: false,
            default: false,
            auth: 'none',
            health: 'down',
            hint: 'daemon is not reachable',
          },
          {
            id: 'example-older-server',
            url: 'https://daemon.example',
            local: false,
            default: false,
            auth: 'none',
            health: 'upgrade_required',
            hint: 'daemon does not support the Kata web UI',
          },
        ],
      },
    }),
  )
  await page.getByRole('button', { name: 'Switch Kata daemon: example-local' }).click()

  for (const id of ['example-local', 'example-down-server', 'example-older-server']) {
    const row = page.getByTestId(`daemon-row-${id}`)
    await expect(row).toBeVisible()
    const name = row.getByText(id, { exact: true })
    expect((await name.boundingBox())!.width).toBeGreaterThan(60)
  }
  const row = (await page.getByTestId('daemon-row-example-down-server').boundingBox())!
  const hint = (await page.getByText('daemon is not reachable').boundingBox())!
  await expect(page.getByText('unreachable daemon is not reachable', { exact: true })).toBeVisible()
  expect(hint.y).toBeGreaterThanOrEqual(row.y + row.height)
})

test('switches between configured daemons without leaving the Kata workspace', async ({
  page,
  kata,
}) => {
  const credentials = await kata.launch(page)
  await kata.seedIssue(page, credentials, { title: 'Local daemon task' })
  await page.reload()
  await expect(page.getByRole('button', { name: /Local daemon task/ })).toBeVisible()

  await page.getByRole('button', { name: 'Switch Kata daemon: example-local' }).click()
  await page.getByRole('menuitemradio', { name: /example-remote/ }).click()

  await expect(page.getByRole('button', { name: /Remote daemon task/ })).toBeVisible()
  await expect(page.getByRole('button', { name: /Local daemon task/ })).toHaveCount(0)
  expect(new URL(page.url()).origin).toBe(kata.origin)
})
