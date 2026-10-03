import { spawnSync } from 'node:child_process'
import { mkdir, mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

export interface DevRuntimePaths {
  home: string
  workspace: string
  database: string
}

export function developmentKataBuildArguments(binary: string): string[] {
  return [
    'build',
    '-tags',
    'kit_posthog_disabled',
    '-trimpath',
    '-buildvcs=false',
    '-o',
    binary,
    './cmd/kata',
  ]
}

export interface BrowserTestRuntime {
  binary: string
  teardown(): Promise<void>
}

export interface BrowserTestRuntimeOptions {
  repositoryRoot: string
  inheritedEnvironment: NodeJS.ProcessEnv
  runCommand?: BrowserTestCommandRunner
}

export type BrowserTestCommandRunner = (cmd: string[], cwd: string, env: NodeJS.ProcessEnv) => void

export async function prepareBrowserTestRuntime(
  options: BrowserTestRuntimeOptions,
): Promise<BrowserTestRuntime> {
  const root = await mkdtemp(join(tmpdir(), 'kata-web-browser-e2e-build-'))
  const home = join(root, 'home')
  const workspace = join(root, 'workspace')
  const binaryDirectory = join(root, 'bin')
  const binary = join(binaryDirectory, process.platform === 'win32' ? 'kata.exe' : 'kata')
  const runCommand = options.runCommand ?? runChecked
  let restoreRequired = false
  try {
    await Promise.all([
      mkdir(home, { recursive: true }),
      mkdir(workspace, { recursive: true }),
      mkdir(binaryDirectory, { recursive: true }),
    ])
    const environment = createDevChildEnvironment(options.inheritedEnvironment, {
      home,
      workspace,
      database: join(home, 'kata.db'),
    })

    runCommand(['make', 'web-build', 'web-assets-check'], options.repositoryRoot, environment)
    restoreRequired = true
    runCommand(
      ['bun', 'run', 'scripts/embed-assets.ts', 'dist'],
      join(options.repositoryRoot, 'web'),
      environment,
    )
    runCommand(
      ['go', ...developmentKataBuildArguments(binary)],
      options.repositoryRoot,
      environment,
    )

    let teardown: Promise<void> | undefined
    return {
      binary,
      teardown() {
        teardown ??= (async () => {
          try {
            runCommand(restoreWebStubCommand(), join(options.repositoryRoot, 'web'), environment)
          } finally {
            await rm(root, { recursive: true, force: true })
          }
        })()
        return teardown
      },
    }
  } catch (error) {
    let cleanupError: unknown
    try {
      if (restoreRequired) {
        const environment = createDevChildEnvironment(options.inheritedEnvironment, {
          home,
          workspace,
          database: join(home, 'kata.db'),
        })
        runCommand(restoreWebStubCommand(), join(options.repositoryRoot, 'web'), environment)
      }
    } catch (restoreError) {
      cleanupError = restoreError
    }
    try {
      await rm(root, { recursive: true, force: true })
    } catch (removeError) {
      cleanupError ??= removeError
    }
    if (cleanupError)
      throw new AggregateError([error, cleanupError], 'browser test setup cleanup failed')
    throw error
  }
}

function restoreWebStubCommand(): string[] {
  return ['bun', 'run', 'scripts/embed-assets.ts', '--restore-stub']
}

function runChecked(cmd: string[], cwd: string, env: NodeJS.ProcessEnv): void {
  const [command, ...args] = cmd
  if (!command) throw new Error('browser test command cannot be empty')
  const result = spawnSync(command, args, { cwd, env, encoding: 'utf8' })
  if (result.status !== 0) {
    throw new Error(`${command} failed with status ${result.status}: ${result.stderr}`)
  }
}

const inheritedProcessEnvironment = [
  'PATH',
  'Path',
  'HOME',
  'USERPROFILE',
  'HOMEDRIVE',
  'HOMEPATH',
  'SystemRoot',
  'SYSTEMROOT',
  'WINDIR',
  'ComSpec',
  'COMSPEC',
  'PATHEXT',
  'TMPDIR',
  'TMP',
  'TEMP',
  'LANG',
  'LC_ALL',
  'LC_CTYPE',
  'TZ',
  'BUN_INSTALL',
  'GOROOT',
  'GOPATH',
  'GOMODCACHE',
  'GOCACHE',
  'GOTOOLCHAIN',
  'XDG_CACHE_HOME',
  'SSL_CERT_FILE',
  'SSL_CERT_DIR',
  'NODE_EXTRA_CA_CERTS',
] as const

export function createDevChildEnvironment(
  inherited: NodeJS.ProcessEnv,
  paths: DevRuntimePaths,
): NodeJS.ProcessEnv {
  const environment: NodeJS.ProcessEnv = {}
  for (const name of inheritedProcessEnvironment) {
    const value = inherited[name]
    if (value !== undefined) environment[name] = value
  }
  const goFlags =
    inherited.GOFLAGS === undefined ? undefined : withoutParallelismLimit(inherited.GOFLAGS)
  if (goFlags) environment.GOFLAGS = goFlags
  return {
    ...environment,
    KATA_HOME: paths.home,
    KATA_DB: paths.database,
    KATA_WORKSPACE: paths.workspace,
    KATA_AUTHOR: 'user-a',
  }
}

function withoutParallelismLimit(goFlags: string): string {
  const tokens = splitGoFlags(goFlags)
  const filtered: string[] = []
  for (let index = 0; index < tokens.length; index++) {
    if (tokens[index]?.value === '-p=2') continue
    if (tokens[index]?.value === '-p' && tokens[index + 1]?.value === '2') {
      index++
      continue
    }
    filtered.push(tokens[index]!.raw)
  }
  return filtered.join(' ')
}

interface GoFlagToken {
  value: string
  raw: string
}

function splitGoFlags(goFlags: string): GoFlagToken[] {
  const tokens: GoFlagToken[] = []
  let index = 0
  while (index < goFlags.length) {
    while (index < goFlags.length && isGoFlagSpace(goFlags[index]!)) index++
    if (index >= goFlags.length) break

    const start = index
    const quote = goFlags[index]
    if (quote === '"' || quote === "'") {
      const valueStart = ++index
      while (index < goFlags.length && goFlags[index] !== quote) index++
      if (index >= goFlags.length) {
        const raw = goFlags.slice(start)
        tokens.push({ value: raw, raw })
        break
      }
      tokens.push({ value: goFlags.slice(valueStart, index), raw: goFlags.slice(start, index + 1) })
      index++
      continue
    }

    while (index < goFlags.length && !isGoFlagSpace(goFlags[index]!)) index++
    const raw = goFlags.slice(start, index)
    tokens.push({ value: raw, raw })
  }
  return tokens
}

function isGoFlagSpace(character: string): boolean {
  return character === ' ' || character === '\t' || character === '\n' || character === '\r'
}

export function devDaemonCommand(
  binary: string,
  backendPort: number,
  platform: NodeJS.Platform = process.platform,
): string[] {
  const command = [binary, 'daemon', 'start', '--foreground']
  if (platform === 'win32') command.push('--listen', `127.0.0.1:${backendPort}`)
  return command
}
