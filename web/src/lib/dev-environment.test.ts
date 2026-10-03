import { access } from 'node:fs/promises'
import { dirname } from 'node:path'

import { describe, expect, it } from 'vitest'

import * as devEnvironment from './dev-environment'

const { createDevChildEnvironment, devDaemonCommand, prepareBrowserTestRuntime } = devEnvironment

describe('createDevChildEnvironment', () => {
  it('replaces inherited daemon targets with isolated development paths', () => {
    const environment = createDevChildEnvironment(
      {
        PATH: '/example/bin',
        HOME: '/example/home',
        SystemRoot: 'C:\\Windows',
        TEMP: '/example/tmp',
        GOFLAGS:
          "'-p=2' -p=2 -p 2 -mod=mod -tags=example '-ldflags=-X example.message=hello world'",
        GOMAXPROCS: '2',
        KATA_DSN: 'postgres://daemon.example/kata',
        KATA_DB: '/var/lib/kata.db',
        KATA_SERVER: 'https://daemon.example',
        KATA_WORKSPACE: '/srv/example-workspace',
        KATA_CONFIG: '/srv/kata/config.toml',
        KATA_AUTH_TOKEN: 'secret-value',
        KATA_WEB_PUBLIC_ORIGIN: 'https://daemon.example',
        KATA_TRUST_PRIVATE_NETWORK: '1',
        KATA_ALLOW_UNAUTHENTICATED_PRIVATE_NETWORK_WRITES: '1',
        DATABASE_URL: 'postgres://daemon.example/production',
        HTTP_PROXY: 'http://proxy.example:8080',
        EXAMPLE_SERVICE_TOKEN: 'service-secret',
        PORT: '8080',
      },
      {
        home: '/tmp/kata-web/home',
        workspace: '/tmp/kata-web/workspace',
        database: '/tmp/kata-web/home/kata.db',
      },
    )

    expect(environment).toEqual({
      PATH: '/example/bin',
      HOME: '/example/home',
      SystemRoot: 'C:\\Windows',
      TEMP: '/example/tmp',
      GOFLAGS: "-mod=mod -tags=example '-ldflags=-X example.message=hello world'",
      KATA_HOME: '/tmp/kata-web/home',
      KATA_DB: '/tmp/kata-web/home/kata.db',
      KATA_WORKSPACE: '/tmp/kata-web/workspace',
      KATA_AUTHOR: 'user-a',
    })
  })
})

describe('devDaemonCommand', () => {
  it('binds the Windows shared daemon listener to the selected backend port', () => {
    expect(devDaemonCommand('kata.exe', 43127, 'win32')).toEqual([
      'kata.exe',
      'daemon',
      'start',
      '--foreground',
      '--listen',
      '127.0.0.1:43127',
    ])
  })
})

describe('developmentKataBuildArguments', () => {
  it('compiles the development daemon with Kit telemetry disabled', () => {
    const helper = (devEnvironment as Record<string, unknown>).developmentKataBuildArguments

    expect(helper).toBeTypeOf('function')
    expect((helper as (binary: string) => string[])('/tmp/kata')).toEqual([
      'build',
      '-tags',
      'kit_posthog_disabled',
      '-trimpath',
      '-buildvcs=false',
      '-o',
      '/tmp/kata',
      './cmd/kata',
    ])
  })
})

describe('prepareBrowserTestRuntime', () => {
  it('embeds and builds once before workers share a binary, then restores the stub once', async () => {
    const commands: { cmd: string[]; cwd: string; env: NodeJS.ProcessEnv }[] = []
    const runtime = await prepareBrowserTestRuntime({
      repositoryRoot: '/example/repository',
      inheritedEnvironment: {
        PATH: '/example/bin',
        GOFLAGS:
          "'-p=2' -p=2 -p 2 -mod=mod -tags=example '-ldflags=-X example.message=hello world'",
        GOMAXPROCS: '2',
      },
      runCommand(cmd, cwd, env) {
        commands.push({ cmd, cwd, env })
      },
    })

    expect(commands).toHaveLength(3)
    expect(commands[0]?.cmd).toEqual(['make', 'web-build', 'web-assets-check'])
    expect(commands[1]?.cmd).toEqual(['bun', 'run', 'scripts/embed-assets.ts', 'dist'])
    expect(commands[2]?.cmd).toEqual([
      'go',
      'build',
      '-tags',
      'kit_posthog_disabled',
      '-trimpath',
      '-buildvcs=false',
      '-o',
      runtime.binary,
      './cmd/kata',
    ])
    expect(commands[0]?.env.KATA_HOME).toBeTruthy()
    expect(commands[0]?.env.KATA_DB).toBe(`${commands[0]?.env.KATA_HOME}/kata.db`)
    expect(commands[0]?.env.GOFLAGS).toBe(
      "-mod=mod -tags=example '-ldflags=-X example.message=hello world'",
    )
    expect(commands[0]?.env.GOMAXPROCS).toBeUndefined()

    await runtime.teardown()
    await runtime.teardown()

    expect(commands).toHaveLength(4)
    expect(commands[3]?.cmd).toEqual(['bun', 'run', 'scripts/embed-assets.ts', '--restore-stub'])
  })

  it('restores the embed stub once and removes temporary state when setup fails mid-embed', async () => {
    const commands: { cmd: string[]; env: NodeJS.ProcessEnv }[] = []
    const setupError = new Error('embed interrupted')
    let restoreCount = 0
    await expect(
      prepareBrowserTestRuntime({
        repositoryRoot: '/example/repository',
        inheritedEnvironment: { PATH: '/example/bin' },
        runCommand(cmd, _cwd, env) {
          commands.push({ cmd, env })
          if (
            cmd.at(-1) === 'dist' &&
            cmd.some((argument) => argument.endsWith('/embed-assets.ts'))
          )
            throw setupError
          if (cmd.includes('--restore-stub')) restoreCount++
        },
      }),
    ).rejects.toBe(setupError)

    expect(restoreCount).toBe(1)
    const home = commands[0]?.env.KATA_HOME
    expect(home).toBeTruthy()
    await expect(access(dirname(home!))).rejects.toMatchObject({ code: 'ENOENT' })
  })
})
