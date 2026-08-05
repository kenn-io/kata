import { describe, expect, test } from 'vitest'

import type { KataTaskEvent } from '../kata/types'
import { describeKataEvent, type KataEventTone } from './format'

function makeEvent(type: string, payload?: Record<string, unknown>): KataTaskEvent {
  return {
    event_id: 1,
    event_uid: 'evt_1',
    origin_instance_uid: 'kata_1',
    type,
    project_id: 1,
    project_uid: 'proj_1',
    project_name: 'example-project',
    actor: 'tester',
    payload,
    created_at: '2026-05-16T00:00:00Z',
  }
}

describe('describeKataEvent', () => {
  test.each<[string, string, KataEventTone]>([
    ['issue.created', 'created the task', 'neutral'],
    ['issue.reopened', 'reopened', 'warning'],
    ['issue.commented', 'commented', 'neutral'],
    ['issue.unassigned', 'unassigned', 'neutral'],
    ['issue.priority_cleared', 'cleared priority', 'neutral'],
    ['issue.updated', 'updated the task', 'neutral'],
    ['issue.linked', 'linked', 'neutral'],
    ['issue.unlinked', 'unlinked', 'neutral'],
    ['issue.soft_deleted', 'deleted', 'negative'],
    ['issue.restored', 'restored', 'positive'],
  ])('payload-free event %s maps to label %s with tone %s', (type, label, tone) => {
    const descriptor = describeKataEvent(makeEvent(type))
    expect(descriptor.label).toBe(label)
    expect(descriptor.tone).toBe(tone)
    expect(descriptor.icon).toBeDefined()
  })

  test('issue.closed includes the reason from payload', () => {
    const descriptor = describeKataEvent(makeEvent('issue.closed', { reason: 'wontfix' }))
    expect(descriptor.label).toBe('closed (wontfix)')
    expect(descriptor.tone).toBe('positive')
  })

  test('issue.closed defaults to done when reason missing', () => {
    expect(describeKataEvent(makeEvent('issue.closed')).label).toBe('closed (done)')
  })

  test('issue.labeled formats the label value', () => {
    expect(describeKataEvent(makeEvent('issue.labeled', { label: 'p0' })).label).toBe(
      'added label p0',
    )
  })

  test('issue.unlabeled formats the label value', () => {
    expect(describeKataEvent(makeEvent('issue.unlabeled', { label: 'stale' })).label).toBe(
      'removed label stale',
    )
  })

  test('issue.assigned names the new owner', () => {
    expect(describeKataEvent(makeEvent('issue.assigned', { owner: 'user-a' })).label).toBe(
      'assigned to user-a',
    )
  })

  test('issue.priority_set names the new priority', () => {
    expect(describeKataEvent(makeEvent('issue.priority_set', { priority: 1 })).label).toBe(
      'set priority P1',
    )
  })

  test('issue.metadata_updated lists changed keys from diff', () => {
    const descriptor = describeKataEvent(
      makeEvent('issue.metadata_updated', {
        diff: { due_date: { from: null, to: '2026-06-01' }, sprint: { from: 1, to: 2 } },
      }),
    )
    expect(descriptor.label).toBe('updated due_date, sprint')
  })

  test('issue.metadata_updated falls back when diff is empty', () => {
    expect(describeKataEvent(makeEvent('issue.metadata_updated', { diff: {} })).label).toBe(
      'updated metadata',
    )
  })

  test('issue.links_changed summarises daemon relationship arrays', () => {
    const descriptor = describeKataEvent(
      makeEvent('issue.links_changed', {
        blocks_added: [{ uid: 'issue-late-fee', short_id: 'late' }],
        related_removed: ['foo', 'bar'],
      }),
    )
    expect(descriptor.label).toBe('+blocks · -related (2)')
  })

  test('issue.links_changed handles parent_set and parent_cleared', () => {
    expect(describeKataEvent(makeEvent('issue.links_changed', { parent_set: 42 })).label).toBe(
      '+parent',
    )
    expect(
      describeKataEvent(makeEvent('issue.links_changed', { parent_cleared: true })).label,
    ).toBe('-parent')
  })

  test('issue.links_changed falls back when no relationships changed', () => {
    expect(describeKataEvent(makeEvent('issue.links_changed', {})).label).toBe('changed links')
  })

  test('issue.moved names the destination short id', () => {
    const descriptor = describeKataEvent(
      makeEvent('issue.moved', {
        from_short_id: 'old-1',
        to_short_id: 'new-7',
      }),
    )
    expect(descriptor.label).toBe('moved to new-7')
  })

  test('issue.moved falls back when destination missing', () => {
    expect(describeKataEvent(makeEvent('issue.moved')).label).toBe('moved')
  })

  test('unknown event types fall back to the stripped type as label', () => {
    const descriptor = describeKataEvent(makeEvent('issue.mystery'))
    expect(descriptor.label).toBe('mystery')
    expect(descriptor.tone).toBe('neutral')
    expect(descriptor.icon).toBeDefined()
  })

  test('legacy daemon event names stay visible as unknown event labels', () => {
    for (const legacy of [
      'issue.label_added',
      'issue.label_removed',
      'issue.owner_assigned',
      'issue.owner_unassigned',
      'issue.title_changed',
      'issue.body_changed',
      'issue.metadata_changed',
      'issue.comment_added',
    ]) {
      const descriptor = describeKataEvent(makeEvent(legacy))
      expect(descriptor.label).toBe(legacy.replace(/^issue\./, ''))
    }
  })
})
