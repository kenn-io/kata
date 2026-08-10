import type { KataIssueDetailModel, KataIssueDetailWire } from './types.js'

function referenceFor(issue: {
  uid: string
  qualified_id?: string | undefined
  short_id?: string | undefined
}): string {
  return issue.qualified_id ?? issue.short_id ?? issue.uid
}

function projectReference(issue: {
  uid: string
  qualified_id?: string | undefined
  short_id?: string | undefined
  title?: string | undefined
  status?: string | undefined
}): KataIssueDetailModel['children'][number] {
  return {
    uid: issue.uid,
    reference: referenceFor(issue),
    title: issue.title ?? '',
    status: issue.status ?? '',
  }
}

function relationFor(
  link: NonNullable<KataIssueDetailWire['links']>[number],
  selectedUID: string,
): string {
  if (link.type === 'parent') return link.from.uid === selectedUID ? 'parent' : 'child'
  if (link.type === 'blocks') return link.to.uid === selectedUID ? 'blocked_by' : 'blocks'
  return 'related'
}

export function projectIssueDetail(wire: KataIssueDetailWire): KataIssueDetailModel {
  const { issue } = wire
  const labels = issue.labels ?? wire.labels?.map((label) => label.label) ?? []
  const parent = wire.parent == null ? undefined : projectReference(wire.parent)
  const children = (wire.children ?? []).map(projectReference)
  const representedRelationships = new Set<string>()
  if (parent) representedRelationships.add(`parent:${parent.uid}`)
  for (const child of children) representedRelationships.add(`child:${child.uid}`)
  const links: KataIssueDetailModel['links'] = []
  for (const link of wire.links ?? []) {
    const peer = link.from.uid === issue.uid ? link.to : link.from
    const relation = relationFor(link, issue.uid)
    const relationshipKey = `${relation}:${peer.uid}`
    if (representedRelationships.has(relationshipKey)) continue
    representedRelationships.add(relationshipKey)
    links.push({
      id: String(link.id),
      relation,
      peerUID: peer.uid,
      peerReference: referenceFor(peer),
      peerStatus: peer.status ?? '',
    })
  }
  const projected: KataIssueDetailModel = {
    issue: {
      uid: issue.uid,
      projectUID: issue.project_uid ?? '',
      projectName: issue.project_name ?? '',
      reference: referenceFor(issue),
      title: issue.title,
      body: issue.body ?? '',
      status: issue.status,
      ...(issue.owner === undefined ? {} : { owner: issue.owner }),
      ...(issue.priority === undefined ? {} : { priority: issue.priority }),
      ...(issue.metadata?.scheduled_on === undefined
        ? {}
        : { scheduledOn: issue.metadata.scheduled_on }),
      ...(issue.metadata?.deadline_on === undefined
        ? {}
        : { deadlineOn: issue.metadata.deadline_on }),
      checklist: (issue.metadata?.checklist ?? []).map((item) => ({ ...item })),
      labels: [...labels],
      ...(issue.updated_at === undefined ? {} : { updatedAt: issue.updated_at }),
    },
    comments: (wire.comments ?? []).map((comment) => ({
      id: String(comment.id),
      author: comment.author,
      body: comment.body,
      createdAt: comment.created_at,
    })),
    links,
    children,
    pendingClaims: (wire.pending_claims ?? []).map((claim) => ({
      holder: claim.holder,
      kind: claim.claim_kind ?? '',
      purpose: claim.purpose ?? '',
    })),
  }

  if (parent) projected.parent = parent
  if (wire.claim !== undefined && wire.claim !== null) {
    projected.claim = {
      holder: wire.claim.holder,
      kind: wire.claim.claim_kind ?? '',
      purpose: wire.claim.purpose ?? '',
    }
  }

  return projected
}
