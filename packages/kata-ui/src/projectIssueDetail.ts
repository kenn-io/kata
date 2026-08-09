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
  const projected: KataIssueDetailModel = {
    issue: {
      uid: issue.uid,
      projectUID: issue.project_uid,
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
    links: (wire.links ?? []).map((link) => {
      const peer = link.from.uid === issue.uid ? link.to : link.from
      return {
        id: String(link.id),
        relation: relationFor(link, issue.uid),
        peerUID: peer.uid,
        peerReference: referenceFor(peer),
        peerStatus: peer.status ?? '',
      }
    }),
    children: (wire.children ?? []).map(projectReference),
    pendingClaims: (wire.pending_claims ?? []).map((claim) => ({
      holder: claim.holder,
      kind: claim.claim_kind ?? '',
      purpose: claim.purpose ?? '',
    })),
  }

  if (wire.parent !== undefined && wire.parent !== null) {
    projected.parent = projectReference(wire.parent)
  }
  if (wire.claim !== undefined && wire.claim !== null) {
    projected.claim = {
      holder: wire.claim.holder,
      kind: wire.claim.claim_kind ?? '',
      purpose: wire.claim.purpose ?? '',
    }
  }

  return projected
}
