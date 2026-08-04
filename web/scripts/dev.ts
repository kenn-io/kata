import { mkdir, mkdtemp, readFile, readdir, rm, unlink, writeFile } from 'node:fs/promises'
import { request } from 'node:http'
import { createServer } from 'node:net'
import { basename, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import { createDevChildEnvironment, devDaemonCommand } from '../src/lib/dev-environment'

const repositoryRoot = fileURLToPath(new URL('../..', import.meta.url))
const stateRoot = join(repositoryRoot, '.kata-web-dev')
const activeRuntimePath = join(stateRoot, 'active.json')
const vitePort = parsePort(process.env.KATA_WEB_DEV_PORT ?? '5173', 'KATA_WEB_DEV_PORT')
const publicOrigin = `http://127.0.0.1:${vitePort}`

await mkdir(stateRoot, { recursive: true })
await cleanupStaleRuns(stateRoot)
const runRoot = await mkdtemp(join(stateRoot, 'run-'))
const home = join(runRoot, 'home')
const workspace = join(runRoot, 'workspace')
const binaryDirectory = join(runRoot, 'bin')
const binary = join(binaryDirectory, process.platform === 'win32' ? 'kata.exe' : 'kata')
await Promise.all([
  mkdir(home, { recursive: true }),
  mkdir(workspace, { recursive: true }),
  mkdir(binaryDirectory, { recursive: true }),
])
await writeFile(join(runRoot, '.kata-web-dev-owned'), `${JSON.stringify({ pid: process.pid })}\n`, {
  mode: 0o600,
})

const backendPort = await availablePort()
const backendOrigin = `http://127.0.0.1:${backendPort}`
await writeFile(
  join(home, 'config.toml'),
  `[web]\nlisten = "127.0.0.1:${backendPort}"\npublic_origin = "${publicOrigin}"\n`,
  { mode: 0o600 },
)

const childEnvironment = createDevChildEnvironment(process.env, {
  home,
  workspace,
  database: join(home, 'kata.db'),
})

await runChecked(['go', 'build', '-o', binary, './cmd/kata'], repositoryRoot, childEnvironment)

const daemon = Bun.spawn({
  cmd: devDaemonCommand(binary, backendPort),
  cwd: workspace,
  env: childEnvironment,
  stdin: 'ignore',
  stdout: 'inherit',
  stderr: 'inherit',
})

let vite: ReturnType<typeof Bun.spawn> | undefined
let shuttingDown = false
let runtimeFile = ''

try {
  runtimeFile = await waitForRuntimeRecord(home, publicOrigin, daemon)
  await waitForPing(backendOrigin, publicOrigin, daemon)
  await runChecked([binary, 'projects', 'create', 'example-project'], workspace, childEnvironment)
  await writeFile(
    join(workspace, '.kata.toml'),
    'version = 1\n\n[project]\nname = "example-project"\n',
  )
  await writeFile(
    activeRuntimePath,
    `${JSON.stringify({ home, workspace, binary, backendOrigin, publicOrigin, runtimeFile })}\n`,
    { mode: 0o600 },
  )

  vite = Bun.spawn({
    cmd: [
      process.execPath,
      'x',
      'vite',
      '--host',
      '127.0.0.1',
      '--port',
      String(vitePort),
      '--strictPort',
    ],
    cwd: join(repositoryRoot, 'web'),
    env: {
      ...childEnvironment,
      KATA_WEB_DEV_BACKEND: backendOrigin,
      KATA_WEB_DEV_ORIGIN: publicOrigin,
      KATA_WEB_DEV_PORT: String(vitePort),
    },
    stdin: 'ignore',
    stdout: 'inherit',
    stderr: 'inherit',
  })

  process.once('SIGINT', () => {
    void shutdown('SIGINT').then(() => process.exit(0))
  })
  process.once('SIGTERM', () => {
    void shutdown('SIGTERM').then(() => process.exit(0))
  })

  const exited = await Promise.race([
    daemon.exited.then((code) => ({ child: 'daemon', code })),
    vite.exited.then((code) => ({ child: 'Vite', code })),
  ])
  if (!shuttingDown) {
    throw new Error(`${exited.child} exited unexpectedly with status ${exited.code}`)
  }
} finally {
  await shutdown('SIGTERM')
}

async function shutdown(signal: NodeJS.Signals): Promise<void> {
  if (shuttingDown) return
  shuttingDown = true
  vite?.kill(signal)
  daemon.kill(signal)
  await Promise.all([waitForExit(vite), waitForExit(daemon)])
  await removeOwnedActiveRuntime()
  await rm(runRoot, { recursive: true, force: true })
}

async function removeOwnedActiveRuntime(): Promise<void> {
  try {
    const active = JSON.parse(await readFile(activeRuntimePath, 'utf8')) as { runtimeFile?: string }
    if (active.runtimeFile === runtimeFile) await unlink(activeRuntimePath)
  } catch {
    // Another run may already have replaced or removed the non-secret marker.
  }
}

async function waitForExit(child: ReturnType<typeof Bun.spawn> | undefined): Promise<void> {
  if (!child) return
  const exited = child.exited.then(() => undefined)
  const timedOut = new Promise<void>((resolve) => setTimeout(resolve, 5_000))
  await Promise.race([exited, timedOut])
  if (child.exitCode === null) {
    child.kill('SIGKILL')
    await child.exited
  }
}

async function runChecked(
  cmd: string[],
  cwd: string,
  env: Record<string, string | undefined>,
): Promise<void> {
  const child = Bun.spawn({ cmd, cwd, env, stdin: 'ignore', stdout: 'inherit', stderr: 'inherit' })
  const code = await child.exited
  if (code !== 0) throw new Error(`${basename(cmd[0]!)} exited with status ${code}`)
}

async function waitForRuntimeRecord(
  root: string,
  expectedOrigin: string,
  daemonProcess: ReturnType<typeof Bun.spawn>,
): Promise<string> {
  const deadline = Date.now() + 60_000
  while (Date.now() < deadline) {
    if (daemonProcess.exitCode !== null) {
      throw new Error(`daemon exited during startup with status ${daemonProcess.exitCode}`)
    }
    for (const candidate of await runtimeRecords(root)) {
      try {
        const record = JSON.parse(await readFile(candidate, 'utf8')) as {
          metadata?: Record<string, string>
        }
        if (record.metadata?.web_origin === expectedOrigin) return candidate
      } catch {
        // The daemon may still be atomically publishing the runtime record.
      }
    }
    await new Promise((resolve) => setTimeout(resolve, 50))
  }
  throw new Error('daemon runtime did not become ready')
}

async function waitForPing(
  backend: string,
  browserOrigin: string,
  daemonProcess: ReturnType<typeof Bun.spawn>,
): Promise<void> {
  const deadline = Date.now() + 30_000
  while (Date.now() < deadline) {
    if (daemonProcess.exitCode !== null) {
      throw new Error(`daemon exited during startup with status ${daemonProcess.exitCode}`)
    }
    if (await ping(backend, browserOrigin)) return
    await new Promise((resolve) => setTimeout(resolve, 50))
  }
  throw new Error('daemon ping did not become ready')
}

function ping(backend: string, browserOrigin: string): Promise<boolean> {
  return new Promise((resolve) => {
    const url = new URL('/api/v1/ping', backend)
    const pingRequest = request(
      url,
      { headers: { Host: new URL(browserOrigin).host } },
      (response) => {
        response.resume()
        response.once('end', () => resolve(response.statusCode === 200))
      },
    )
    pingRequest.once('error', () => resolve(false))
    pingRequest.setTimeout(1_000, () => {
      pingRequest.destroy()
      resolve(false)
    })
    pingRequest.end()
  })
}

async function runtimeRecords(root: string): Promise<string[]> {
  const found: string[] = []
  const visit = async (directory: string): Promise<void> => {
    let entries
    try {
      entries = await readdir(directory, { withFileTypes: true })
    } catch {
      return
    }
    await Promise.all(
      entries.map(async (entry) => {
        const path = join(directory, entry.name)
        if (entry.isDirectory()) await visit(path)
        else if (/^daemon\.\d+\.json$/.test(entry.name)) found.push(path)
      }),
    )
  }
  await visit(root)
  return found.sort()
}

async function cleanupStaleRuns(root: string): Promise<void> {
  let entries
  try {
    entries = await readdir(root, { withFileTypes: true })
  } catch {
    return
  }
  await Promise.all(
    entries.map(async (entry) => {
      if (!entry.isDirectory() || !entry.name.startsWith('run-')) return
      const path = join(root, entry.name)
      try {
        const marker = JSON.parse(await readFile(join(path, '.kata-web-dev-owned'), 'utf8')) as {
          pid?: number
        }
        if (typeof marker.pid !== 'number' || processIsRunning(marker.pid)) return
        await rm(path, { recursive: true, force: true })
      } catch {
        // Directories without the ownership marker are never removed.
      }
    }),
  )
}

function processIsRunning(pid: number): boolean {
  try {
    process.kill(pid, 0)
    return true
  } catch {
    return false
  }
}

function availablePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const server = createServer()
    server.unref()
    server.once('error', reject)
    server.listen(0, '127.0.0.1', () => {
      const address = server.address()
      if (!address || typeof address === 'string') {
        server.close()
        reject(new Error('could not reserve a loopback port'))
        return
      }
      server.close((error) => (error ? reject(error) : resolve(address.port)))
    })
  })
}

function parsePort(raw: string, name: string): number {
  const port = Number.parseInt(raw, 10)
  if (!Number.isInteger(port) || port < 1 || port > 65_535) {
    throw new Error(`${name} must be an integer from 1 through 65535`)
  }
  return port
}
