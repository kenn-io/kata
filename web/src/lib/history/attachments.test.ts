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
})
