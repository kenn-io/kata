import { defineConfig } from '@playwright/test'

const port = Number.parseInt(process.env.KATA_WEB_DEV_PORT ?? '5173', 10)
const origin = `http://127.0.0.1:${port}`

export default defineConfig({
  testDir: './tests',
  workers: 1,
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
    timeout: 120_000,
    reuseExistingServer: false,
    gracefulShutdown: { signal: 'SIGTERM', timeout: 5_000 },
    stdout: 'pipe',
    stderr: 'pipe',
  },
})
