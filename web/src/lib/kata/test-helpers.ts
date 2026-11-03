import type { KataTaskSummary } from './types'

export function task(overrides: Partial<KataTaskSummary> = {}): KataTaskSummary {
  const shortID = overrides.short_id ?? 'root'
  return {
    id: overrides.id ?? 1,
    uid: overrides.uid ?? 'issue-root',
    project_id: overrides.project_id ?? 7,
    project_uid: overrides.project_uid ?? 'project-example',
    project_name: overrides.project_name ?? 'example-project',
    short_id: shortID,
    qualified_id: overrides.qualified_id ?? `example-project#${shortID}`,
    title: overrides.title ?? 'Root issue',
    status: overrides.status ?? 'open',
    metadata: overrides.metadata ?? {},
    revision: overrides.revision ?? 1,
    author: overrides.author ?? 'user-a',
    priority: overrides.priority,
    closed_reason: overrides.closed_reason,
    created_at: overrides.created_at ?? '2026-08-01T12:00:00Z',
    updated_at: overrides.updated_at ?? '2026-08-01T12:00:00Z',
  }
}
