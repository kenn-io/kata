import { randomUUID } from 'node:crypto'

import type { Page } from '@playwright/test'

import { expect, test, type BrowserCredentials, type KataFixture } from './fixtures'

test.use({ trace: 'off', video: 'off', screenshot: 'off' })

// Open task counts for the seeded projects: from empty to more than 400 tasks,
// about 1,900 in all.
const projectSizes = [
  0, 1, 2, 3, 5, 8, 12, 15, 20, 25, 30, 40, 50, 60, 75, 90, 110, 140, 180, 240, 320, 405,
]
const budgetMilliseconds = 100

const daemons = [
  { daemon: 'example-local', roundTripMilliseconds: 0 },
  // The daemon switcher proxies every request to a daemon on another host and
  // polls it for changes instead of streaming them.
  { daemon: 'example-remote', roundTripMilliseconds: 100 },
] as const

for (const { daemon, roundTripMilliseconds } of daemons) {
  test(`switching projects on ${daemon} shows the whole list within the budget`, async ({
    page,
    kata,
  }) => {
    test.setTimeout(300_000)
    const credentials = await kata.launch(page, '/kata?view=inbox')
    const names = await seedProjects(page, kata, credentials, daemon)
    if (daemon !== 'example-local') {
      await page.getByRole('button', { name: 'Switch Kata daemon: example-local' }).click()
      await page.getByRole('menuitemradio', { name: new RegExp(daemon) }).click()
    } else {
      await page.reload()
    }
    await expect(page.locator('.project-select-button', { hasText: names.at(-1)! })).toBeVisible()
    if (roundTripMilliseconds > 0) {
      const session = await page.context().newCDPSession(page)
      await session.send('Network.enable')
      await session.send('Network.emulateNetworkConditions', {
        offline: false,
        latency: roundTripMilliseconds,
        downloadThroughput: -1,
        uploadThroughput: -1,
      })
    }

    const timings: { name: string; round: string; milliseconds: number }[] = []
    for (const round of ['first', 'repeat']) {
      for (const name of names) {
        timings.push({ name, round, milliseconds: await switchTo(page, name) })
      }
    }

    const sorted = timings.map(({ milliseconds }) => milliseconds).sort((a, b) => a - b)
    const p95 = sorted[Math.ceil(sorted.length * 0.95) - 1]!
    const summary = `p50 ${sorted[Math.floor(sorted.length / 2)]!.toFixed(1)} ms, p95 ${p95.toFixed(1)} ms, max ${sorted.at(-1)!.toFixed(1)} ms`
    test.info().annotations.push({ type: 'project switch', description: summary })
    console.log(`${daemon}: ${summary}`)
    expect(
      p95,
      `p95 switch time over ${timings.length} clicks: ${JSON.stringify(timings)}`,
    ).toBeLessThan(budgetMilliseconds)
    for (const timing of timings.filter(({ name }) => name === names.at(-1))) {
      expect(timing.milliseconds, `${timing.round} switch to the 405-task project`).toBeLessThan(
        budgetMilliseconds,
      )
    }
  })
}

// Clicks the project in the sidebar and returns the time until the browser has
// painted a frame in which the list shows that project's tasks, its header
// count equals the sidebar count, and no snapshot load is pending.
async function switchTo(page: Page, name: string): Promise<number> {
  return page.evaluate(
    async ({ name, timeout }) => {
      const button = [...document.querySelectorAll<HTMLElement>('.project-select-button')].find(
        (candidate) => candidate.querySelector('.project-name')?.textContent?.trim() === name,
      )
      if (!button) throw new Error(`no sidebar entry for ${name}`)
      const expected = button.querySelector('.project-count')?.textContent?.trim() ?? ''
      const shown = (): boolean => {
        const header = document.querySelector('h2')
        if (header?.textContent?.trim() !== name) return false
        const count = header.parentElement?.querySelector('.count')?.textContent?.trim()
        if (count !== `${expected} ${expected === '1' ? 'task' : 'tasks'}`) return false
        if (
          [...document.querySelectorAll('.kit-sr-only')].some((node) =>
            node.textContent?.includes('Loading snapshot'),
          )
        ) {
          return false
        }
        const badges = [...document.querySelectorAll('button.row[data-uid] .id-badge')]
        if (expected !== '0' && badges.length === 0) return false
        return badges.every((badge) => badge.getAttribute('title')?.startsWith(`${name}#`))
      }
      return new Promise<number>((resolve, reject) => {
        const start = performance.now()
        button.click()
        const check = (): void => {
          if (shown()) {
            // check runs in an animation frame callback, before that frame
            // paints. A task queued now runs after the paint.
            setTimeout(() => resolve(performance.now() - start), 0)
          } else if (performance.now() - start > timeout) {
            reject(new Error(`${name} did not render within ${timeout} ms`))
          } else {
            requestAnimationFrame(check)
          }
        }
        requestAnimationFrame(check)
      })
    },
    { name, timeout: 10_000 },
  )
}

async function seedProjects(
  page: Page,
  kata: KataFixture,
  credentials: BrowserCredentials,
  daemon: string,
): Promise<string[]> {
  // The browser gateway forwards requests for another configured daemon.
  const prefix = daemon === 'example-local' ? '' : '/api/v1/ui/proxy'
  const headers = daemon === 'example-local' ? {} : { 'X-Kata-Web-Daemon': daemon }
  const names: string[] = []
  for (const [index, size] of projectSizes.entries()) {
    const name = `example-area-${String(index + 1).padStart(2, '0')}`
    names.push(name)
    const created = await kata.request(
      page,
      credentials,
      'POST',
      `${prefix}/api/v1/projects`,
      { actor: 'user-a', name },
      headers,
    )
    expect(created.ok()).toBe(true)
    const { project } = (await created.json()) as { project: { id: number } }
    let parentUID: string | undefined
    const create = async (task: number): Promise<void> => {
      const response = await kata.request(
        page,
        credentials,
        'POST',
        `${prefix}/api/v1/projects/${project.id}/issues`,
        {
          actor: 'user-a',
          force_new: true,
          title: `Example task ${task} in ${name}`,
          body: `Synthetic body for task ${task}.`,
          owner: ['user-a', 'user-b', 'user-c'][task % 3],
          priority: task % 4,
          labels: task % 5 === 0 ? ['planning'] : task % 7 === 0 ? ['delivery', 'review'] : [],
          ...(parentUID && task % 8 === 0
            ? { links: [{ type: 'parent', to_ref: parentUID }] }
            : {}),
        },
        { ...headers, 'Idempotency-Key': randomUUID() },
      )
      if (!response.ok()) throw new Error(`issue seed failed with status ${response.status()}`)
      if (task === 0) parentUID = ((await response.json()) as { issue: { uid: string } }).issue.uid
    }
    if (size > 0) await create(0)
    for (let start = 1; start < size; start += 32) {
      await Promise.all(
        Array.from({ length: Math.min(32, size - start) }, (_, offset) => create(start + offset)),
      )
    }
  }
  return names
}
