import { describe, expect, it } from 'vitest'

import { relativeTime } from './dates'

describe('relativeTime', () => {
  const now = new Date('2026-05-16T10:00:00Z')

  it('uses readable relative labels for recent updates', () => {
    expect(relativeTime('2026-05-16T09:59:30Z', now)).toBe('just now')
    expect(relativeTime('2026-05-16T09:55:00Z', now)).toBe('5m ago')
    expect(relativeTime('2026-05-16T07:00:00Z', now)).toBe('3h ago')
    expect(relativeTime('2026-05-14T10:00:00Z', now)).toBe('2d ago')
  })

  it('uses calendar labels for older updates and missing labels for invalid dates', () => {
    expect(relativeTime('2026-05-01T10:00:00Z', now)).toBe('May 1')
    expect(relativeTime('2025-12-01T10:00:00Z', now)).toBe("Dec 1 '25")
    expect(relativeTime('invalid', now)).toBe('-')
  })
})
