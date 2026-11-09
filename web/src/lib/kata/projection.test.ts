import { describe, expect, test } from 'vitest'

import type { UISnapshot } from '../state/snapshot'
import { normalizeKataUISnapshot } from './projection'

describe('normalizeKataUISnapshot', () => {
  test.each([
    ['blocks hub', 4000, 16000],
    ['hub with back-link', 1000, 8000],
    ['related hub', 4000, 16000],
    ['hub fan-in/out', 2000, 8000],
    ['layered10', 2000, 8000],
    ['skip4', 2000, 8000],
  ] as const)(
    '%s normalization keeps array traversal below quadratic growth',
    (shape, smallSize, largeSize) => {
      const traversals = [smallSize, largeSize].map((size) => {
        const wire = snapshot()
        const issues = Array.from({ length: size }, (_, index) =>
          issue(`issue-${index}`, `t${index}`, `Task ${index}`),
        )
        const links: NonNullable<UISnapshot['collection_links']> = []
        const connect = (from: number, to: number) => {
          links.push(
            link(
              `issue-${from}`,
              `t${from}`,
              'open',
              `issue-${to}`,
              `t${to}`,
              'open',
              shape.startsWith('related') ? 'related' : 'blocks',
            ),
          )
        }
        for (let index = 1; index < size; index++) {
          if (shape.endsWith('hub') || shape === 'hub with back-link') connect(0, index)
          else if (shape === 'hub fan-in/out') {
            if (index < size / 2) connect(index, 0)
            else connect(0, index)
          } else if (shape === 'layered10') {
            const previousLayer = (Math.floor(index / 10) - 1) * 10
            if (previousLayer >= 0) {
              for (let peer = previousLayer; peer < previousLayer + 10; peer++) connect(peer, index)
            }
          } else {
            for (let skip = 1; skip <= 4 && skip <= index; skip++) connect(index - skip, index)
          }
        }
        if (shape === 'hub with back-link') connect(1, 0)
        wire.selected!.issue = issues[0]!
        wire.graph = { issues, links, edges: [], unresolved_refs: [] }
        let visits = 0
        const iterate = Array.prototype[Symbol.iterator]
        Array.prototype[Symbol.iterator] = function* (this: unknown[]) {
          const iterator = iterate.call(this)
          for (let item = iterator.next(); !item.done; item = iterator.next()) {
            visits++
            yield item.value
          }
          return undefined
        }
        let graph
        try {
          graph = normalizeKataUISnapshot(wire).selected_graph!
        } finally {
          Array.prototype[Symbol.iterator] = iterate
        }
        expect(graph.nodes).toHaveLength(size)
        expect(graph.edges.map((edge) => edge.layout)).toEqual(
          links.map((edge) =>
            shape === 'skip4'
              ? Number(edge.to_issue_uid.slice(6)) - Number(edge.from_issue_uid.slice(6)) === 1
              : true,
          ),
        )
        return visits
      })
      const [small, large] = traversals as [number, number]
      // Count real array iteration, including spreads and per-edge graph walks,
      // so runner scheduling and garbage collection cannot change the verdict.
      expect(large).toBeLessThan(small * (largeSize / smallSize) ** 1.5)
    },
    60_000,
  )

  test('matches the alternate-path rule on all four-task graphs and seeded cyclic graphs', () => {
    const check = (uids: string[], pairs: string[]) => {
      const wire = snapshot()
      const issues = uids.map((uid, index) => issue(uid, `t${index}`, uid))
      wire.selected!.issue = issues[0]!
      wire.graph = {
        issues,
        links: [
          ...pairs.map((pair) =>
            link(pair[0]!, pair[0]!, 'open', pair[1]!, pair[1]!, 'open', 'blocks'),
          ),
          // Keep disconnected blocking components visible without adding blocking paths.
          ...uids.slice(1).map((uid) => link(uids[0]!, 't0', 'open', uid, uid, 'open', 'related')),
        ],
        edges: [],
        unresolved_refs: [],
      }
      const expected = pairs.map((pair) => {
        const source = pair[0]!
        const target = pair[1]!
        const seen = new Set([source])
        const queue = pairs.filter((edge) => edge[0] === source).map((edge) => edge[1]!)
        while (queue.length > 0) {
          const next = queue.shift()!
          if (next === target || seen.has(next)) continue
          seen.add(next)
          for (const edge of pairs) {
            if (edge[0] !== next) continue
            if (edge[1] === target) return false
            if (!seen.has(edge[1]!)) queue.push(edge[1]!)
          }
        }
        return true
      })
      const actual = normalizeKataUISnapshot(wire).selected_graph!.edges.filter(
        (edge) => edge.kind === 'blocks',
      )
      expect(
        actual.map((edge) => edge.layout),
        pairs.join(','),
      ).toEqual(expected)
    }
    const slots = [...'abcd'].flatMap((from) =>
      [...'abcd'].filter((to) => to !== from).map((to) => from + to),
    )
    for (let mask = 0; mask < 4096; mask++) {
      check(
        [...'abcd'],
        slots.filter((_, index) => (mask & (1 << index)) !== 0),
      )
    }
    let seed = 12345
    const random = () => (seed = (seed * 1664525 + 1013904223) >>> 0) / 2 ** 32
    for (let trial = 0; trial < 500; trial++) {
      // The first trial is a DAG with 70 join targets, spanning three bitset words.
      const uids =
        trial === 0
          ? Array.from({ length: 72 }, (_, index) => String.fromCharCode(0x100 + index))
          : [...'abcdefgh']
      const pairs: string[] = []
      for (const [fromIndex, from] of uids.entries()) {
        for (const [toIndex, to] of uids.entries()) {
          if (from === to || (trial === 0 && fromIndex >= toIndex)) continue
          const distance = toIndex - fromIndex
          if (!(trial === 0 && distance <= 2) && random() >= 0.2) continue
          pairs.push(from + to)
        }
      }
      for (let index = pairs.length - 1; index > 0; index--) {
        const other = Math.floor(random() * (index + 1))
        ;[pairs[index], pairs[other]] = [pairs[other]!, pairs[index]!]
      }
      check(uids, pairs)
    }
  })

  test.each(['verified', 'pending', 'legacy'] as const)(
    'retains %s creation projections in accepted snapshots',
    (verification) => {
      const wire = snapshot()
      const attribution = {
        verification,
        accountable_actor: 'member-one',
        source_actor: 'source-agent',
        teammate: 'example-worker',
      }
      Object.assign(wire.collection![0]!, attribution)
      Object.assign(wire.selected!.issue!, attribution)
      Object.assign(wire.selected!.comments![0]!, attribution)
      const projection = normalizeKataUISnapshot(wire, '2026-08-01T12:30:00.000Z')
      expect(projection.issues[0]).toMatchObject(attribution)
      expect(projection.selected_detail?.issue).toMatchObject(attribution)
      expect(projection.selected_detail?.comments[0]).toMatchObject(attribution)
    },
  )

  test('selects only open blockers for the collection column', () => {
    const wire = snapshot()
    wire.collection_links = [
      link('issue-blocker', 'b1', 'open', 'issue-blocked', 'b2', 'open', 'blocks'),
      link('issue-done', 'd2', 'closed', 'issue-blocked', 'b2', 'open', 'blocks'),
      link('issue-later', 'l3', 'open', 'issue-blocked', 'b2', 'open', 'blocks'),
    ]
    const projection = normalizeKataUISnapshot(wire, '2026-08-01T12:30:00.000Z')
    const blocked = projection.issues.find((issue) => issue.uid === 'issue-blocked')!

    expect(blocked.open_blocked_by).toEqual([
      { uid: 'issue-blocker', short_id: 'b1', qualified_id: 'example-project#b1' },
      { uid: 'issue-later', short_id: 'l3', qualified_id: 'example-project#l3' },
    ])
    expect(blocked.blocked_by).toHaveLength(3)
  })

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
            from: {
              uid: 'issue-child',
              short_id: 'c1',
              qualified_id: 'example-project#c1',
              status: 'open',
            },
            to: {
              uid: 'issue-parent',
              short_id: 'p1',
              qualified_id: 'example-project#p1',
              status: 'open',
            },
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

  test('preserves the server-projected browser date for timed deadlines', () => {
    const source = snapshot()
    source.collection![0]!.metadata = { deadline_on: '2026-09-01T00:30:00Z' }
    source.collection![0]!.deadline_on_date = '2026-08-31'

    const projection = normalizeKataUISnapshot(source)

    expect(projection.issues[0]).toEqual(
      expect.objectContaining({
        deadline_on_date: '2026-08-31',
        metadata: { deadline_on: '2026-09-01T00:30:00Z' },
      }),
    )
  })

  test('preserves selected-link endpoint authority outside the filtered collection', () => {
    const authority = snapshot()
    authority.collection = authority.collection!.filter((issue) => issue.uid !== 'issue-parent')
    authority.selected!.links = [
      {
        ...link('issue-child', 'c1', 'open', 'issue-parent', 'p1', 'closed', 'parent'),
        to_qualified_id: 'example-workspace#p1',
      },
    ]

    const projection = normalizeKataUISnapshot(authority)

    expect(projection.selected_detail?.links[0]?.to).toEqual({
      uid: 'issue-parent',
      short_id: 'p1',
      qualified_id: 'example-workspace#p1',
      status: 'closed',
    })
  })
})

function snapshot(): UISnapshot {
  return {
    contract_version: '2',
    cursor: 12,
    capabilities: { writable: true, updates: 'sse', actor_policy: 'identity' },
    origin: 'https://daemon.example',
    origin_stable: true,
    catalog: [
      {
        project: {
          active: true,
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

test('snapshot normalization preserves comment identities, links, and edit state', () => {
  const captured = snapshot()
  captured.selected!.comments = [
    {
      id: 1,
      issue_id: 2,
      uid: 'source-comment',
      author: 'worker',
      body: 'Answer',
      created_at: '2030-01-01T00:00:00Z',
      handle: 'c:abc123',
      reply: {
        kind: 'reply',
        uid: 'target-comment',
        issue_uid: 'other-issue',
        handle: 'abcd:def456',
      },
      backlinks: [],
      edited_at: '2030-01-02T00:00:00Z',
    },
  ]
  const normalized = normalizeKataUISnapshot(captured)
  expect(normalized.selected_detail?.comments[0]).toMatchObject({
    uid: 'source-comment',
    handle: 'c:abc123',
    reply: { uid: 'target-comment' },
    edited_at: '2030-01-02T00:00:00Z',
  })
})
