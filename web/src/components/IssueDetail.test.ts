import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte'
import type { ComponentProps } from 'svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { KataProjectSummary, KataTaskDetail } from '../lib/kata/types'

import IssueDetail from './IssueDetail.svelte'

type IssueDetailProps = ComponentProps<typeof IssueDetail>

function makeIssue(overrides: Partial<KataTaskDetail['issue']> = {}): KataTaskDetail {
  return {
    issue: {
      id: 1,
      uid: 'issue-1',
      project_id: 1,
      project_uid: 'project-1',
      project_name: 'Inbox',
      short_id: 'I-1',
      qualified_id: 'INBOX-1',
      title: 'Ship the thing',
      body: 'Initial body',
      status: 'open',
      metadata: { checklist: [{ id: 'item-1', text: 'Send', done: false }] },
      revision: 1,
      author: 'user-a',
      created_at: '2026-06-01T12:00:00Z',
      updated_at: '2026-06-01T12:00:00Z',
      ...overrides,
    },
    comments: [],
    labels: [
      { issue_id: 1, label: 'review', author: 'user-a', created_at: '2026-06-01T12:00:00Z' },
    ],
    links: [],
  }
}

function makeProject(uid: string, name: string, role = ''): KataProjectSummary {
  return {
    id: uid === 'project-1' ? 1 : 2,
    uid,
    name,
    metadata: role ? { role } : {},
    open_count: 1,
    revision: 1,
    created_at: '2026-06-01T12:00:00Z',
  }
}

function renderDetail(props: Partial<IssueDetailProps> = {}) {
  return render(IssueDetail, {
    props: {
      issue: makeIssue(),
      projects: [makeProject('project-1', 'Inbox', 'inbox'), makeProject('project-2', 'Roadmap')],
      ownerOptions: [],
      onMoveIssue: vi.fn(async () => true),
      onPatchMetadata: vi.fn(async () => true),
      onEditIssue: vi.fn(async () => true),
      onAssignOwner: vi.fn(async () => true),
      onUnassignOwner: vi.fn(async () => true),
      onSetPriority: vi.fn(async () => true),
      onAddLabel: vi.fn(async () => true),
      onRemoveLabel: vi.fn(),
      onCloseIssue: vi.fn(async () => true),
      onReopenIssue: vi.fn(),
      onDeleteIssue: vi.fn(async () => true),
      ...props,
    },
  })
}

describe('IssueDetail', () => {
  afterEach(() => {
    cleanup()
    vi.useRealTimers()
  })

  it('renders the selected issue shell and composed sections', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-06-01T13:00:00Z'))
    renderDetail()

    const detail = screen.getByRole('region', { name: 'Task detail' })
    expect(within(detail).getByRole('heading', { name: 'Ship the thing' })).toBeTruthy()
    expect(within(detail).getByText('INBOX-1')).toBeTruthy()
    expect(within(detail).getByText('1h ago')).toBeTruthy()
    expect(within(detail).getByText('Initial body')).toBeTruthy()
  })

  it('edits title and description through the issue edit callback', async () => {
    const onEditIssue = vi.fn(async () => true)
    renderDetail({ onEditIssue })

    await fireEvent.click(screen.getByRole('button', { name: 'Edit title' }))
    await fireEvent.input(screen.getByLabelText('Edit title'), {
      target: { value: 'Updated title' },
    })
    await fireEvent.keyDown(screen.getByLabelText('Edit title'), { key: 'Enter' })

    expect(onEditIssue).toHaveBeenCalledWith('issue-1', { title: 'Updated title' })

    await fireEvent.click(screen.getByRole('button', { name: 'Edit description' }))
    await fireEvent.input(screen.getByLabelText('Edit description'), {
      target: { value: 'Updated body' },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    expect(onEditIssue).toHaveBeenCalledWith('issue-1', { body: 'Updated body' })
  })

  it('keeps unrelated drafts visible for manual copying after local authority expires', async () => {
    const onEditIssue = vi.fn(async () => true)
    const view = renderDetail({ onEditIssue })

    await fireEvent.click(screen.getByRole('button', { name: 'Edit description' }))
    await fireEvent.input(screen.getByLabelText('Edit description'), {
      target: { value: 'Keep this unrelated body draft' },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Edit title' }))
    await fireEvent.input(screen.getByLabelText('Edit title'), {
      target: { value: 'Accepted title' },
    })
    await fireEvent.keyDown(screen.getByLabelText('Edit title'), { key: 'Enter' })

    await view.rerender({ actionsDisabled: true })

    expect(
      (screen.getByRole('region', { name: 'Task detail' }) as HTMLElement & { inert: boolean })
        .inert,
    ).toBe(true)
    expect((screen.getByRole('textbox', { name: 'Edit title' }) as HTMLInputElement).value).toBe(
      'Accepted title',
    )
    expect(
      (screen.getByRole('textbox', { name: 'Edit description' }) as HTMLTextAreaElement).value,
    ).toBe('Keep this unrelated body draft')

    await view.rerender({ actionsDisabled: true, draftResetGeneration: 1 })
    await view.rerender({ actionsDisabled: false })

    expect(screen.queryByRole('textbox', { name: 'Edit title' })).toBeNull()
    expect(
      (screen.getByRole('textbox', { name: 'Edit description' }) as HTMLTextAreaElement).value,
    ).toBe('Keep this unrelated body draft')
    expect(onEditIssue).toHaveBeenCalledWith('issue-1', { title: 'Accepted title' })
  })

  it('preserves a newer selection draft when an older mutation reset arrives', async () => {
    const view = renderDetail({
      issue: makeIssue({ uid: 'issue-2', short_id: 'I-2', qualified_id: 'INBOX-2' }),
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Edit description' }))
    await fireEvent.input(screen.getByLabelText('Edit description'), {
      target: { value: 'Draft on the newer task' },
    })

    await view.rerender({ actionsDisabled: true, draftResetGeneration: 1 })
    await view.rerender({ actionsDisabled: false })

    expect(
      (screen.getByRole('textbox', { name: 'Edit description' }) as HTMLTextAreaElement).value,
    ).toBe('Draft on the newer task')
  })

  it('moves the issue from the task actions menu', async () => {
    const onMoveIssue = vi.fn(async () => true)
    renderDetail({ onMoveIssue })

    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }))
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Move to another project' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Roadmap 1' }))

    expect(onMoveIssue).toHaveBeenCalledWith('project-2')
  })

  it('composes the ported checklist, recurrence, comments, links, and history sections', async () => {
    const onAddComment = vi.fn(async () => true)
    renderDetail({
      issue: {
        ...makeIssue({ metadata: {} }),
        comments: [
          {
            id: 1,
            issue_id: 1,
            author: 'user-a',
            body: 'Accepted comment',
            created_at: '2026-06-01T12:30:00Z',
          },
        ],
      },
      events: [
        {
          event_id: 1,
          event_uid: 'event-1',
          origin_instance_uid: 'instance-example',
          type: 'issue.commented',
          project_id: 1,
          project_uid: 'project-1',
          project_name: 'example-project',
          actor: 'user-a',
          created_at: '2026-06-01T12:31:00Z',
        },
      ],
      selectedRecurrences: [
        {
          id: 1,
          uid: 'recurrence-1',
          project_id: 1,
          rrule: 'FREQ=WEEKLY;COUNT=2',
          dtstart: '2026-06-01',
          timezone: 'UTC',
          template_title: 'Weekly example',
          template_body: '',
          template_labels: [],
          template_metadata: {},
          next_occurrence_key: '2026-06-08',
          author: 'user-a',
          revision: 1,
          created_at: '2026-06-01T12:00:00Z',
          updated_at: '2026-06-01T12:00:00Z',
        },
      ],
      onAddComment,
    } as Partial<IssueDetailProps>)

    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }))
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Add checklist' }))
    expect(screen.getByRole('region', { name: 'Checklist' })).not.toBeNull()
    expect(screen.getByText('Weekly example')).not.toBeNull()
    expect(screen.getByText('Accepted comment')).not.toBeNull()
    expect(screen.getByText('commented')).not.toBeNull()

    await fireEvent.input(screen.getByLabelText('Comment'), {
      target: { value: 'New comment' },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Add comment' }))
    expect(onAddComment).toHaveBeenCalledWith('issue-1', 'New comment')
  })

  it('opens the reachable graph for the selected task', async () => {
    const onOpenGraph = vi.fn()
    renderDetail({ onOpenGraph })

    await fireEvent.click(screen.getByRole('button', { name: 'Open reachable graph' }))

    expect(onOpenGraph).toHaveBeenCalledWith(expect.objectContaining({ uid: 'issue-1' }))
  })

  it('falls back to project UID when the issue omits project name', () => {
    renderDetail({
      issue: makeIssue({
        project_id: 1,
        project_uid: 'project-2',
        project_name: '',
      }),
    })

    expect(screen.getByText('Roadmap')).toBeTruthy()
  })
})
