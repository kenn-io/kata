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
  test('marks evidence changed when its origin differs', () => {
    const withoutOrigin = {
      name: 'report.pdf',
      media_type: 'application/pdf',
      size: 5,
      docbank_version: '00000000-0000-4000-8000-000000000001',
    }
    const frozen = { ...withoutOrigin, origin: '01ARZ3NDEKTSV4RRFFQ69G5FAV' }
    const event = closeEvent({
      type: 'attachment',
      attachment_id: 'attachment-1',
      attachment: frozen,
    })

    expect(
      closingAttachments(event, {
        'attachment.attachment-1': { ...frozen, origin: '01ARZ3NDEKTSV4RRFFQ69G5FAW' },
      }),
    ).toEqual([{ label: 'report.pdf · 5 bytes (changed)' }])

    expect(closingAttachments(event, { 'attachment.attachment-1': withoutOrigin })).toEqual([
      { label: 'report.pdf · 5 bytes (changed)' },
    ])
    const legacyEvent = closeEvent({
      type: 'attachment',
      attachment_id: 'attachment-1',
      attachment: withoutOrigin,
    })
    expect(closingAttachments(legacyEvent, { 'attachment.attachment-1': frozen })).toEqual([
      { label: 'report.pdf · 5 bytes (changed)' },
    ])
    expect(closingAttachments(legacyEvent, { 'attachment.attachment-1': withoutOrigin })).toEqual([
      { label: 'report.pdf · 5 bytes', href: '#kata-attachment-attachment-1' },
    ])
  })

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
})
