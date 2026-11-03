import { svelte, vitePreprocess } from '@sveltejs/vite-plugin-svelte'
import { defineConfig } from 'vitest/config'

// On GitHub Actions, retry a failed test twice and write a JSON report in which
// a flaky test passes with the earlier failure messages. Explicit reporters
// replace Vitest's default, so keep its annotations and a log of every test.
const ciOptions =
  process.env.GITHUB_ACTIONS === 'true'
    ? {
        retry: 2,
        reporters: ['tree', 'github-actions', 'json'],
        outputFile: { json: 'test-results/vitest/web.json' },
      }
    : {}

export default defineConfig({
  plugins: [svelte({ preprocess: vitePreprocess({ script: true }) })],
  resolve: {
    conditions: ['browser'],
  },
  optimizeDeps: {
    exclude: ['@kenn-io/kit-ui', '@xyflow/svelte'],
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.{js,ts}'],
    setupFiles: ['./src/test/setup.ts'],
    ...ciOptions,
  },
})
