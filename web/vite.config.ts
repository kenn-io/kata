import { svelte, vitePreprocess } from '@sveltejs/vite-plugin-svelte'
import { defineConfig } from 'vite'

const developmentBackend = process.env.KATA_WEB_DEV_BACKEND
const developmentPort = Number.parseInt(process.env.KATA_WEB_DEV_PORT ?? '5173', 10)

export default defineConfig({
  base: './',
  plugins: [svelte({ preprocess: vitePreprocess({ script: true }) })],
  build: {
    manifest: true,
  },
  optimizeDeps: {
    exclude: ['@kenn-io/kit-ui', '@xyflow/svelte'],
  },
  server: {
    host: '127.0.0.1',
    port: developmentPort,
    strictPort: true,
    ...(developmentBackend
      ? {
          proxy: {
            '/api': {
              target: developmentBackend,
              changeOrigin: false,
            },
          },
        }
      : {}),
  },
})
