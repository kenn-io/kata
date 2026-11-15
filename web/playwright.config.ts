import { defineConfig } from '@playwright/test'

const port = Number.parseInt(process.env.KATA_WEB_DEV_PORT ?? '5173', 10)
const origin = `http://127.0.0.1:${port}`
const ci = Boolean(process.env.CI)
// CI runs the Chromium and Firefox selections separately; each names its report.
const jsonSuite = process.env.KATA_PLAYWRIGHT_SUITE ?? 'web-e2e'

export default defineConfig({
  testDir: './tests',
  // In CI, retry a failed test twice. The JSON report marks a test that passed
  // on retry as flaky; the list reporter names every test in the log.
  retries: ci ? 2 : 0,
  reporter: ci
    ? [['list'], ['json', { outputFile: `test-results/playwright/${jsonSuite}.json` }]]
    : 'list',
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
