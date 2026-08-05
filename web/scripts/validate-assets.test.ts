import { afterEach, describe, expect, test } from 'vitest'
import { mkdtemp, mkdir, rm, symlink, writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import { tmpdir } from 'node:os'

import { validateAssetGraph } from './validate-assets'

const temporaryRoots: string[] = []

afterEach(async () => {
  await Promise.all(
    temporaryRoots.splice(0).map((root) => rm(root, { recursive: true, force: true })),
  )
})

async function writeFixture(overrides: Record<string, string | null> = {}): Promise<string> {
  const root = await mkdtemp(join(tmpdir(), 'kata-web-assets-'))
  temporaryRoots.push(root)

  const files: Record<string, string> = {
    'index.html': `<!doctype html><html><head><link rel="stylesheet" href="/assets/app-a1b2c3d4.css"></head><body><div id="app"></div><script type="module" src="/assets/app-a1b2c3d4.js"></script></body></html>`,
    '.vite/manifest.json': JSON.stringify({
      'src/main.ts': {
        file: 'assets/app-a1b2c3d4.js',
        name: 'main',
        src: 'src/main.ts',
        isEntry: true,
        imports: ['_chunk.ts'],
        css: ['assets/app-a1b2c3d4.css'],
        assets: ['assets/icon-facecafe.svg'],
      },
      '_chunk.ts': {
        file: 'assets/chunk-deadbeef.js',
        name: 'chunk',
      },
    }),
    'assets/app-a1b2c3d4.js': `import './chunk-deadbeef.js'; new URL('./icon-facecafe.svg', import.meta.url);`,
    'assets/chunk-deadbeef.js': `export const ready = true;`,
    'assets/app-a1b2c3d4.css': `@font-face { src: url('./font-c001d00d.woff2') } .logo { background-image: url('./icon-facecafe.svg') }`,
    'assets/icon-facecafe.svg': `<svg xmlns="http://www.w3.org/2000/svg"></svg>`,
    'assets/font-c001d00d.woff2': `font fixture`,
    ...Object.fromEntries(Object.entries(overrides).filter(([, value]) => value !== null)),
  }

  for (const [name, contents] of Object.entries(files)) {
    if (overrides[name] === null) continue
    const target = join(root, name)
    await mkdir(join(target, '..'), { recursive: true })
    await writeFile(target, contents)
  }
  return root
}

describe('validateAssetGraph', () => {
  test('accepts a complete self-contained Vite asset graph', async () => {
    const root = await writeFixture()

    const graph = await validateAssetGraph(root)

    expect(graph.files).toEqual([
      '.vite/manifest.json',
      'assets/app-a1b2c3d4.css',
      'assets/app-a1b2c3d4.js',
      'assets/chunk-deadbeef.js',
      'assets/font-c001d00d.woff2',
      'assets/icon-facecafe.svg',
      'index.html',
    ])
  })

  test.each([
    ['missing index', { 'index.html': null }, 'index.html'],
    ['missing manifest', { '.vite/manifest.json': null }, 'manifest'],
    ['missing JavaScript import', { 'assets/chunk-deadbeef.js': null }, 'chunk-deadbeef.js'],
    ['unreferenced file', { 'assets/orphan-acde1234.js': 'export {}' }, 'unreferenced'],
    ['unhashed immutable asset', { 'assets/icon.svg': '<svg></svg>' }, 'hashed'],
    ['release source map', { 'assets/app-a1b2c3d4.js.map': '{}' }, 'source map'],
    ['hidden filename', { 'assets/.environment-a1b2c3d4': 'hidden' }, 'hidden'],
    ['credential filename', { 'assets/credentials-a1b2c3d4.json': '{}' }, 'credential'],
  ])('rejects %s', async (_name, overrides, message) => {
    const root = await writeFixture(overrides)

    await expect(validateAssetGraph(root)).rejects.toThrow(message)
  })

  test('rejects symlinks', async () => {
    const root = await writeFixture()
    await symlink(join(root, 'assets/icon-facecafe.svg'), join(root, 'assets/link-acde1234.svg'))

    await expect(validateAssetGraph(root)).rejects.toThrow('symbolic link')
  })

  test.each([
    ['path escapes', { 'assets/app-a1b2c3d4.js': `import '../../../outside.js'` }, 'escape'],
    [
      'external modules',
      { 'assets/app-a1b2c3d4.js': `import 'https://cdn.example/app.js'` },
      'external',
    ],
    [
      'external scripts',
      {
        'index.html': `<!doctype html><script type="module" src="https://cdn.example/app.js"></script>`,
      },
      'external',
    ],
    [
      'external styles',
      {
        'index.html': `<!doctype html><link rel="stylesheet" href="https://cdn.example/app.css"><script type="module" src="/assets/app-a1b2c3d4.js"></script>`,
      },
      'external',
    ],
    [
      'external fonts',
      {
        'assets/app-a1b2c3d4.css': `@font-face { src: url('https://fonts.example/font.woff2') }`,
      },
      'external',
    ],
  ])('rejects %s', async (_name, overrides, message) => {
    const root = await writeFixture(overrides)

    await expect(validateAssetGraph(root)).rejects.toThrow(message)
  })
})
