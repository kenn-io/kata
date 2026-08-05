import { cp, lstat, mkdir, rename, rm, writeFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

import { validateAssetGraph } from './validate-assets'

const repositoryRoot = resolve(dirname(fileURLToPath(import.meta.url)), '../..')
const embeddedDistribution = resolve(repositoryRoot, 'internal/web/dist')
const compilationStub = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>Kata UI assets are not built</title>
  </head>
  <body>
    <main>
      <h1>Kata UI assets are not built</h1>
      <p>This harmless compilation stub is not a release bundle.</p>
    </main>
  </body>
</html>
`

export async function embedAssets(source: string): Promise<void> {
  const absoluteSource = resolve(source)
  await validateAssetGraph(absoluteSource)
  await replaceDistribution(async (staging) => {
    await cp(absoluteSource, staging, { recursive: true, dereference: false, errorOnExist: true })
    await validateAssetGraph(staging)
  })
}

export async function restoreCompilationStub(): Promise<void> {
  await replaceDistribution(async (staging) => {
    await writeFile(resolve(staging, 'index.html'), compilationStub, { mode: 0o644 })
  })
}

async function replaceDistribution(populate: (staging: string) => Promise<void>): Promise<void> {
  const parent = dirname(embeddedDistribution)
  const staging = resolve(parent, `.dist-staging-${process.pid}`)
  const backup = resolve(parent, `.dist-backup-${process.pid}`)
  await assertSafeTarget(parent)
  await rm(staging, { recursive: true, force: true })
  await rm(backup, { recursive: true, force: true })
  await mkdir(staging, { recursive: false })

  try {
    await populate(staging)
    await rename(embeddedDistribution, backup)
    try {
      await rename(staging, embeddedDistribution)
    } catch (error) {
      await rename(backup, embeddedDistribution)
      throw error
    }
    await rm(backup, { recursive: true, force: true })
  } finally {
    await rm(staging, { recursive: true, force: true })
  }
}

async function assertSafeTarget(parent: string): Promise<void> {
  if (embeddedDistribution !== resolve(repositoryRoot, 'internal/web/dist')) {
    throw new Error('refusing to replace an unexpected embedded asset directory')
  }
  const parentInfo = await lstat(parent)
  if (!parentInfo.isDirectory() || parentInfo.isSymbolicLink()) {
    throw new Error('embedded web parent must be a real directory')
  }
  const targetInfo = await lstat(embeddedDistribution)
  if (!targetInfo.isDirectory() || targetInfo.isSymbolicLink()) {
    throw new Error('embedded web distribution must be a real directory')
  }
}

async function main(): Promise<void> {
  if (process.argv[2] === '--restore-stub') {
    await restoreCompilationStub()
    return
  }
  const source = process.argv[2] ? resolve(process.argv[2]) : resolve(repositoryRoot, 'web/dist')
  await embedAssets(source)
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  await main()
}
