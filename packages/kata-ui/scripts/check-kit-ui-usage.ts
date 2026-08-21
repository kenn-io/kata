import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'

import { parse } from 'svelte/compiler'

interface Finding {
  line: number
  message: string
}

export function findRawButtons(source: string): Finding[] {
  const findings: Finding[] = []
  const seen = new Set<object>()

  function visit(value: unknown): void {
    if (Array.isArray(value)) {
      for (const item of value) visit(item)
      return
    }
    if (typeof value !== 'object' || value === null || seen.has(value)) return
    seen.add(value)

    const node = value as Record<string, unknown>
    if (node.type === 'RegularElement' && node.name === 'button') {
      const start = typeof node.start === 'number' ? node.start : 0
      findings.push({
        line: source.slice(0, start).split('\n').length,
        message: 'raw <button> element; use Button from @kenn-io/kit-ui',
      })
    }

    for (const child of Object.values(node)) visit(child)
  }

  visit(parse(source, { modern: true }))
  return findings
}

function* svelteFiles(path: string): Generator<string> {
  const stats = statSync(path)
  if (stats.isFile()) {
    if (path.endsWith('.svelte')) yield path
    return
  }

  for (const entry of readdirSync(path, { withFileTypes: true })) {
    if (entry.name === 'node_modules' || entry.name === 'dist') continue
    const child = join(path, entry.name)
    if (entry.isDirectory()) yield* svelteFiles(child)
    else if (entry.name.endsWith('.svelte')) yield child
  }
}

if (import.meta.main) {
  const roots = process.argv.slice(2)
  if (roots.length === 0) roots.push('src')

  let findingCount = 0
  let fileCount = 0
  for (const root of roots) {
    for (const file of svelteFiles(root)) {
      fileCount += 1
      const findings = findRawButtons(readFileSync(file, 'utf8'))
      findingCount += findings.length
      for (const finding of findings) {
        console.error(
          `${relative(process.cwd(), file)}:${finding.line} [raw-button] ${finding.message}`,
        )
      }
    }
  }

  console.log(`kata-ui usage check: ${findingCount} finding(s) in ${fileCount} file(s)`)
  if (findingCount > 0) process.exitCode = 1
}
