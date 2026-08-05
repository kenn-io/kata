export type KataTaskViewName = 'inbox' | 'today' | 'upcoming' | 'deadlines' | 'all' | 'logbook'

export interface KataTaskChecklistItem {
  id: string
  text: string
  done: boolean
}

export interface KataTaskMetadata {
  scheduled_on?: string | undefined
  deadline_on?: string | undefined
  today_bucket?: 'day' | 'evening' | undefined
  checklist?: KataTaskChecklistItem[] | undefined
  area?: string | undefined
  [key: string]: unknown
}

export interface KataProjectMetadata {
  area?: string | undefined
  sidebar_order?: number | undefined
  icon?: string | undefined
  timezone?: string | undefined
  role?: string | undefined
  [key: string]: unknown
}

export interface KataProjectSummary {
  id: number
  uid: string
  name: string
  metadata: KataProjectMetadata
  revision?: number | undefined
  created_at?: string | undefined
  deleted_at?: string | undefined
  open_count: number
}

export interface KataLinkPeer {
  uid: string
  short_id: string
}

export interface KataTaskLinkPeer extends KataLinkPeer {
  qualified_id: string
  status: 'open' | 'closed'
}

export interface KataTaskSummary {
  id: number
  uid: string
  project_id: number
  short_id: string
  qualified_id: string
  title: string
  body?: string | undefined
  status: 'open' | 'closed'
  project_uid: string
  project_name: string
  metadata: KataTaskMetadata
  revision: number
  owner?: string | undefined
  author: string
  priority?: number | undefined
  labels?: string[] | undefined
  parent?: KataLinkPeer | undefined
  parent_short_id?: string | undefined
  blocks?: KataLinkPeer[] | undefined
  blocked_by?: KataLinkPeer[] | undefined
  related?: KataLinkPeer[] | undefined
  child_counts?: { open: number; total: number } | undefined
  recurrence_id?: number | undefined
  occurrence_key?: string | undefined
  created_at: string
  updated_at: string
  closed_reason?: 'done' | 'wontfix' | 'duplicate' | 'superseded' | 'audit-no-change' | undefined
  closed_at?: string | undefined
  deleted_at?: string | undefined
}

export interface KataTaskGroup {
  id: string
  title: string
  issues: KataTaskSummary[]
}

export interface KataTaskViewResponse {
  view: KataTaskViewName
  groups: KataTaskGroup[]
  fetched_at: string
}

export type KataTaskStatusFilter = 'open' | 'ready' | 'closed' | 'all'
export type KataTaskSearchScope = { kind: 'all' } | { kind: 'project'; project_uid: string }

export interface KataTaskSearchFilters {
  scope: KataTaskSearchScope
  status: KataTaskStatusFilter
  owner: string
  label: string
  query: string
  relationships?: string[] | undefined
}

export type KataReachableGraphDepth = 'full' | '1' | '2' | '3'
export type KataReachableGraphEdgeKind = 'parent' | 'blocks' | 'related'

export interface KataReachableGraphEdge {
  from_uid: string
  to_uid: string
  kind: KataReachableGraphEdgeKind
  layout: boolean
}

export interface KataReachableGraphUnresolvedRef {
  uid: string
  side: 'from' | 'to'
  kind: KataReachableGraphEdgeKind
  other_uid: string
}

export interface KataReachableGraphResponse {
  source_uid: string
  depth: KataReachableGraphDepth
  hide_done: boolean
  nodes: KataTaskSummary[]
  edges: KataReachableGraphEdge[]
  unresolved_refs: KataReachableGraphUnresolvedRef[]
  fetched_at: string
}

export interface KataComment {
  id: number
  issue_id: number
  author: string
  body: string
  created_at: string
}

export interface KataTaskLabel {
  issue_id: number
  label: string
  author: string
  created_at: string
}

export interface KataTaskLink {
  id: number
  project_id: number
  from: KataTaskLinkPeer
  to: KataTaskLinkPeer
  type: 'parent' | 'blocks' | 'related'
  author: string
  created_at: string
}

export interface KataTaskRef {
  uid: string
  short_id: string
  qualified_id: string
  title: string
  status: 'open' | 'closed'
}

export interface KataTaskDetail {
  issue: KataTaskSummary & {
    body: string
  }
  etag?: string | undefined
  comments: KataComment[]
  labels: KataTaskLabel[]
  links: KataTaskLink[]
  parent?: KataTaskRef | undefined
  children?: KataTaskSummary[] | undefined
  // Accepted snapshot enrichment carries the workspace action atomically
  // with selected task detail.
  workspace_target?: KataWorkspaceTarget | undefined
}

export type KataWorkspaceTarget = Record<string, unknown>

export interface KataTaskEvent {
  event_id: number
  event_uid: string
  origin_instance_uid: string
  type: string
  project_id: number
  project_uid: string
  project_name: string
  issue_id?: number | undefined
  issue_uid?: string | undefined
  issue_short_id?: string | undefined
  related_issue_id?: number | undefined
  related_issue_uid?: string | undefined
  related_issue_short_id?: string | undefined
  actor: string
  payload?: Record<string, unknown> | undefined
  created_at: string
}

export interface KataRecurrence {
  id: number
  uid: string
  project_id: number
  rrule: string
  dtstart: string
  timezone: string
  template_title: string
  template_body: string
  template_owner?: string | undefined
  template_priority?: number | undefined
  template_labels: string[]
  template_metadata: KataTaskMetadata
  next_occurrence_key?: string | undefined
  last_materialized_uid?: string | undefined
  author: string
  revision: number
  created_at: string
  updated_at: string
  deleted_at?: string | undefined
}

export interface KataRecurrencesResponse {
  recurrences: KataRecurrence[]
  fetched_at: string
}

export interface KataRecurrenceTemplateInput {
  title: string
  body?: string | undefined
  owner?: string | undefined
  priority?: number | undefined
  labels?: string[] | undefined
  metadata?: KataTaskMetadata | undefined
}

export interface KataCreateRecurrenceInput {
  actor: string
  initialIssueRef: string
  rrule: string
  dtstart: string
  timezone: string
  template: KataRecurrenceTemplateInput
}

export interface KataRecurrenceTemplateUpdateInput {
  title?: string | undefined
  body?: string | undefined
  owner?: string | undefined
  clearOwner?: boolean | undefined
  priority?: number | undefined
  clearPriority?: boolean | undefined
  labels?: string[] | undefined
  metadata?: KataTaskMetadata | undefined
}

export interface KataPatchRecurrenceInput {
  actor: string
  rrule?: string | undefined
  dtstart?: string | undefined
  timezone?: string | undefined
  template?: KataRecurrenceTemplateUpdateInput | undefined
}

export interface KataRecurrenceResponse {
  recurrence: KataRecurrence
  etag?: string | undefined
  changed?: boolean | undefined
}

export interface KataTaskMutationTarget {
  project_id: number
  ref: string
}

export interface KataTaskCreateDraft {
  title: string
  body?: string | undefined
  owner?: string | undefined
  priority?: number | undefined
  labels?: string[] | undefined
  metadata?: KataTaskMetadata | undefined
  force_new?: boolean | undefined
}

export interface KataTaskEditPatch {
  title?: string | undefined
  body?: string | undefined
  links_delta?: KataTaskLinkDelta | undefined
}

export interface KataTaskLinkDelta {
  add_related?: string[] | undefined
  add_blocks?: string[] | undefined
  add_blocked_by?: string[] | undefined
  set_parent?: string | null | undefined
  remove?: string[] | undefined
}

export type KataTaskMetadataPatch = Record<string, unknown>

export interface KataTaskCloseOptions {
  reason?: 'done' | 'wontfix' | 'duplicate' | 'superseded' | 'audit-no-change' | undefined
  message?: string | undefined
  source?: string | undefined
  evidence?: import('../api/schema').components['schemas']['Evidence'][] | undefined
}

export interface KataTaskCloseRequest {
  reason: 'done' | 'wontfix' | 'duplicate' | 'superseded'
  message: string
  evidence: import('../api/schema').components['schemas']['Evidence'][]
}

export interface KataTaskMutationResponse {
  changed: boolean
}

export interface KataPinnedDaemonOptions {
  daemonId: string
}

export interface KataPinnedDaemonRequestOptions extends KataPinnedDaemonOptions {
  signal?: AbortSignal | undefined
}

export interface KataTaskAPI {
  createProject(name: string, options: KataPinnedDaemonOptions): Promise<KataTaskMutationResponse>
  createIssue(
    projectID: number,
    actor: string,
    draft: KataTaskCreateDraft,
    options: KataPinnedDaemonOptions,
    idempotencyKey?: string | undefined,
  ): Promise<KataTaskMutationResponse>
  addComment(
    target: KataTaskMutationTarget,
    actor: string,
    body: string,
    options: KataPinnedDaemonOptions,
  ): Promise<KataTaskMutationResponse>
  addLabel(
    target: KataTaskMutationTarget,
    actor: string,
    label: string,
    options: KataPinnedDaemonOptions,
  ): Promise<KataTaskMutationResponse>
  removeLabel(
    target: KataTaskMutationTarget,
    actor: string,
    label: string,
    options: KataPinnedDaemonOptions,
  ): Promise<KataTaskMutationResponse>
  assignOwner(
    target: KataTaskMutationTarget,
    actor: string,
    owner: string,
    options: KataPinnedDaemonOptions,
  ): Promise<KataTaskMutationResponse>
  unassignOwner(
    target: KataTaskMutationTarget,
    actor: string,
    options: KataPinnedDaemonOptions,
  ): Promise<KataTaskMutationResponse>
  setPriority(
    target: KataTaskMutationTarget,
    actor: string,
    priority: number | null,
    options: KataPinnedDaemonOptions,
  ): Promise<KataTaskMutationResponse>
  closeIssue(
    target: KataTaskMutationTarget,
    actor: string,
    close: KataTaskCloseOptions,
    options: KataPinnedDaemonOptions,
  ): Promise<KataTaskMutationResponse>
  reopenIssue(
    target: KataTaskMutationTarget,
    actor: string,
    options: KataPinnedDaemonOptions,
  ): Promise<KataTaskMutationResponse>
  editIssue(
    target: KataTaskMutationTarget,
    actor: string,
    patch: KataTaskEditPatch,
    options: KataPinnedDaemonOptions,
  ): Promise<KataTaskMutationResponse>
  patchIssueMetadata(
    target: KataTaskMutationTarget,
    actor: string,
    patch: KataTaskMetadataPatch,
    ifMatch: string,
    options: KataPinnedDaemonOptions,
  ): Promise<KataTaskMutationResponse>
  moveIssue(
    target: KataTaskMutationTarget,
    actor: string,
    toProjectUID: string,
    ifMatch: string,
    options: KataPinnedDaemonOptions,
  ): Promise<KataTaskMutationResponse>
  recurrences(
    projectID: number,
    options: KataPinnedDaemonRequestOptions,
  ): Promise<KataRecurrencesResponse>
  createRecurrence(
    projectID: number,
    input: KataCreateRecurrenceInput,
    options: KataPinnedDaemonOptions,
  ): Promise<KataRecurrenceResponse>
  showRecurrence(
    projectID: number,
    recurrenceUID: string,
    options: KataPinnedDaemonOptions,
  ): Promise<KataRecurrenceResponse>
  patchRecurrence(
    projectID: number,
    recurrenceUID: string,
    patch: KataPatchRecurrenceInput,
    ifMatch: string,
    options: KataPinnedDaemonOptions,
  ): Promise<KataRecurrenceResponse>
  deleteRecurrence(
    projectID: number,
    recurrenceUID: string,
    actor: string,
    options: KataPinnedDaemonOptions,
    ifMatch?: string,
  ): Promise<void>
}
