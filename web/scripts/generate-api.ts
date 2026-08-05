import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

const schema = fileURLToPath(new URL('../../api/openapi.yaml', import.meta.url))
const output = fileURLToPath(new URL('../src/lib/api/schema.d.ts', import.meta.url))
const prettierConfig = fileURLToPath(new URL('../.prettierrc.json', import.meta.url))
const check = process.argv.includes('--check')
const temporaryDirectory = check ? await mkdtemp(join(tmpdir(), 'kata-web-api-')) : undefined
const destination = temporaryDirectory ? join(temporaryDirectory, 'schema.d.ts') : output

try {
  const generated = Bun.spawnSync([
    process.execPath,
    'x',
    'openapi-typescript',
    schema,
    '--output',
    destination,
  ])
  if (!generated.success) {
    process.stderr.write(generated.stderr)
    process.exit(generated.exitCode)
  }
  const formatted = Bun.spawnSync([
    process.execPath,
    'x',
    'prettier',
    '--config',
    prettierConfig,
    '--write',
    destination,
  ])
  if (!formatted.success) {
    process.stderr.write(formatted.stderr)
    process.exit(formatted.exitCode)
  }
  if (check) {
    const [expected, actual] = await Promise.all([
      Bun.file(output).arrayBuffer(),
      Bun.file(destination).arrayBuffer(),
    ])
    if (!Buffer.from(expected).equals(Buffer.from(actual))) {
      console.error('generated browser API schema is stale; run `bun run generate`')
      process.exit(1)
    }
  }
} finally {
  if (temporaryDirectory) {
    await rm(temporaryDirectory, { recursive: true, force: true })
  }
}
