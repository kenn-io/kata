import { defineConfig } from '@playwright/test'

const port = Number.parseInt(process.env.KATA_WEB_DEV_PORT ?? '5173', 10)
const origin = `http://127.0.0.1:${port}`

export default defineConfig({
  testDir: './tests',
  // Playwright's CI default, dot, does not name passing tests. The list
  // reporter does, so a rerun's log can confirm a flaky test.
  reporter: 'list',
  globalSetup: './tests/global-setup.ts',
  timeout: 30_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL: origin,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
  },
  webServer: {
    command: 'bun run scripts/dev.ts',
    url: `${origin}/api/v1/ping`,
    // Startup includes downloading Go modules and building the daemon on a cold cache.
    timeout: 600_000,
    reuseExistingServer: false,
    gracefulShutdown: { signal: 'SIGTERM', timeout: 5_000 },
    stdout: 'pipe',
    stderr: 'pipe',
  },
})
