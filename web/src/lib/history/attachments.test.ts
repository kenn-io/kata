import { describe, expect, test } from 'vitest'

import type { KataTaskEvent } from '../kata/types'
import { closingAttachments } from './attachments'

function closeEvent(evidence: Record<string, unknown>): KataTaskEvent {
  return {
    event_id: 1,
    event_uid: 'event-1',
    origin_instance_uid: 'instance-1',
    type: 'issue.closed',
    project_id: 1,
    project_uid: 'project-1',
    project_name: 'example-project',
    actor: 'tester',
    payload: { evidence: [evidence] },
    created_at: '2026-11-03T00:00:00Z',
  }
}

describe('closingAttachments', () => {
  test('uses a plain label for invalid frozen attachment details', () => {
    const event = closeEvent({
      type: 'attachment',
      attachment_id: 'attachment-1',
      attachment: { name: '', media_type: 'application/pdf', size: 5, docbank_version: 'bad' },
    })

    expect(closingAttachments(event, {})).toEqual([{ label: 'Invalid attachment' }])
  })

  test('uses a plain label for a legacy attachment without current metadata', () => {
    const event = closeEvent({ type: 'attachment', attachment_id: 'attachment-1' })

    expect(closingAttachments(event, {})).toEqual([{ label: 'Attachment removed' }])
  })

  test('marks an origin-only change and removes the browser link', () => {
    const event = closeEvent({
      type: 'attachment',
      attachment_id: 'attachment-1',
      attachment: {
        origin: '01ARZ3NDEKTSV4RRFFQ69G5FAV',
        name: 'report.pdf',
        media_type: 'application/pdf',
        size: 5,
        docbank_version: '00000000-0000-4000-8000-000000000001',
      },
    })

    expect(
      closingAttachments(event, {
        'attachment.attachment-1': {
          origin: '01ARZ3NDEKTSV4RRFFQ69G5FAW',
          name: 'report.pdf',
          media_type: 'application/pdf',
          size: 5,
          docbank_version: '00000000-0000-4000-8000-000000000001',
        },
      }),
    ).toEqual([{ label: 'report.pdf · 5 bytes (changed)' }])
  })

  test('treats a missing and local origin as equivalent', () => {
    const event = closeEvent({
      type: 'attachment',
      attachment_id: 'attachment-1',
      attachment: {
        name: 'report.pdf',
        media_type: 'application/pdf',
        size: 5,
        docbank_version: '00000000-0000-4000-8000-000000000001',
      },
    })

    expect(
      closingAttachments(event, {
        'attachment.attachment-1': {
          origin: 'local',
          name: 'report.pdf',
          media_type: 'application/pdf',
          size: 5,
          docbank_version: '00000000-0000-4000-8000-000000000001',
        },
      }),
    ).toEqual([{ label: 'report.pdf · 5 bytes', href: '#kata-attachment-attachment-1' }])
  })
})
