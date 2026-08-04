import type { Component } from 'svelte'
import ArchiveRestoreIcon from '@lucide/svelte/icons/archive-restore'
import ArrowRightLeftIcon from '@lucide/svelte/icons/arrow-right-left'
import CheckCircleIcon from '@lucide/svelte/icons/check-circle-2'
import CircleIcon from '@lucide/svelte/icons/circle'
import FlagIcon from '@lucide/svelte/icons/flag'
import FolderInputIcon from '@lucide/svelte/icons/folder-input'
import MessageSquareIcon from '@lucide/svelte/icons/message-square'
import PencilIcon from '@lucide/svelte/icons/pencil'
import PlusIcon from '@lucide/svelte/icons/plus'
import RotateCcwIcon from '@lucide/svelte/icons/rotate-ccw'
import TagIcon from '@lucide/svelte/icons/tag'
import TrashIcon from '@lucide/svelte/icons/trash-2'
import UserIcon from '@lucide/svelte/icons/user-round'

import type { KataTaskEvent } from '../kata/types'

export type KataEventTone = 'neutral' | 'positive' | 'negative' | 'warning'

export interface KataEventDescriptor {
  icon: Component
  label: string
  tone: KataEventTone
}

export function describeKataEvent(event: KataTaskEvent): KataEventDescriptor {
  const payload = event.payload ?? {}
  switch (event.type) {
    case 'issue.created':
      return { icon: PlusIcon, label: 'created the task', tone: 'neutral' }
    case 'issue.closed': {
      const reason = (payload.reason as string | undefined) ?? 'done'
      return { icon: CheckCircleIcon, label: `closed (${reason})`, tone: 'positive' }
    }
    case 'issue.reopened':
      return { icon: RotateCcwIcon, label: 'reopened', tone: 'warning' }
    case 'issue.commented':
      return { icon: MessageSquareIcon, label: 'commented', tone: 'neutral' }
    case 'issue.labeled':
      return { icon: TagIcon, label: `added label ${formatValue(payload.label)}`, tone: 'neutral' }
    case 'issue.unlabeled':
      return {
        icon: TagIcon,
        label: `removed label ${formatValue(payload.label)}`,
        tone: 'neutral',
      }
    case 'issue.assigned':
      return { icon: UserIcon, label: `assigned to ${formatValue(payload.owner)}`, tone: 'neutral' }
    case 'issue.unassigned':
      return { icon: UserIcon, label: 'unassigned', tone: 'neutral' }
    case 'issue.priority_set':
      return {
        icon: FlagIcon,
        label: `set priority P${formatValue(payload.priority)}`,
        tone: 'neutral',
      }
    case 'issue.priority_cleared':
      return { icon: FlagIcon, label: 'cleared priority', tone: 'neutral' }
    case 'issue.metadata_updated': {
      const keys = Object.keys((payload.diff as Record<string, unknown>) ?? {})
      const label = keys.length === 0 ? 'updated metadata' : `updated ${keys.join(', ')}`
      return { icon: PencilIcon, label, tone: 'neutral' }
    }
    case 'issue.updated':
      return { icon: PencilIcon, label: 'updated the task', tone: 'neutral' }
    case 'issue.linked':
      return { icon: ArrowRightLeftIcon, label: 'linked', tone: 'neutral' }
    case 'issue.unlinked':
      return { icon: ArrowRightLeftIcon, label: 'unlinked', tone: 'neutral' }
    case 'issue.links_changed':
      return { icon: ArrowRightLeftIcon, label: summarizeLinksChanged(payload), tone: 'neutral' }
    case 'issue.moved': {
      const to = formatValue(payload.to_short_id)
      const label = to === '-' ? 'moved' : `moved to ${to}`
      return { icon: FolderInputIcon, label, tone: 'neutral' }
    }
    case 'issue.soft_deleted':
      return { icon: TrashIcon, label: 'deleted', tone: 'negative' }
    case 'issue.restored':
      return { icon: ArchiveRestoreIcon, label: 'restored', tone: 'positive' }
    default:
      return { icon: CircleIcon, label: event.type.replace(/^issue\./, ''), tone: 'neutral' }
  }
}

function formatValue(value: unknown): string {
  if (value === null || value === undefined) return '-'
  if (typeof value === 'string') return value
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  return JSON.stringify(value)
}

function summarizeLinksChanged(payload: Record<string, unknown>): string {
  const segments: string[] = []
  for (const [key, value] of Object.entries(payload)) {
    if (Array.isArray(value) && value.length > 0) {
      if (key.endsWith('_added')) {
        const rel = key.slice(0, -'_added'.length)
        segments.push(value.length > 1 ? `+${rel} (${value.length})` : `+${rel}`)
      } else if (key.endsWith('_removed')) {
        const rel = key.slice(0, -'_removed'.length)
        segments.push(value.length > 1 ? `-${rel} (${value.length})` : `-${rel}`)
      }
    } else if (key === 'parent_set' && value !== null && value !== undefined && value !== false) {
      segments.push('+parent')
    } else if (
      key === 'parent_cleared' &&
      value !== null &&
      value !== undefined &&
      value !== false
    ) {
      segments.push('-parent')
    }
  }
  return segments.length ? segments.join(' · ') : 'changed links'
}
