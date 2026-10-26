import { expect, test } from 'vitest'
import * as hegel from '@hegeldev/hegel'
import * as gs from '@hegeldev/hegel/generators'
import { compareInstants } from './time'

test.each([
  ['2026-10-25T10:00:00Z', '2026-10-25T10:00:00.001Z'],
  ['2026-10-25T10:00:00.12Z', '2026-10-25T10:00:00.121Z'],
  ['2026-10-25T10:00:00.000000001Z', '2026-10-25T10:00:00.000000002Z'],
  ['2026-10-25T10:00:00.999999999Z', '2026-10-25T10:00:01Z'],
])('orders exact API instants %s before %s', (earlier, later) => {
  expect(compareInstants(earlier, later)).toBe(-1)
  expect(compareInstants(later, earlier)).toBe(1)
})
test('normalizes equivalent fractional widths and offsets', () => {
  expect(compareInstants('2026-10-25T10:00:00.12Z', '2026-10-25T11:00:00.120000000+01:00')).toBe(0)
})
test('fractional timestamp ordering matches independent nanosecond integers', () =>
  hegel.test(
    (tc) => {
      const first = tc.draw(gs.integers({ minValue: 0, maxValue: 999999999 }))
      const second = tc.draw(gs.integers({ minValue: 0, maxValue: 999999999 }))
      const wire = (n: number) => {
        const fraction = String(n).padStart(9, '0').replace(/0+$/, '')
        return `2026-10-25T10:00:00${fraction ? '.' + fraction : ''}Z`
      }
      expect(compareInstants(wire(first), wire(second))).toBe(Math.sign(first - second))
    },
    { testCases: 2000 },
  ))
