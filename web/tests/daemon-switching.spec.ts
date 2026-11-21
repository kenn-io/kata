import { expect, test } from './fixtures'

test.use({ trace: 'off' })

test('keeps daemon names readable alongside health and recovery hints', async ({ page, kata }) => {
  await kata.launch(page)
  await page.route('**/api/v1/ui/daemons', (route) =>
    route.fulfill({
      json: {
        daemons: [
          { id: 'example-local', url: '', default: true, auth: 'none', health: 'connected' },
          {
            id: 'example-down-server',
            url: '',
            default: false,
            auth: 'none',
            health: 'down',
            hint: 'Check that the daemon is running and reachable.',
          },
          {
            id: 'example-older-server',
            url: '',
            default: false,
            auth: 'none',
            health: 'upgrade_required',
            hint: 'Update this daemon to use the web interface.',
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
  await expect(page.getByText('unreachable')).toBeVisible()
  await expect(page.getByRole('menuitemradio', { name: /example-down-server/ })).toContainText(
    'unreachable Check that the daemon is running and reachable.',
  )
  await expect(page.getByText('update required')).toBeVisible()
  await expect(page.getByText('Check that the daemon is running and reachable.')).toBeVisible()
  await expect(page.getByText('Update this daemon to use the web interface.')).toBeVisible()
  const row = (await page.getByTestId('daemon-row-example-down-server').boundingBox())!
  const hint = (await page
    .getByText('Check that the daemon is running and reachable.')
    .boundingBox())!
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
