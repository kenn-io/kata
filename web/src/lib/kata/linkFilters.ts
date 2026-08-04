import type { KataTaskLink, KataTaskStatusFilter, KataTaskSummary } from './types'

export const KATA_LINK_RELATIONS = ['parent', 'child', 'blocks', 'blocked_by', 'related'] as const

export type KataLinkRelation = (typeof KATA_LINK_RELATIONS)[number]

export interface KataLinkFilters {
  statuses: Record<KataTaskSummary['status'], boolean>
  relations: Record<KataLinkRelation, boolean>
}

export type KataLinkPeerResolution =
  | { kind: 'pending' }
  | { kind: 'failed' }
  | { kind: 'resolved'; peer: KataTaskSummary }

function statusesForScope(scope: KataTaskStatusFilter): KataLinkFilters['statuses'] {
  return {
    open: scope !== 'closed',
    closed: scope !== 'open',
  }
}

export function createKataLinkFilters(scope: KataTaskStatusFilter): KataLinkFilters {
  return {
    statuses: statusesForScope(scope),
    relations: {
      parent: true,
      child: true,
      blocks: true,
      blocked_by: true,
      related: true,
    },
  }
}

export function applyKataLinkStatusScope(
  current: KataLinkFilters,
  scope: KataTaskStatusFilter,
): KataLinkFilters {
  return {
    statuses: statusesForScope(scope),
    relations: { ...current.relations },
  }
}

export function relationForKataLink(link: KataTaskLink, selectedUID: string): KataLinkRelation {
  if (link.type === 'parent') return link.from.uid === selectedUID ? 'parent' : 'child'
  if (link.type === 'blocks') return link.to.uid === selectedUID ? 'blocked_by' : 'blocks'
  return 'related'
}

export function kataLinkCouldAffectVisibleResults(
  link: KataTaskLink,
  selectedUID: string,
  filters: KataLinkFilters,
): boolean {
  return (
    filters.relations[relationForKataLink(link, selectedUID)] &&
    (filters.statuses.open || filters.statuses.closed)
  )
}

export function kataLinkMatchesFilters(
  link: KataTaskLink,
  selectedUID: string,
  resolution: KataLinkPeerResolution,
  filters: KataLinkFilters,
): boolean {
  if (!kataLinkCouldAffectVisibleResults(link, selectedUID, filters)) return false
  if (resolution.kind === 'failed') return true
  if (resolution.kind === 'pending') return filters.statuses.open && filters.statuses.closed
  return filters.statuses[resolution.peer.status]
}
