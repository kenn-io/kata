import { describe, expect, it } from 'vitest'
import { attachmentAvailable, attachmentWritable } from './attachmentAvailability'

const capabilities = {
  actor_policy: 'request',
  updates: 'poll' as const,
  writable: true,
  attachment_instance_uid: 'leaf',
  attachments: false,
  attachment_routes: [{ project_uid: 'spoke', read: true, write: true, origin: 'root' }],
}
const file = { name: 'file.pdf', media_type: 'application/pdf', size: 1, docbank_version: 'id' }

describe('attachment availability', () => {
  it('uses the selected project and reference origin', () => {
    expect(attachmentWritable(capabilities, 'spoke')).toBe(true)
    expect(attachmentWritable(capabilities, 'local')).toBe(false)
    expect(attachmentAvailable(capabilities, 'spoke', { ...file, origin: 'root' })).toBe(true)
    expect(attachmentAvailable(capabilities, 'spoke', file)).toBe(false)
    expect(attachmentAvailable(capabilities, 'local', { ...file, origin: 'root' })).toBe(false)
    expect(attachmentAvailable({ ...capabilities, attachments: true }, 'spoke', file)).toBe(true)
    expect(
      attachmentAvailable(
        {
          ...capabilities,
          attachments: true,
          attachment_routes: [{ project_uid: 'spoke', read: false, write: false }],
        },
        'spoke',
        file,
      ),
    ).toBe(true)
    expect(
      attachmentWritable(
        {
          ...capabilities,
          attachments: true,
          attachment_routes: [{ project_uid: 'spoke', read: false, write: false }],
        },
        'spoke',
      ),
    ).toBe(false)
  })
})
