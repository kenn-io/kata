import { describe, expect, it } from 'vitest'

import * as devEnvironment from './dev-environment'

const { createDevChildEnvironment, devDaemonCommand } = devEnvironment

describe('createDevChildEnvironment', () => {
  it('replaces inherited daemon targets with isolated development paths', () => {
    const environment = createDevChildEnvironment(
      {
        PATH: '/example/bin',
        HOME: '/example/home',
        SystemRoot: 'C:\\Windows',
        TEMP: '/example/tmp',
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
