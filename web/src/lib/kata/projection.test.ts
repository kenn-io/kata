import { describe, expect, test } from 'vitest'

import type { UISnapshot } from '../state/snapshot'
import { normalizeKataUISnapshot } from './projection'

describe('normalizeKataUISnapshot', () => {
  test('adapts the native snapshot to the ported immutable collection model', () => {
    const projection = normalizeKataUISnapshot(snapshot(), '2026-08-01T12:30:00.000Z')

    expect(projection.projects).toEqual([
      expect.objectContaining({
        uid: 'project-example',
        name: 'example-project',
        open_count: 4,
        metadata: { area: 'Personal' },
      }),
    ])
    expect(projection.issues).toEqual([
      expect.objectContaining({
        uid: 'issue-parent',
        child_counts: { open: 1, total: 1 },
      }),
      expect.objectContaining({
        uid: 'issue-child',
        parent: { uid: 'issue-parent', short_id: 'p1' },
        parent_short_id: 'p1',
      }),
      expect.objectContaining({
        uid: 'issue-blocker',
        blocks: [{ uid: 'issue-blocked', short_id: 'b2' }],
      }),
      expect.objectContaining({
        uid: 'issue-blocked',
        blocked_by: [{ uid: 'issue-blocker', short_id: 'b1' }],
      }),
    ])
    expect(projection.member_issue_uid_set.has('issue-child')).toBe(true)
    expect(projection.selected_state).toBe('available')
    expect(projection.selected_revision).toBe('"rev-3"')
    expect(projection.selected_recurrences).toEqual([
      expect.objectContaining({ uid: 'recurrence-1', template_title: 'Weekly example' }),
    ])
    expect(projection.selected_history).toEqual([
      expect.objectContaining({ event_uid: 'event-1', type: 'issue.commented' }),
    ])
    expect(projection.selected_graph).toEqual(
      expect.objectContaining({
        source_uid: 'issue-child',
        nodes: expect.arrayContaining([
          expect.objectContaining({ uid: 'issue-child', status: 'open' }),
          expect.objectContaining({ uid: 'issue-closed', status: 'closed' }),
        ]),
        edges: expect.arrayContaining([
          {
            from_uid: 'issue-child',
            to_uid: 'issue-closed',
            kind: 'blocks',
            layout: true,
          },
          {
            from_uid: 'issue-child',
            to_uid: 'issue-missing',
            kind: 'blocks',
            layout: true,
          },
        ]),
        unresolved_refs: [
          {
            uid: 'issue-missing',
            side: 'to',
            kind: 'blocks',
            other_uid: 'issue-child',
          },
        ],
      }),
    )
    expect(projection.selected_detail).toEqual(
      expect.objectContaining({
        issue: expect.objectContaining({ uid: 'issue-child', body: 'Selected body' }),
        comments: [expect.objectContaining({ author: 'user-a', body: 'Selected comment' })],
        labels: [expect.objectContaining({ label: 'review' })],
        links: [
          expect.objectContaining({
            type: 'parent',
            from: { uid: 'issue-child', short_id: 'c1' },
            to: { uid: 'issue-parent', short_id: 'p1' },
          }),
        ],
      }),
    )
    expect(projection.fetched_at).toBe('2026-08-01T12:30:00.000Z')
    expect(Object.isFrozen(projection)).toBe(true)
    expect(Object.isFrozen(projection.issues)).toBe(true)
  })

  test('rejects issue states outside the ported open and closed contract', () => {
    const invalid = snapshot()
    invalid.collection![0]!.status = 'unknown'

    expect(() => normalizeKataUISnapshot(invalid)).toThrow(
      'Invalid Kata snapshot collection issue status',
    )
  })
})

function snapshot(): UISnapshot {
  return {
    contract_version: '1',
    cursor: 12,
    capabilities: { writable: true, updates: 'sse', actor_policy: 'identity' },
    origin: 'https://daemon.example',
    origin_stable: true,
    catalog: [
      {
        project: {
          id: 7,
          uid: 'project-example',
          name: 'example-project',
          metadata: { area: 'Personal' },
          revision: 2,
          created_at: '2026-08-01T09:00:00.000Z',
        },
        stats: { Open: 4, Closed: 0, LastEventAt: '2026-08-01T12:00:00.000Z' },
      },
    ],
    collection: [
      issue('issue-parent', 'p1', 'Parent issue'),
      issue('issue-child', 'c1', 'Child issue'),
      issue('issue-blocker', 'b1', 'Blocker issue'),
      issue('issue-blocked', 'b2', 'Blocked issue'),
    ],
    collection_links: [
      link('issue-child', 'c1', 'open', 'issue-parent', 'p1', 'open', 'parent'),
      link('issue-blocker', 'b1', 'open', 'issue-blocked', 'b2', 'open', 'blocks'),
    ],
    graph: {
      issues: [
        issue('issue-parent', 'p1', 'Parent issue'),
        issue('issue-child', 'c1', 'Child issue'),
        { ...issue('issue-closed', 'd1', 'Closed issue'), status: 'closed' },
      ],
      links: [
        link('issue-child', 'c1', 'open', 'issue-parent', 'p1', 'open', 'parent'),
        link('issue-child', 'c1', 'open', 'issue-closed', 'd1', 'closed', 'blocks'),
      ],
      edges: [
        {
          from_uid: 'issue-child',
          to_uid: 'issue-missing',
          kind: 'blocks',
          layout: true,
        },
      ],
      unresolved_refs: [
        {
          uid: 'issue-missing',
          side: 'to',
          kind: 'blocks',
          other_uid: 'issue-child',
        },
      ],
    },
    selected: {
      state: 'available',
      issue: {
        ...issue('issue-child', 'c1', 'Child issue'),
        body: 'Selected body',
        revision: 3,
      },
      comments: [
        {
          id: 1,
          uid: 'comment-1',
          issue_id: 2,
          author: 'user-a',
          body: 'Selected comment',
          created_at: '2026-08-01T12:05:00.000Z',
        },
      ],
      labels: [
        {
          issue_id: 2,
          label: 'review',
          author: 'user-a',
          created_at: '2026-08-01T12:00:00.000Z',
        },
      ],
      links: [link('issue-child', 'c1', 'open', 'issue-parent', 'p1', 'open', 'parent')],
      recurrences: [
        {
          id: 1,
          uid: 'recurrence-1',
          project_id: 7,
          rrule: 'FREQ=WEEKLY;BYDAY=FR',
          dtstart: '2026-08-01',
          timezone: 'UTC',
          template_title: 'Weekly example',
          template_body: '',
          template_labels: [],
          template_metadata: {},
          author: 'user-a',
          revision: 1,
          created_at: '2026-08-01T12:00:00.000Z',
          updated_at: '2026-08-01T12:00:00.000Z',
        },
      ],
      history: [
        {
          id: 1,
          uid: 'event-1',
          origin_instance_uid: 'instance-example',
          content_hash: 'hash-example',
          hlc_counter: 0,
          hlc_physical_ms: 1,
          type: 'issue.commented',
          project_id: 7,
          project_uid: 'project-example',
          project_name: 'example-project',
          actor: 'user-a',
          payload: '{}',
          created_at: '2026-08-01T12:05:00.000Z',
        },
      ],
    },
  } as UISnapshot
}

function issue(
  uid: string,
  shortID: string,
  title: string,
): NonNullable<UISnapshot['collection']>[number] {
  return {
    id: Number.parseInt(shortID.slice(1), 10),
    uid,
    project_id: 7,
    project_uid: 'project-example',
    project_name: 'example-project',
    short_id: shortID,
    qualified_id: `example-project#${shortID}`,
    title,
    body: '',
    status: 'open',
    metadata: {},
    revision: 1,
    author: 'user-a',
    labels: [],
    created_at: '2026-08-01T09:00:00.000Z',
    updated_at: '2026-08-01T12:00:00.000Z',
  }
}

function link(
  fromUID: string,
  fromShortID: string,
  fromStatus: string,
  toUID: string,
  toShortID: string,
  toStatus: string,
  type: string,
): NonNullable<UISnapshot['collection_links']>[number] {
  return {
    id: 1,
    from_issue_id: 1,
    from_issue_uid: fromUID,
    from_qualified_id: `example-project#${fromShortID}`,
    from_status: fromStatus,
    to_issue_id: 2,
    to_issue_uid: toUID,
    to_qualified_id: `example-project#${toShortID}`,
    to_status: toStatus,
    type,
    author: 'user-a',
    created_at: '2026-08-01T12:00:00.000Z',
  } as NonNullable<UISnapshot['collection_links']>[number]
}
