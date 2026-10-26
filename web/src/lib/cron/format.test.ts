import { expect, test } from 'vitest'
import { jobTags } from './format'

test('job tags reject malformed values', () => {
  expect(jobTags({ tags: [1, 'a'] })).toEqual([])
})
