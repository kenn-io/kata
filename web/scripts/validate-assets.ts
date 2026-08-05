import { lstat, readdir, readFile } from 'node:fs/promises'
import { isAbsolute, posix, relative, resolve, sep } from 'node:path'
import { pathToFileURL } from 'node:url'

import { init, parse as parseModules } from 'es-module-lexer'
import { parse as parseHTML, type DefaultTreeAdapterMap } from 'parse5'
import postcss from 'postcss'
import parseCSSValue from 'postcss-value-parser'

const manifestName = '.vite/manifest.json'
const hashedAssetName = /-[A-Za-z0-9_-]{8,}\.[A-Za-z0-9]+$/
const credentialName =
  /(^|[-_.])(credential|credentials|secret|secrets|token|tokens|password|passwd|id_rsa|id_ed25519)([-_.]|$)/i

interface ManifestEntry {
  file?: string
  css?: string[]
  assets?: string[]
  imports?: string[]
  dynamicImports?: string[]
  isEntry?: boolean
}

export interface ValidatedAssetGraph {
  files: string[]
  reachable: string[]
}

export async function validateAssetGraph(root: string): Promise<ValidatedAssetGraph> {
  const absoluteRoot = resolve(root)
  const files = await collectFiles(absoluteRoot)
  requireFile(files, 'index.html')
  requireFile(files, manifestName)

  for (const name of files) validateDistributionName(name)

  const reachable = new Set<string>(['index.html', manifestName])
  const html = await readFile(resolveAsset(absoluteRoot, 'index.html'), 'utf8')
  for (const reference of htmlReferences(html)) {
    reachable.add(resolveReference('index.html', reference, absoluteRoot, files))
  }

  const manifest = await parseManifest(absoluteRoot)
  collectManifestGraph(manifest, absoluteRoot, files, reachable)

  const pending = [...reachable]
  const parsed = new Set<string>()
  while (pending.length > 0) {
    const name = pending.pop()!
    if (parsed.has(name)) continue
    parsed.add(name)

    const references = await assetReferences(absoluteRoot, name)
    for (const reference of references) {
      const target = resolveReference(name, reference, absoluteRoot, files)
      if (!reachable.has(target)) {
        reachable.add(target)
        pending.push(target)
      }
    }
  }

  const unreferenced = files.filter((name) => !reachable.has(name))
  if (unreferenced.length > 0) {
    throw new Error(`unreferenced release asset: ${unreferenced.join(', ')}`)
  }

  return { files, reachable: [...reachable].sort() }
}

async function collectFiles(root: string): Promise<string[]> {
  const files: string[] = []

  async function visit(directory: string): Promise<void> {
    for (const entry of await readdir(directory, { withFileTypes: true })) {
      const target = resolve(directory, entry.name)
      const name = relative(root, target).split(sep).join('/')
      const info = await lstat(target)
      if (info.isSymbolicLink())
        throw new Error(`release asset must not be a symbolic link: ${name}`)
      if (info.isDirectory()) {
        await visit(target)
      } else if (info.isFile()) {
        files.push(name)
      } else {
        throw new Error(`release asset must be a regular file: ${name}`)
      }
    }
  }

  await visit(root)
  return files.sort()
}

function validateDistributionName(name: string): void {
  if (name.endsWith('.map')) throw new Error(`release source map is forbidden: ${name}`)
  const segments = name.split('/')
  for (const segment of segments) {
    if (segment.startsWith('.') && name !== manifestName) {
      throw new Error(`hidden release asset is forbidden: ${name}`)
    }
    if (credentialName.test(segment)) {
      throw new Error(`credential-like release asset is forbidden: ${name}`)
    }
  }
  if (
    name !== 'index.html' &&
    name !== manifestName &&
    !hashedAssetName.test(posix.basename(name))
  ) {
    throw new Error(`immutable release asset must have a hashed filename: ${name}`)
  }
}

function requireFile(files: string[], name: string): void {
  if (!files.includes(name)) throw new Error(`required release asset is missing: ${name}`)
}

async function parseManifest(root: string): Promise<Record<string, ManifestEntry>> {
  const contents = await readFile(resolveAsset(root, manifestName), 'utf8')
  let parsed: unknown
  try {
    parsed = JSON.parse(contents)
  } catch (error) {
    throw new Error(`invalid Vite manifest: ${String(error)}`)
  }
  if (parsed === null || Array.isArray(parsed) || typeof parsed !== 'object') {
    throw new Error('invalid Vite manifest: expected an object')
  }
  return parsed as Record<string, ManifestEntry>
}

function collectManifestGraph(
  manifest: Record<string, ManifestEntry>,
  root: string,
  files: string[],
  reachable: Set<string>,
): void {
  const entryKeys = Object.entries(manifest)
    .filter(([, entry]) => entry.isEntry)
    .map(([key]) => key)
  if (entryKeys.length === 0) throw new Error('Vite manifest has no entry')

  const visited = new Set<string>()
  const visit = (key: string): void => {
    if (visited.has(key)) return
    const entry = manifest[key]
    if (!entry) throw new Error(`Vite manifest import is missing: ${key}`)
    visited.add(key)

    for (const reference of [entry.file, ...(entry.css ?? []), ...(entry.assets ?? [])]) {
      if (!reference) continue
      reachable.add(resolveReference(manifestName, reference, root, files, true))
    }
    for (const imported of [...(entry.imports ?? []), ...(entry.dynamicImports ?? [])])
      visit(imported)
  }
  for (const key of entryKeys) visit(key)
}

type HTMLNode = DefaultTreeAdapterMap['node']
type HTMLElement = DefaultTreeAdapterMap['element']

function htmlReferences(source: string): string[] {
  const document = parseHTML(source)
  const references: string[] = []
  const visit = (node: HTMLNode): void => {
    if ('tagName' in node) {
      const element = node as HTMLElement
      const attributes = Object.fromEntries(
        element.attrs.map((attribute) => [attribute.name, attribute.value]),
      )
      if (element.tagName === 'script' && attributes.src)
        references.push(requireLocal(attributes.src, 'script'))
      if (element.tagName === 'link' && attributes.href) {
        const relationships = new Set((attributes.rel ?? '').toLowerCase().split(/\s+/))
        if (relationships.has('stylesheet') || relationships.has('modulepreload')) {
          references.push(requireLocal(attributes.href, 'style or module'))
        }
      }
    }
    if ('childNodes' in node) for (const child of node.childNodes) visit(child)
  }
  visit(document)
  return references
}

async function assetReferences(root: string, name: string): Promise<string[]> {
  if (name.endsWith('.js') || name.endsWith('.mjs')) {
    await init
    const source = await readFile(resolveAsset(root, name), 'utf8')
    const [imports] = parseModules(source)
    return imports
      .map((record) => record.n)
      .filter((reference): reference is string => reference !== undefined)
      .map((reference) => requireLocal(reference, 'module'))
  }
  if (name.endsWith('.css')) {
    const source = await readFile(resolveAsset(root, name), 'utf8')
    const tree = postcss.parse(source, { from: name })
    const references: string[] = []
    tree.walkAtRules('import', (rule) => {
      const parsed = parseCSSValue(rule.params)
      const first = parsed.nodes.find((node) => node.type === 'string' || node.type === 'word')
      if (first && 'value' in first) references.push(requireLocal(first.value, 'style'))
    })
    tree.walkDecls((declaration) => {
      parseCSSValue(declaration.value).walk((node) => {
        if (node.type !== 'function' || node.value.toLowerCase() !== 'url') return
        const value = parseCSSValue
          .stringify(node.nodes)
          .trim()
          .replace(/^(['"])(.*)\1$/, '$2')
        if (value && !value.startsWith('data:') && !value.startsWith('#')) {
          references.push(requireLocal(value, 'style or font'))
        }
      })
    })
    return references
  }
  return []
}

function requireLocal(reference: string, kind: string): string {
  const trimmed = reference.trim()
  if (/^(?:[a-z][a-z\d+.-]*:|\/\/)/i.test(trimmed)) {
    throw new Error(`external ${kind} reference is forbidden: ${reference}`)
  }
  return trimmed
}

function resolveReference(
  from: string,
  reference: string,
  root: string,
  files: string[],
  manifestReference = false,
): string {
  const cleanReference = reference.split(/[?#]/, 1)[0] ?? ''
  if (!cleanReference || cleanReference.includes('\\')) {
    throw new Error(`invalid asset reference from ${from}: ${reference}`)
  }
  const base = manifestReference || cleanReference.startsWith('/') ? '' : posix.dirname(from)
  const candidate = posix.normalize(posix.join(base, cleanReference.replace(/^\/+/, '')))
  if (candidate === '..' || candidate.startsWith('../') || isAbsolute(candidate)) {
    throw new Error(`asset reference escapes distribution from ${from}: ${reference}`)
  }
  resolveAsset(root, candidate)
  requireFile(files, candidate)
  return candidate
}

function resolveAsset(root: string, name: string): string {
  const target = resolve(root, name)
  const prefix = root.endsWith(sep) ? root : `${root}${sep}`
  if (target !== root && !target.startsWith(prefix))
    throw new Error(`asset path escapes distribution: ${name}`)
  return target
}

async function main(): Promise<void> {
  const root = process.argv[2] ? resolve(process.argv[2]) : resolve('dist')
  const graph = await validateAssetGraph(root)
  process.stdout.write(`validated ${graph.files.length} web assets\n`)
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  await main()
}
