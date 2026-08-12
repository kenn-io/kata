import { svelte, vitePreprocess } from '@sveltejs/vite-plugin-svelte'
import { defineConfig } from 'vitest/config'

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
  },
})
