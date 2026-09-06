import { describe, expect, it } from 'vitest'

import { applicationBaseURL, applicationURL } from './applicationBase'

describe('browser application base', () => {
  it('keeps top-level release assets on the origin root', () => {
    const base = applicationBaseURL(
      new URL('https://daemon.example/assets/app-a1b2c3d4.js'),
      'https://daemon.example',
    )

    expect(base.href).toBe('https://daemon.example/')
    expect(applicationURL('/api/v1/ui/snapshot', base).href).toBe(
      'https://daemon.example/api/v1/ui/snapshot',
    )
  })

  it('keeps API requests beside assets mounted below a path', () => {
    const base = applicationBaseURL(
      new URL('https://daemon.example/tools/tasks/assets/app-a1b2c3d4.js'),
      'https://daemon.example',
    )

    expect(base.href).toBe('https://daemon.example/tools/tasks/')
    expect(applicationURL('/api/v1/ui/snapshot?view=today', base).href).toBe(
      'https://daemon.example/tools/tasks/api/v1/ui/snapshot?view=today',
    )
  })

  it('does not move an already mounted or cross-origin URL', () => {
    const base = new URL('https://daemon.example/tools/tasks/')

    expect(applicationURL('/tools/tasks/api/v1/health', base).href).toBe(
      'https://daemon.example/tools/tasks/api/v1/health',
    )
    expect(applicationURL('https://other.example/api/v1/health', base).href).toBe(
      'https://other.example/api/v1/health',
    )
  })
})
