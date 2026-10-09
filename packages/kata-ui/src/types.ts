export interface KataChecklistItem {
  id: string
  text: string
  done: boolean
}

export interface KataIssueWire {
  uid: string
  project_uid?: string | undefined
  project_name?: string | undefined
  short_id?: string | undefined
  qualified_id?: string | undefined
  title: string
  body?: string | undefined
  status: string
  owner?: string | undefined
  priority?: number | undefined
  metadata?:
    | {
        scheduled_on?: string | undefined
        deadline_on?: string | undefined
        checklist?: KataChecklistItem[] | undefined
      }
    | undefined
  labels?: string[] | null | undefined
  updated_at?: string | undefined
}

export interface KataIssueReferenceWire {
  uid: string
  short_id?: string | undefined
  qualified_id?: string | undefined
  title?: string | undefined
  status?: string | undefined
}

export type KataReplyKind = 'reply' | 'confirm' | 'refute' | 'supersede'
export interface KataCommentLink {
  uid?: string | undefined
  handle?: string | undefined
  kind: string
  author?: string | undefined
  teammate?: string | undefined
  issue_uid?: string | undefined
  issue_short_id?: string | undefined
  project_uid?: string | undefined
  project_id?: number | undefined
  status?: string | undefined
  target_edited?: boolean | undefined
  body?: string | undefined
  created_at?: string | undefined
  edited_at?: string | undefined
}

export interface KataIssueDetailWire {
  issue: KataIssueWire
  comments?:
    | Array<{
        id: number
        uid?: string | undefined
        handle?: string | undefined
        reply?: KataCommentLink | undefined
        backlinks?: KataCommentLink[] | undefined
        backlinks_truncated?: boolean | undefined
        edited_at?: string | undefined
        author: string
        teammate?: string | undefined
        body: string
        created_at: string
      }>
    | null
    | undefined
  labels?: Array<{ label: string }> | null | undefined
  links?:
    | Array<{
        id: number
        from: KataIssueReferenceWire
        to: KataIssueReferenceWire
        type: string
      }>
    | null
    | undefined
  parent?: KataIssueReferenceWire | null | undefined
  children?: KataIssueWire[] | null | undefined
  claim?:
    | {
        holder: string
        claim_kind?: string | undefined
        purpose?: string | undefined
      }
    | null
    | undefined
  pending_claims?:
    | Array<{
        holder: string
        claim_kind?: string | undefined
        purpose?: string | undefined
      }>
    | null
    | undefined
}

export interface KataIssueReferenceModel {
  uid: string
  reference: string
  title: string
  status: string
}

export interface KataIssueDetailModel {
  issue: {
    uid: string
    projectUID: string
    projectName: string
    reference: string
    title: string
    body: string
    status: string
    owner?: string
    priority?: number
    scheduledOn?: string
    deadlineOn?: string
    checklist: KataChecklistItem[]
    labels: string[]
    updatedAt?: string
  }
  comments: Array<{
    id: string
    uid?: string
    handle?: string
    reply?: KataCommentLink
    backlinksPartial?: boolean
    backlinks?: KataCommentLink[]
    editedAt?: string
    author: string
    teammate?: string
    body: string
    createdAt: string
  }>
  links: Array<{
    id: string
    relation: string
    peerUID: string
    peerReference: string
    peerStatus: string
  }>
  parent?: KataIssueReferenceModel
  children: KataIssueReferenceModel[]
  claim?: { holder: string; kind: string; purpose: string }
  pendingClaims: Array<{ holder: string; kind: string; purpose: string }>
}

export interface KataIssueHostAction {
  id: string
  label: string
  disabled?: boolean
  busy?: boolean
  invoke: () => void | Promise<void>
}

export interface KataIssueDetailProps {
  detail: KataIssueDetailModel
  actions?: readonly KataIssueHostAction[]
  /** Host navigation for parent, child, and linked issue references. */
  onOpenIssue?: (uid: string) => void
  onOpenComment?: (
    issueUID: string,
    commentUID: string,
    source?: { issueUID: string; commentUID: string },
  ) => void
  returnComment?: { issueUID: string; commentUID: string } | undefined
  onReplyComment?: (commentUID: string, kind: KataReplyKind) => void
  commentActionsDisabled?: boolean
  selectedCommentUID?: string | undefined
}
