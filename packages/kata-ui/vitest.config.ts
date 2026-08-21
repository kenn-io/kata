import { svelte, vitePreprocess } from '@sveltejs/vite-plugin-svelte'
import { defineConfig } from 'vitest/config'

export default defineConfig({
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
