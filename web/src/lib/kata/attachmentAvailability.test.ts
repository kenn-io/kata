import { describe, expect, it } from 'vitest'
import { attachmentAvailable, attachmentWritable } from './attachmentAvailability'

const capabilities = {
  actor_policy: 'request',
  updates: 'poll' as const,
  writable: true,
  attachment_local_origins: ['01ARZ3NDEKTSV4RRFFQ69G5FAV', '01ARZ3NDEKTSV4RRFFQ69G5FAU'],
  attachments: true,
}
const project = '01ARZ3NDEKTSV4RRFFQ69G5FAX'
const route = {
  project_uid: project,
  read: true,
  write: true,
  origin: '01ARZ3NDEKTSV4RRFFQ69G5FAW',
}
const file = {
  name: 'file.pdf',
  media_type: 'application/pdf',
  size: 1,
  docbank_version: '6f1c2e8a-2222-4222-8222-222222222222',
}

describe('attachment availability', () => {
  it('gates shared controls while a project route is pending', () => {
    expect(attachmentWritable(capabilities, project, undefined)).toBe(false)
    expect(attachmentAvailable(capabilities, project, undefined, file)).toBe(true)
    expect(
      attachmentAvailable(capabilities, project, undefined, { ...file, origin: '01ARZ3NDEKTSV4RRFFQ69G5FAU' }),
    ).toBe(true)
    expect(
      attachmentAvailable(capabilities, project, undefined, { ...file, origin: route.origin }),
    ).toBe(false)
    expect(attachmentWritable(capabilities, project, null)).toBe(true)
    expect(attachmentWritable(capabilities, project, route)).toBe(true)
    expect(attachmentWritable(capabilities, 'other-project', route)).toBe(false)
    expect(
      attachmentAvailable(capabilities, project, route, { ...file, origin: route.origin }),
    ).toBe(true)
    expect(
      attachmentAvailable(capabilities, project, route, { ...file, origin: 'another-origin' }),
    ).toBe(false)
    expect(attachmentWritable(capabilities, project, { ...route, write: false })).toBe(false)
    expect(attachmentAvailable(capabilities, project, { ...route, read: false }, file)).toBe(true)
  })
})
