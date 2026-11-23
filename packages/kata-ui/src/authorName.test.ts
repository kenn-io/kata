import { describe, expect, it } from 'vitest'
import { authorName } from './authorName.js'

describe('authorName', () => {
  const id = '11111111-1111-4111-8111-111111111111'
  const member = `Ana [member:${id}; enrollment:${id}]`

  it.each([
    [member, 'Ana'],
    [`Bot [service-account:${id}; token:${id}]`, 'Bot'],
    ['Ana', 'Ana'],
    ['Ana [team]', 'Ana [team]'],
    [`Ana [member:${id}; token:${id}]`, `Ana [member:${id}; token:${id}]`],
    [`Ana [member:short; enrollment:${id}]`, `Ana [member:short; enrollment:${id}]`],
    [`${member}\n`, `${member}\n`],
  ])('formats %s', (input, expected) => {
    expect(authorName(input)).toBe(expected)
  })
})
