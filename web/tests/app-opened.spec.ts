import type { Request } from '@playwright/test'

import { expect, test } from './fixtures'

test.use({ trace: 'off' })

const telemetryPath = '/api/v1/ui/telemetry'

test('the web UI reports app_opened once per UTC day to the serving daemon', async ({
  page,
  kata,
}) => {
  const requests: Request[] = []
  page.on('request', (request) => {
    if (request.method() === 'POST' && new URL(request.url()).pathname === telemetryPath) {
      requests.push(request)
    }
  })
  const isTelemetry = (response: { url(): string; request(): Request }) =>
    new URL(response.url()).pathname === telemetryPath && response.request().method() === 'POST'
  await page.clock.install({ time: Date.now() })

  const firstResponse = page.waitForResponse(isTelemetry)
  await kata.launch(page)
  const first = await firstResponse
  expect(first.status()).toBe(202)
  expect(await first.json()).toEqual({ status: 'disabled' })
  expect(first.request().postDataJSON()).toEqual({
    event: 'app_opened',
    properties: { surface: 'web' },
  })
  expect(new URL(first.url()).origin).toBe(kata.origin)

  await page.evaluate(() => window.dispatchEvent(new Event('focus')))

  const now = new Date(await page.evaluate(() => Date.now()))
  const nextMidnight = Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate() + 1)
  await page.clock.setSystemTime(nextMidnight + 60_000)
  const secondResponse = page.waitForResponse(isTelemetry)
  await page.evaluate(() => window.dispatchEvent(new Event('focus')))
  const second = await secondResponse
  expect(second.status()).toBe(202)
  expect(second.request().postDataJSON()).toEqual({
    event: 'app_opened',
    properties: { surface: 'web' },
  })
  expect(requests).toHaveLength(2)
})
