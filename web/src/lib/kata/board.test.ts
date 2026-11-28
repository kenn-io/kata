import { describe, expect, it } from 'vitest'

import {
  askRecipient,
  boardColumnTitles,
  buildBoard,
  cardMarkers,
  columnFor,
  defaultDoneLimit,
  isEpic,
  moveTargets,
  planBoardMove,
} from './board'
import type { KataTaskSummary } from './types'

const now = new Date('2026-11-10T12:00:00Z')

function issue(uid: string, overrides: Partial<KataTaskSummary> = {}): KataTaskSummary {
  return {
    id: 1,
    uid,
    project_id: 7,
    project_uid: 'project-example',
    project_name: 'example-project',
    short_id: uid,
    qualified_id: `example-project#${uid}`,
    title: `Task ${uid}`,
    status: 'open',
    metadata: {},
    revision: 3,
    author: 'agent-a',
    labels: [],
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

describe('columnFor', () => {
  it('puts an unclaimed open issue in Open and a closed one in Done', () => {
    expect(columnFor(issue('a'), { now })).toBe('open')
    expect(columnFor(issue('b', { status: 'closed' }), { now })).toBe('done')
  })

  it('puts an owned open issue in In progress until its claim expires', () => {
    expect(columnFor(issue('a', { owner: 'agent-a' }), { now })).toBe('in-progress')
    expect(
      columnFor(issue('b', { owner: 'agent-a', assignment_expires_on: '2026-11-10T13:00:00Z' }), {
        now,
      }),
    ).toBe('in-progress')
    expect(
      columnFor(issue('c', { owner: 'agent-a', assignment_expires_on: '2026-11-10T11:00:00Z' }), {
        now,
      }),
    ).toBe('open')
  })

  it('puts anything waiting on a human in Needs attention', () => {
    expect(
      columnFor(issue('a', { owner: 'agent-a', metadata: { 'work.attention': 'stuck' } }), { now }),
    ).toBe('attention')
    expect(columnFor(issue('b', { labels: ['needs-review'] }), { now })).toBe('attention')
    expect(columnFor(issue('d', { metadata: { 'work.attention': 'ok' } }), { now })).toBe('open')
  })

  it('leaves work waiting on another issue in its own state, as kata ready does', () => {
    const blocker = [{ uid: 'u-x', short_id: 'x', qualified_id: 'example-project#x' }]
    expect(columnFor(issue('a', { open_blocked_by: blocker }), { now })).toBe('open')
    expect(columnFor(issue('b', { owner: 'agent-a', open_blocked_by: blocker }), { now })).toBe(
      'in-progress',
    )
  })

  it("counts only requests in the viewer's inbox when the viewer has one", () => {
    const asked = issue('a', {
      metadata: { 'notify.Y29vcmRpbmF0b3I': { from: 'agent-a', message: 'Decide' } },
    })
    expect(columnFor(asked, { now })).toBe('attention')
    expect(columnFor(asked, { now, viewer: { actor: 'reviewer', inbox: 'reviewer' } })).toBe('open')
    expect(columnFor(asked, { now, viewer: { actor: 'coordinator', inbox: 'coordinator' } })).toBe(
      'attention',
    )
  })

  it('keeps a closed issue in Done whatever signals it still carries', () => {
    expect(
      columnFor(issue('a', { status: 'closed', metadata: { 'work.attention': 'stuck' } }), { now }),
    ).toBe('done')
  })
})

describe('buildBoard', () => {
  it('always shows the four states in order', () => {
    const board = buildBoard([], { now })
    expect(board.columns.map((column) => [column.id, column.title])).toEqual([
      ['open', 'Open'],
      ['in-progress', 'In progress'],
      ['attention', 'Needs attention'],
      ['done', 'Done'],
    ])
    expect(boardColumnTitles.attention).toBe('Needs attention')
  })

  it('places each issue in the column its state derives', () => {
    const board = buildBoard(
      [
        issue('todo'),
        issue('doing', { owner: 'agent-a' }),
        issue('stuck', { metadata: { 'work.attention': 'stuck' } }),
        issue('shipped', { status: 'closed' }),
      ],
      { now },
    )
    expect(board.columns.map((column) => column.issues.map((i) => i.uid))).toEqual([
      ['todo'],
      ['doing'],
      ['stuck'],
      ['shipped'],
    ])
  })

  it('orders open cards by priority, then comment count, then most recent update', () => {
    const board = buildBoard(
      [
        issue('quiet-new', { priority: 1, comment_count: 0, updated_at: '2026-06-01T00:00:00Z' }),
        issue('busy', { priority: 1, comment_count: 7 }),
        issue('urgent', { priority: 0 }),
        issue('none', {}),
      ],
      { now },
    )
    expect(board.columns[0]!.issues.map((i) => i.uid)).toEqual([
      'urgent',
      'busy',
      'quiet-new',
      'none',
    ])
  })

  it('lists subtasks under their parent within a column', () => {
    const board = buildBoard(
      [
        issue('child', { parent: { uid: 'epic', short_id: 'epic' } }),
        issue('epic', { child_counts: { open: 1, total: 1 } }),
      ],
      { now },
    )
    expect(board.columns[0]!.issues.map((i) => i.uid)).toEqual(['epic', 'child'])
    expect(board.columns[0]!.depths.get('child')).toBe(1)
    expect(isEpic(board.columns[0]!.issues[0]!)).toBe(true)
  })

  it('pages Done locally or reports the server total', () => {
    const closed = Array.from({ length: defaultDoneLimit + 5 }, (_, n) =>
      issue(`c${n}`, {
        status: 'closed',
        closed_at: `2026-01-01T00:${String(n).padStart(2, '0')}:00Z`,
      }),
    )
    expect(buildBoard(closed, { now }).columns[3]!.issues).toHaveLength(defaultDoneLimit)
    const paged = buildBoard(closed, { now, doneTotal: 90 }).columns[3]!
    expect(paged.issues).toHaveLength(defaultDoneLimit + 5)
    expect(paged.total).toBe(90)
  })

  it('pages Done by close time before nesting subtasks under their parent', () => {
    const closed = (uid: string, hour: number, parent?: string) =>
      issue(uid, {
        status: 'closed',
        closed_at: `2026-01-01T${String(hour).padStart(2, '0')}:00:00Z`,
        ...(parent ? { parent: { uid: parent, short_id: parent } } : {}),
      })
    const issues = [
      closed('epic', 10),
      closed('old-1', 1, 'epic'),
      closed('old-2', 2, 'epic'),
      closed('new-1', 9),
      closed('new-2', 8),
    ]

    const done = buildBoard(issues, { now, doneLimit: 3 }).columns[3]!

    expect(done.issues.map((card) => card.uid)).toEqual(['epic', 'new-1', 'new-2'])
    expect(done.total).toBe(5)
  })

  it('shows a pending move in its target column', () => {
    const board = buildBoard([issue('a')], { now, pending: new Map([['a', 'in-progress']]) })
    expect(board.columns[1]!.issues.map((i) => i.uid)).toEqual(['a'])
  })
})

describe('planBoardMove', () => {
  it('claims an unowned card moved to In progress, and releases it moved back to Open', () => {
    expect(planBoardMove(issue('a'), 'in-progress', { now })).toEqual({ claim: true })
    expect(planBoardMove(issue('a', { owner: 'agent-a' }), 'open', { now })).toEqual({
      release: true,
    })
  })

  it('lets a blocked card be claimed, because dependencies only filter the ready queue', () => {
    const blocked = issue('a', {
      open_blocked_by: [{ uid: 'u-x', short_id: 'x', qualified_id: 'example-project#x' }],
    })
    expect(planBoardMove(blocked, 'in-progress', { now })).toEqual({ claim: true })
  })

  it('never moves a card into Needs attention, which only the waiting party can raise', () => {
    expect(planBoardMove(issue('a', { owner: 'agent-a' }), 'attention', { now })).toBeUndefined()
    expect(moveTargets(issue('a'), { now })).toEqual(['in-progress', 'done'])
    expect(moveTargets(issue('b', { labels: ['needs-review'] }), { now })).toEqual([
      'open',
      'in-progress',
      'done',
    ])
  })

  it('clears the signals that asked for a human when a card leaves Needs attention', () => {
    const waiting = issue('a', {
      owner: 'agent-a',
      labels: ['needs-review', 'bug'],
      metadata: {
        'work.attention': 'needs-human',
        'work.attention_msg': 'Pick one',
        'notify.Y29vcmRpbmF0b3I': { from: 'agent-a', message: 'Decide' },
        'notify.cmV2aWV3ZXI': { from: 'agent-a', message: 'latest: reply c:abc123', kind: 'reply' },
        other: 'kept',
      },
    })
    expect(planBoardMove(waiting, 'in-progress', { now })).toEqual({
      clearAttention: {
        metadata: {
          'work.attention': 'ok',
          'work.attention_msg': null,
          'notify.Y29vcmRpbmF0b3I': null,
        },
        removeLabels: ['needs-review'],
      },
    })
    expect(planBoardMove(waiting, 'open', { now })).toEqual({
      clearAttention: {
        metadata: {
          'work.attention': null,
          'work.attention_msg': null,
          'notify.Y29vcmRpbmF0b3I': null,
        },
        removeLabels: ['needs-review'],
      },
      release: true,
    })
  })

  it("clears only the viewer's own requests when the viewer has an inbox", () => {
    const asked = issue('a', {
      labels: ['needs-review'],
      metadata: {
        'notify.Y29vcmRpbmF0b3I': { from: 'agent-a', message: 'Decide' },
        'notify.cmV2aWV3ZXI': { from: 'agent-a', message: 'Check' },
      },
    })
    expect(
      planBoardMove(asked, 'in-progress', {
        now,
        viewer: { actor: 'reviewer', inbox: 'reviewer' },
      }),
    ).toEqual({
      clearAttention: { metadata: { 'notify.cmV2aWV3ZXI': null }, removeLabels: ['needs-review'] },
      claim: true,
    })
  })

  it('closes through the completion form and reopens into the target state', () => {
    expect(planBoardMove(issue('a', { owner: 'agent-a' }), 'done', { now })).toEqual({
      close: true,
    })
    const closed = issue('b', { status: 'closed', owner: 'agent-a' })
    expect(planBoardMove(closed, 'open', { now })).toEqual({ reopen: true, release: true })
    expect(planBoardMove(closed, 'in-progress', { now })).toEqual({ reopen: true })
    expect(planBoardMove(issue('c', { status: 'closed' }), 'in-progress', { now })).toEqual({
      reopen: true,
      claim: true,
    })
  })

  it('clears signals left from before a close when a Done card reopens elsewhere', () => {
    // work.* is meaningless on a closed issue, so it must not resurface on reopen.
    const closed = issue('a', {
      status: 'closed',
      labels: ['needs-review'],
      metadata: {
        'work.attention': 'stuck',
        'work.attention_msg': 'Old blocker',
        'notify.Y29vcmRpbmF0b3I': { from: 'agent-a', message: 'Old request' },
      },
    })
    expect(planBoardMove(closed, 'in-progress', { now })).toEqual({
      reopen: true,
      clearAttention: {
        metadata: {
          'work.attention': 'ok',
          'work.attention_msg': null,
          'notify.Y29vcmRpbmF0b3I': null,
        },
        removeLabels: ['needs-review'],
      },
      claim: true,
    })
    expect(planBoardMove(issue('b', { status: 'closed' }), 'open', { now })).toEqual({
      reopen: true,
    })
  })

  it('does nothing when the card is already in the target column', () => {
    expect(planBoardMove(issue('a'), 'open', { now })).toBeUndefined()
    expect(planBoardMove(issue('b', { status: 'closed' }), 'done', { now })).toBeUndefined()
  })
})

describe('cardMarkers', () => {
  it('names the open issues a card waits on', () => {
    expect(
      cardMarkers(
        issue('a', {
          open_blocked_by: [
            { uid: 'u-x', short_id: 'x', qualified_id: 'example-project#x' },
            { uid: 'u-y', short_id: 'y', qualified_id: 'other#y' },
          ],
        }),
        '2026-11-10',
      ),
    ).toEqual({ waitingOn: ['x', 'other#y'] })
  })

  it('marks work parked by a future schedule or someday, which kata next skips', () => {
    expect(cardMarkers(issue('a', { scheduled_on_date: '2026-11-20' }), '2026-11-10')).toEqual({
      waitingOn: [],
      parked: { kind: 'scheduled', date: '2026-11-20' },
    })
    expect(cardMarkers(issue('b', { scheduled_on_date: '2026-11-10' }), '2026-11-10')).toEqual({
      waitingOn: [],
    })
    expect(cardMarkers(issue('c', { metadata: { someday: true } }), '2026-11-10')).toEqual({
      waitingOn: [],
      parked: { kind: 'someday' },
    })
  })
})

describe('askRecipient', () => {
  it('asks the owner, or the author when nobody owns the issue, like kata reminders', () => {
    expect(askRecipient(issue('a', { owner: 'agent-b' }))).toBe('agent-b')
    expect(askRecipient(issue('b'))).toBe('agent-a')
  })
})
