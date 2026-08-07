import { beforeEach, describe, expect, test } from 'vitest'

import { loadDaemonRoute, saveDaemonRoute } from './state'

describe('daemon workspace state', () => {
  beforeEach(() => localStorage.clear())

  test('restores independent canonical routes per daemon', () => {
    saveDaemonRoute('example-local', '/kata?scope=01J00000000000000000000001')
    saveDaemonRoute('example-remote', '/kata?view=today')

    expect(loadDaemonRoute('example-local')).toBe('/kata?scope=01J00000000000000000000001')
    expect(loadDaemonRoute('example-remote')).toBe('/kata?view=today')
  })

  test('ignores malformed stored routes without disturbing other daemon state', () => {
    localStorage.setItem(
      'kata.web.workspace-state.v1',
      JSON.stringify({
        version: 1,
        daemons: { 'example-local': '/wrong', 'example-remote': '/kata?view=all-open' },
      }),
    )

    expect(loadDaemonRoute('example-local')).toBeUndefined()
    expect(loadDaemonRoute('example-remote')).toBe('/kata?view=all-open')
  })
})
