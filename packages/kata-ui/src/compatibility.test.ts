import { describe, expect, it } from 'vitest'

import { supportsKataAPISchema } from './compatibility.js'

describe('supportsKataAPISchema', () => {
  it.each(['0.9.0', '0.9.8', '0.10.0', '0.10.4', '0.11.0', '0.11.3'])('accepts %s', (version) => {
    expect(supportsKataAPISchema(version)).toBe(true)
  })

  it.each(['', '0.8.9', '0.12.0', '1.0.0', 'not-semver'])('rejects %s', (version) => {
    expect(supportsKataAPISchema(version)).toBe(false)
  })
})
