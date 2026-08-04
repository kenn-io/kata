import { describe, expect, it } from 'vitest'
import type { KataTaskLink, KataTaskSummary } from './types'
import {
  applyKataLinkStatusScope,
  createKataLinkFilters,
  kataLinkCouldAffectVisibleResults,
  kataLinkMatchesFilters,
  relationForKataLink,
} from './linkFilters'

const selectedUID = 'issue-selected'

function link(overrides: Partial<KataTaskLink> = {}): KataTaskLink {
  return {
    id: 1,
    project_id: 1,
    from: { uid: selectedUID, short_id: 'selected' },
    to: { uid: 'issue-peer', short_id: 'peer' },
    type: 'related',
    author: 'user-a',
    created_at: '2026-07-22T12:00:00Z',
    ...overrides,
  }
}

function peer(status: KataTaskSummary['status']): KataTaskSummary {
  return {
    id: 2,
    uid: 'issue-peer',
    project_id: 1,
    project_uid: 'project-1',
    project_name: 'Inbox',
    short_id: 'peer',
    qualified_id: 'Inbox#peer',
    title: 'Peer task',
    status,
    metadata: {},
    revision: 1,
    author: 'maintainer',
    created_at: '2026-07-22T12:00:00Z',
    updated_at: '2026-07-22T12:00:00Z',
  }
}

describe('kata link filters', () => {
  it.each([
    ['open', { open: true, closed: false }],
    ['closed', { open: false, closed: true }],
    ['all', { open: true, closed: true }],
  ] as const)('defaults task states from the %s scope', (scope, statuses) => {
    expect(createKataLinkFilters(scope).statuses).toEqual(statuses)
  })

  it('classifies relationship direction from the selected task', () => {
    expect(relationForKataLink(link({ type: 'parent' }), selectedUID)).toBe('parent')
    expect(
      relationForKataLink(
        link({
          type: 'parent',
          from: { uid: 'issue-parent', short_id: 'parent' },
          to: { uid: selectedUID, short_id: 'selected' },
        }),
        selectedUID,
      ),
    ).toBe('child')
    expect(relationForKataLink(link({ type: 'blocks' }), selectedUID)).toBe('blocks')
    expect(
      relationForKataLink(
        link({
          type: 'blocks',
          from: { uid: 'issue-blocker', short_id: 'blocker' },
          to: { uid: selectedUID, short_id: 'selected' },
        }),
        selectedUID,
      ),
    ).toBe('blocked_by')
  })

  it('resets task states without changing relationship choices', () => {
    const current = createKataLinkFilters('all')
    current.relations.related = false

    expect(applyKataLinkStatusScope(current, 'closed')).toEqual({
      statuses: { open: false, closed: true },
      relations: { ...current.relations, related: false },
    })
  })

  it('matches resolved, pending, and failed peers without silently hiding failures', () => {
    const openOnly = createKataLinkFilters('open')
    const mixed = createKataLinkFilters('all')

    expect(
      kataLinkMatchesFilters(
        link(),
        selectedUID,
        { kind: 'resolved', peer: peer('open') },
        openOnly,
      ),
    ).toBe(true)
    expect(
      kataLinkMatchesFilters(
        link(),
        selectedUID,
        { kind: 'resolved', peer: peer('closed') },
        openOnly,
      ),
    ).toBe(false)
    expect(kataLinkMatchesFilters(link(), selectedUID, { kind: 'pending' }, openOnly)).toBe(false)
    expect(kataLinkMatchesFilters(link(), selectedUID, { kind: 'pending' }, mixed)).toBe(true)
    expect(kataLinkMatchesFilters(link(), selectedUID, { kind: 'failed' }, openOnly)).toBe(true)
  })

  it('ignores pending work that disabled filters cannot reveal', () => {
    const filters = createKataLinkFilters('open')
    filters.relations.related = false
    expect(kataLinkCouldAffectVisibleResults(link(), selectedUID, filters)).toBe(false)

    filters.relations.related = true
    filters.statuses.open = false
    expect(kataLinkCouldAffectVisibleResults(link(), selectedUID, filters)).toBe(false)
  })
})
