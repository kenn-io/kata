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

  test('keeps the frozen file name and size in a valid evidence label', () => {
    const descriptor = {
      name: 'report.pdf',
      media_type: 'application/pdf',
      size: 5,
      docbank_version: '00000000-0000-4000-8000-000000000001',
    }
    const event = closeEvent({
      type: 'attachment',
      attachment_id: 'attachment-1',
      attachment: descriptor,
    })

    expect(closingAttachments(event, { 'attachment.attachment-1': descriptor })).toEqual([
      { label: 'report.pdf · 5 bytes', href: '#kata-attachment-attachment-1' },
    ])
  })
})
