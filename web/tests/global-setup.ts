import { resolve } from 'node:path'

import { prepareBrowserTestRuntime } from '../src/lib/dev-environment'

export default async function globalSetup(): Promise<() => Promise<void>> {
  const repositoryRoot = resolve(process.cwd(), '..')
  const runtime = await prepareBrowserTestRuntime({
    repositoryRoot,
    inheritedEnvironment: process.env,
  })
  process.env.KATA_WEB_E2E_BINARY = runtime.binary

  return async () => {
    try {
      await runtime.teardown()
    } finally {
      delete process.env.KATA_WEB_E2E_BINARY
    }
  }
}
