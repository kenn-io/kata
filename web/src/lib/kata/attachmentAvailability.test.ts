import { describe, expect, it } from 'vitest'
import { attachmentAvailable, attachmentWritable } from './attachmentAvailability'

const capabilities = {
  actor_policy: 'request',
  updates: 'poll' as const,
  writable: true,
  attachment_instance_uid: '01ARZ3NDEKTSV4RRFFQ69G5FAV',
  attachments: false,
  attachment_routes: [
    {
      project_uid: '01ARZ3NDEKTSV4RRFFQ69G5FAX',
      read: true,
      write: true,
      origin: '01ARZ3NDEKTSV4RRFFQ69G5FAW',
    },
  ],
}
const file = {
  name: 'file.pdf',
  media_type: 'application/pdf',
  size: 1,
  docbank_version: '6f1c2e8a-2222-4222-8222-222222222222',
}

describe('attachment availability', () => {
  it('uses the selected project and reference origin', () => {
    const older = {
      actor_policy: 'request',
      updates: 'poll' as const,
      writable: true,
      attachments: true,
    }
    expect(attachmentWritable(older, '01ARZ3NDEKTSV4RRFFQ69G5FAX')).toBe(true)
    expect(attachmentAvailable(older, '01ARZ3NDEKTSV4RRFFQ69G5FAX', file)).toBe(true)
    expect(attachmentWritable(capabilities, '01ARZ3NDEKTSV4RRFFQ69G5FAX')).toBe(true)
    expect(attachmentWritable(capabilities, '01ARZ3NDEKTSV4RRFFQ69G5FAY')).toBe(false)
    expect(
      attachmentAvailable(capabilities, '01ARZ3NDEKTSV4RRFFQ69G5FAX', {
        ...file,
        origin: '01ARZ3NDEKTSV4RRFFQ69G5FAW',
      }),
    ).toBe(true)
    expect(attachmentAvailable(capabilities, '01ARZ3NDEKTSV4RRFFQ69G5FAX', file)).toBe(false)
    expect(
      attachmentAvailable(capabilities, '01ARZ3NDEKTSV4RRFFQ69G5FAY', {
        ...file,
        origin: '01ARZ3NDEKTSV4RRFFQ69G5FAW',
      }),
    ).toBe(false)
    expect(
      attachmentAvailable(
        { ...capabilities, attachments: true },
        '01ARZ3NDEKTSV4RRFFQ69G5FAX',
        file,
      ),
    ).toBe(true)
    expect(
      attachmentAvailable(
        {
          ...capabilities,
          attachments: true,
          attachment_routes: [
            { project_uid: '01ARZ3NDEKTSV4RRFFQ69G5FAX', read: false, write: false },
          ],
        },
        '01ARZ3NDEKTSV4RRFFQ69G5FAX',
        file,
      ),
    ).toBe(true)
    expect(
      attachmentWritable(
        {
          ...capabilities,
          attachments: true,
          attachment_routes: [
            { project_uid: '01ARZ3NDEKTSV4RRFFQ69G5FAX', read: false, write: false },
          ],
        },
        '01ARZ3NDEKTSV4RRFFQ69G5FAX',
      ),
    ).toBe(false)
  })
})
