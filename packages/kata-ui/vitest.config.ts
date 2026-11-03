import { svelte, vitePreprocess } from '@sveltejs/vite-plugin-svelte'
import { defineConfig } from 'vitest/config'

// GitHub Actions logs name every passing test so a rerun can confirm a flaky
// test. Explicit reporters replace Vitest's default, so keep its annotations.
const ciReporters =
  process.env.GITHUB_ACTIONS === 'true' ? { reporters: ['tree', 'github-actions'] } : {}

export default defineConfig({
  test: { setupFiles: ['./src/test/setup.ts'], ...ciReporters },
  plugins: [svelte({ preprocess: vitePreprocess({ script: true }) })],
  resolve: {
    conditions: ['browser'],
  },
  optimizeDeps: {
    exclude: ['@kenn-io/kit-ui'],
  },
  ssr: {
    noExternal: ['@kenn-io/kit-ui', '@lucide/svelte'],
  },
})
