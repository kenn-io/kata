import { expect, it } from 'vitest'
import { formatDay } from './dates'

it('keeps an invalid calendar date as written', () => {
  expect(formatDay('2026-02-30')).toBe('2026-02-30')
})
