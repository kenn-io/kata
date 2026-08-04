import { plugin } from 'bun'
import { mock } from 'bun:test'
import { JSDOM } from 'jsdom'
import { compile, compileModule } from 'svelte/compiler'
import { fileURLToPath } from 'node:url'

const svelteClient = fileURLToPath(
  new URL('./src/index-client.js', import.meta.resolve('svelte/package.json')),
)

mock.module('svelte', () => import(svelteClient))

plugin({
  name: 'svelte-test-loader',
  setup(build) {
    build.onLoad({ filter: /\.svelte$/ }, async ({ path }) => {
      const source = await Bun.file(path).text()
      const compiled = compile(source, {
        filename: path,
        generate: 'client',
      })

      return {
        contents: compiled.js.code,
        loader: 'js',
      }
    })
    build.onLoad({ filter: /\.svelte\.(js|ts)$/ }, async ({ path }) => {
      const source = await Bun.file(path).text()
      const compiled = compileModule(source, {
        filename: path,
        generate: 'client',
      })

      return {
        contents: compiled.js.code,
        loader: path.endsWith('.ts') ? 'ts' : 'js',
      }
    })
  },
})

const dom = new JSDOM('<!doctype html><html><body></body></html>', {
  url: 'http://127.0.0.1/',
})

for (const property of Object.getOwnPropertyNames(dom.window)) {
  if (!(property in globalThis)) {
    Object.defineProperty(
      globalThis,
      property,
      Object.getOwnPropertyDescriptor(dom.window, property),
    )
  }
}
