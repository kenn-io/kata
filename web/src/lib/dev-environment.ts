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
  return {
    ...environment,
    KATA_HOME: paths.home,
    KATA_DB: paths.database,
    KATA_WORKSPACE: paths.workspace,
    KATA_AUTHOR: 'user-a',
  }
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
