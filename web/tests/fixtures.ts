import { spawn, spawnSync, type ChildProcess } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { mkdtemp, mkdir, readFile, readdir, rm, writeFile } from 'node:fs/promises'
import { createServer } from 'node:net'
import { join, resolve } from 'node:path'
import { tmpdir } from 'node:os'

import { expect, test as base, type APIResponse, type Page } from '@playwright/test'

import { createDevChildEnvironment } from '../src/lib/dev-environment'

interface RuntimeRecord {
  metadata?: Record<string, string>
}

export interface BrowserCredentials {
  session: string
  csrf: string
}

export interface SeededIssue {
  id: number
  uid: string
  short_id: string
  qualified_id: string
  project_id: number
  project_uid: string
  title: string
  revision: number
}

interface KataFixture {
  origin: string
  projectID: number
  projectUID: string
  launch(page: Page, route?: string): Promise<BrowserCredentials>
  snapshot(
    page: Page,
    credentials: BrowserCredentials,
    query?: string,
  ): Promise<Record<string, unknown>>
  request(
    page: Page,
    credentials: BrowserCredentials,
    method: string,
    path: string,
    data?: unknown,
    headers?: Record<string, string>,
  ): Promise<APIResponse>
  seedIssue(
    page: Page,
    credentials: BrowserCredentials,
    input: Record<string, unknown> & { title: string },
  ): Promise<SeededIssue>
  restart(): Promise<void>
  restartReadonly(): Promise<void>
}

interface RunningFixture {
  fixture: KataFixture
  stop(): Promise<void>
}

export const test = base.extend<{ kata: KataFixture }>({
  kata: [
    async ({ browserName }, use) => {
      void browserName
      const running = await startProductionFixture()
      try {
        await use(running.fixture)
      } finally {
        await running.stop()
      }
    },
    { scope: 'worker', timeout: 120_000 },
  ],
})

export { expect }

async function startProductionFixture(): Promise<RunningFixture> {
  const repositoryRoot = resolve(process.cwd(), '..')
  const root = await mkdtemp(join(tmpdir(), 'kata-web-browser-e2e-'))
  const home = join(root, 'home')
  const workspace = join(root, 'workspace')
  const remoteHome = join(root, 'remote-home')
  const remoteWorkspace = join(root, 'remote-workspace')
  const binary = join(root, 'kata')
  await Promise.all([mkdir(home), mkdir(workspace), mkdir(remoteHome), mkdir(remoteWorkspace)])

  run('make', ['web-embed'], repositoryRoot, process.env)
  run(
    'go',
    ['build', '-trimpath', '-buildvcs=false', '-o', binary, './cmd/kata'],
    repositoryRoot,
    process.env,
  )

  const [port, remotePort] = await Promise.all([freePort(), freePort()])
  const origin = `http://127.0.0.1:${port}`
  const remoteOrigin = `http://127.0.0.1:${remotePort}`
  await writeFile(
    join(home, 'config.toml'),
    `active_daemon = "example-local"

[[daemon]]
name = "example-local"
local = true

[[daemon]]
name = "example-remote"
url = "${remoteOrigin}"
token = "example-remote-token"
allow_insecure = true

[web]
listen = "127.0.0.1:${port}"
`,
    { mode: 0o600 },
  )
  await writeFile(
    join(remoteHome, 'config.toml'),
    `listen = "127.0.0.1:${remotePort}"

[auth]
token = "example-remote-token"
`,
    { mode: 0o600 },
  )
  const environment = {
    ...createDevChildEnvironment(process.env, {
      home,
      workspace,
      database: join(home, 'kata.db'),
    }),
    KATA_AUTH_TOKEN: '',
    KATA_SERVER: '',
  }
  const remoteEnvironment = {
    ...createDevChildEnvironment(process.env, {
      home: remoteHome,
      workspace: remoteWorkspace,
      database: join(remoteHome, 'kata.db'),
    }),
    KATA_AUTH_TOKEN: 'example-remote-token',
    KATA_SERVER: '',
  }

  const remoteDaemon = startDaemon(binary, remoteWorkspace, remoteEnvironment)
  await waitForPing(remoteOrigin, remoteDaemon)
  run(binary, ['projects', 'create', 'example-remote-project'], remoteWorkspace, remoteEnvironment)
  await writeFile(
    join(remoteWorkspace, '.kata.toml'),
    'version = 1\n\n[project]\nname = "example-remote-project"\n',
  )
  run(binary, ['create', 'Remote daemon task'], remoteWorkspace, remoteEnvironment)
  const directRemoteSnapshot = await fetch(`${remoteOrigin}/api/v1/ui/snapshot?view=all-open`, {
    headers: { Authorization: 'Bearer example-remote-token' },
  })
  if (!directRemoteSnapshot.ok) {
    throw new Error(`direct fixture remote snapshot failed: ${directRemoteSnapshot.status}`)
  }
  let daemon = startDaemon(binary, workspace, environment)
  await waitForPing(origin, daemon)
  run(binary, ['projects', 'create', 'example-project'], workspace, environment)
  run(binary, ['projects', 'create', 'example-inbox'], workspace, environment)
  await writeFile(
    join(workspace, '.kata.toml'),
    'version = 1\n\n[project]\nname = "example-project"\n',
  )

  const project = await discoverProject(origin, home)

  const fixture: KataFixture = {
    origin,
    projectID: project.id,
    projectUID: project.uid,
    async launch(page, route = '/kata?view=all-open') {
      await page.goto(`${origin}${route}`)
      await expect(page.getByRole('button', { name: 'New task' })).toBeVisible()
      const credentials = await page.evaluate(() => {
        const value = sessionStorage.getItem('kata.web.session.v1')
        if (!value) return null
        const parsed = JSON.parse(value) as { session?: unknown; csrf?: unknown }
        return {
          session: typeof parsed.session === 'string' ? parsed.session : '',
          csrf: typeof parsed.csrf === 'string' ? parsed.csrf : '',
        }
      })
      if (!credentials?.session || !credentials.csrf)
        throw new Error('browser session was not stored')
      return credentials
    },
    async snapshot(page, credentials, query = 'view=all-open') {
      const response = await fixture.request(
        page,
        credentials,
        'GET',
        `/api/v1/ui/snapshot?${query}`,
      )
      expect(response.ok()).toBe(true)
      return (await response.json()) as Record<string, unknown>
    },
    async request(page, credentials, method, path, data, headers = {}) {
      return page.request.fetch(`${origin}${path}`, {
        method,
        headers: {
          Origin: origin,
          'X-Kata-Web-Session': credentials.session,
          ...(method === 'GET' || method === 'HEAD' ? {} : { 'X-Kata-CSRF': credentials.csrf }),
          ...headers,
        },
        ...(data === undefined ? {} : { data }),
      })
    },
    async seedIssue(page, credentials, input) {
      const response = await fixture.request(
        page,
        credentials,
        'POST',
        `/api/v1/projects/${project.id}/issues`,
        { actor: 'user-a', force_new: true, ...input },
        { 'Idempotency-Key': randomUUID() },
      )
      if (!response.ok()) throw new Error(`issue seed failed with status ${response.status()}`)
      return ((await response.json()) as { issue: SeededIssue }).issue
    },
    async restart() {
      await stopDaemon(daemon)
      daemon = startDaemon(binary, workspace, environment)
      await waitForPing(origin, daemon)
    },
    async restartReadonly() {
      await stopDaemon(daemon)
      daemon = startDaemon(binary, workspace, environment, ['--insecure-readonly'])
      await waitForPing(origin, daemon)
    },
  }

  return {
    fixture,
    async stop() {
      await Promise.all([stopDaemon(daemon), stopDaemon(remoteDaemon)])
      run(
        'bun',
        ['run', 'scripts/embed-assets.ts', '--restore-stub'],
        join(repositoryRoot, 'web'),
        process.env,
      )
      await rm(root, { recursive: true, force: true })
    },
  }
}

function run(command: string, args: string[], cwd: string, env: NodeJS.ProcessEnv): string {
  const result = spawnSync(command, args, { cwd, env, encoding: 'utf8' })
  if (result.status !== 0) {
    throw new Error(`${command} failed with status ${result.status}: ${result.stderr}`)
  }
  return result.stdout
}

function startDaemon(
  binary: string,
  cwd: string,
  env: NodeJS.ProcessEnv,
  extraArgs: string[] = [],
): ChildProcess {
  return spawn(binary, ['daemon', 'start', '--foreground', ...extraArgs], {
    cwd,
    env,
    stdio: ['ignore', 'ignore', 'pipe'],
  })
}

async function stopDaemon(daemon: ChildProcess): Promise<void> {
  if (daemon.exitCode !== null) return
  daemon.kill('SIGTERM')
  await Promise.race([
    new Promise<void>((resolve) => daemon.once('exit', () => resolve())),
    new Promise<void>((resolve) => setTimeout(resolve, 5_000)),
  ])
  if (daemon.exitCode === null) daemon.kill('SIGKILL')
}

async function waitForPing(origin: string, daemon: ChildProcess): Promise<void> {
  const deadline = Date.now() + 15_000
  while (Date.now() < deadline) {
    if (daemon.exitCode !== null)
      throw new Error(`Kata daemon exited with status ${daemon.exitCode}`)
    try {
      if ((await fetch(`${origin}/api/v1/ping`)).ok) return
    } catch {
      // Listener startup is still in progress.
    }
    await new Promise((resolve) => setTimeout(resolve, 50))
  }
  throw new Error('Kata browser listener did not become ready')
}

async function waitForRuntime(home: string, origin: string): Promise<string> {
  const deadline = Date.now() + 10_000
  while (Date.now() < deadline) {
    for (const path of await runtimeRecords(home)) {
      try {
        const record = JSON.parse(await readFile(path, 'utf8')) as RuntimeRecord
        if (record.metadata?.web_origin === origin) return path
      } catch {
        // Runtime publication may still be in progress.
      }
    }
    await new Promise((resolve) => setTimeout(resolve, 50))
  }
  throw new Error('Kata browser runtime record did not become ready')
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
    for (const entry of entries) {
      const path = join(directory, entry.name)
      if (entry.isDirectory()) await visit(path)
      else if (/^daemon\.\d+\.json$/.test(entry.name)) found.push(path)
    }
  }
  await visit(root)
  return found
}

async function discoverProject(origin: string, home: string): Promise<{ id: number; uid: string }> {
  await waitForRuntime(home, origin)
  const sessionResponse = await fetch(`${origin}/api/v1/ui/session/local`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Origin: origin },
    body: JSON.stringify({ return_path: '/kata?view=all-open' }),
  })
  const cookie = sessionResponse.headers.get('set-cookie')?.split(';', 1)[0]
  const credentials = (await sessionResponse.json()) as { session: string; csrf: string }
  const snapshot = await fetch(`${origin}/api/v1/ui/snapshot?view=all-open`, {
    headers: { Cookie: cookie ?? '', 'X-Kata-Web-Session': credentials.session },
  })
  const body = (await snapshot.json()) as {
    catalog: Array<{ project: { id: number; uid: string; name: string } }>
  }
  const project = body.catalog.find(({ project }) => project.name === 'example-project')?.project
  if (!project) throw new Error('neutral project was not visible in the production snapshot')
  const inbox = body.catalog.find(({ project }) => project.name === 'example-inbox')?.project
  if (!inbox) throw new Error('neutral Inbox project was not visible in the production snapshot')
  const metadata = await fetch(`${origin}/api/v1/projects/${inbox.id}/metadata`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Cookie: cookie ?? '',
      Origin: origin,
      'X-Kata-Web-Session': credentials.session,
      'X-Kata-CSRF': credentials.csrf,
    },
    body: JSON.stringify({ actor: 'user-a', patch: { role: 'inbox' } }),
  })
  if (!metadata.ok) throw new Error(`failed to designate fixture Inbox: ${metadata.status}`)
  const roster = await fetch(`${origin}/api/v1/ui/daemons`, {
    headers: { Cookie: cookie ?? '', 'X-Kata-Web-Session': credentials.session },
  })
  if (!roster.ok) throw new Error(`failed to load fixture daemon roster: ${roster.status}`)
  const daemonBody = (await roster.json()) as { daemons?: Array<{ id?: string }> }
  if (!daemonBody.daemons?.some((daemon) => daemon.id === 'example-remote')) {
    throw new Error('fixture remote daemon was not present in the browser roster')
  }
  const remoteSnapshot = await fetch(`${origin}/api/v1/ui/proxy/api/v1/ui/snapshot?view=all-open`, {
    headers: {
      Cookie: cookie ?? '',
      'X-Kata-Web-Session': credentials.session,
      'X-Kata-Web-Daemon': 'example-remote',
    },
  })
  if (!remoteSnapshot.ok) {
    const failure = (await remoteSnapshot.json()) as { error?: { code?: string } }
    throw new Error(
      `fixture remote snapshot failed with status ${remoteSnapshot.status}: ${failure.error?.code ?? 'unknown'}`,
    )
  }
  return project
}

async function freePort(): Promise<number> {
  return new Promise((resolvePort, reject) => {
    const server = createServer()
    server.once('error', reject)
    server.listen(0, '127.0.0.1', () => {
      const address = server.address()
      if (!address || typeof address === 'string') {
        server.close(() => reject(new Error('failed to allocate browser port')))
        return
      }
      server.close(() => resolvePort(address.port))
    })
  })
}
