import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte'
import type { ComponentProps } from 'svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type {
  KataProjectSummary,
  KataTaskDetail,
  KataTaskEvent,
  KataTaskSummary,
} from '../lib/kata/types'
import type { UIIssueReference } from '../lib/api/generated'

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

function makeSummary(): KataTaskSummary {
  return {
    id: 2,
    uid: 'blocker-1',
    project_id: 2,
    project_uid: 'project-2',
    project_name: 'Roadmap',
    short_id: 'B-1',
    qualified_id: 'Roadmap#B-1',
    title: 'Verify example packages',
    status: 'open',
    metadata: {},
    revision: 1,
    author: 'user-a',
    created_at: '2026-06-01T12:00:00Z',
    updated_at: '2026-06-01T12:00:00Z',
  }
}

function makeReference(task: KataTaskSummary): UIIssueReference {
  return {
    uid: task.uid,
    short_id: task.short_id,
    qualified_id: task.qualified_id,
    project_uid: task.project_uid,
    project_name: task.project_name,
    title: task.title,
    status: task.status,
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
      ...props,
    },
  })
}

describe('IssueDetail', () => {
  afterEach(() => {
    cleanup()
    vi.useRealTimers()
  })

  it('keeps an empty description as a one-line section with its Edit action', async () => {
    renderDetail({ issue: makeIssue({ body: '' }) })
    const section = screen.getByRole('region', { name: 'Description' })
    expect(within(section).getByText('No description')).toBeTruthy()
    await fireEvent.click(within(section).getByRole('button', { name: 'Edit description' }))
    expect(within(section).queryByText('No description')).toBeNull()
  })

  it('resolves a blocker title when the selected task list omits the blocker', async () => {
    const blocker = makeSummary()
    const searchReferences = vi.fn(async (query: string) =>
      query === blocker.qualified_id ? [makeReference(blocker)] : [],
    )
    renderDetail({
      issue: makeIssue({
        open_blocked_by: [
          {
            uid: blocker.uid,
            short_id: blocker.short_id,
            qualified_id: blocker.qualified_id,
          },
        ],
      }),
      issueCatalog: [],
      searchReferences,
    })

    await vi.waitFor(() => expect(searchReferences).toHaveBeenCalledWith(blocker.qualified_id))
    await vi.waitFor(() =>
      expect(screen.getByRole('region', { name: 'Task detail' }).textContent).toContain(
        'Verify example packages',
      ),
    )
  })

  it('shows the blocker and overdue date when both apply', () => {
    const blocker = makeSummary()
    const view = renderDetail({
      issue: makeIssue({
        open_blocked_by: [
          {
            uid: blocker.uid,
            short_id: blocker.short_id,
            qualified_id: blocker.qualified_id,
          },
        ],
        metadata: { deadline_on: '2020-01-01' },
      }),
      issueCatalog: [blocker],
    })

    const stateLines = [...view.container.querySelectorAll('.state-line')]
      .map((line) => line.textContent ?? '')
      .join(' ')
    expect(stateLines).toMatch(/Blocked by B-1 Verify example packages/)
    expect(stateLines).toMatch(/Due Jan 1(?:, \d{4})?, \d+ days late/)
  })

  it('updates the overdue count as the date changes', async () => {
    const view = renderDetail({
      issue: makeIssue({ metadata: { deadline_on: '2026-11-21' } }),
      today: '2026-11-21',
    })
    expect(view.container.querySelector('.state-line')).toBeNull()
    await view.rerender({ today: '2026-11-22' })
    expect(view.container.querySelector('.state-line')?.textContent).toContain('1 day late')
    await view.rerender({ today: '2026-11-23' })
    expect(view.container.querySelector('.state-line')?.textContent).toContain('2 days late')
  })

  it.each([
    { field: 'title', title: 'Ship the thing', revision: 0, message: null },
    { field: 'title', title: 'Their title', revision: 3, message: /while you were editing/ },
    { field: 'title', title: 'Ship the thing', revision: 4, message: /Someone changed this task/ },
    {
      field: 'description',
      title: 'Ship the thing',
      revision: 3,
      message: /Someone changed this task/,
    },
  ])(
    'keeps and retries the $field draft after a failed save at revision $revision',
    async ({ field, title, revision, message }) => {
      let finish!: (ok: boolean) => void
      const pending = new Promise<boolean>((resolve) => {
        finish = resolve
      })
      const onEditIssue = vi.fn().mockReturnValueOnce(pending).mockResolvedValueOnce(true)
      const view = renderDetail({ issue: makeIssue({ content_revision: 0 }), onEditIssue })
      const isTitle = field === 'title'
      const editName = isTitle ? 'Edit title' : 'Edit description'
      const draft = isTitle ? 'My title' : 'My description'
      const key = isTitle ? 'title' : 'body'
      await fireEvent.click(screen.getByRole('button', { name: editName }))
      await fireEvent.input(screen.getByRole('textbox', { name: editName }), {
        target: { value: draft },
      })
      const submit = async () => {
        if (isTitle)
          await fireEvent.keyDown(screen.getByRole('textbox', { name: editName }), { key: 'Enter' })
        else
          await fireEvent.click(
            within(screen.getByRole('region', { name: 'Description' })).getByRole('button', {
              name: 'Save',
            }),
          )
      }
      await submit()
      expect(onEditIssue).toHaveBeenLastCalledWith('issue-1', {
        [key]: draft,
        expected_content_revision: 0,
      })
      await view.rerender({ issue: makeIssue({ title, content_revision: revision }) })
      finish(false)
      if (message) await vi.waitFor(() => expect(screen.getByText(message)).toBeTruthy())
      expect((screen.getByRole('textbox', { name: editName }) as HTMLInputElement).value).toBe(
        draft,
      )
      if (isTitle) {
        await fireEvent.blur(screen.getByRole('textbox', { name: editName }))
        expect(onEditIssue).toHaveBeenCalledTimes(1)
      } else {
        await vi.waitFor(() =>
          expect(
            within(screen.getByRole('region', { name: 'Description' }))
              .getByRole('button', { name: 'Save' })
              .hasAttribute('disabled'),
          ).toBe(false),
        )
      }
      await submit()
      await vi.waitFor(() => expect(onEditIssue).toHaveBeenCalledTimes(2))
      expect(onEditIssue).toHaveBeenLastCalledWith('issue-1', {
        [key]: draft,
        expected_content_revision: revision,
      })
    },
  )

  it.each(['evidence', 'older effective event', 'no effective event'] as const)(
    'shows the closed task line with %s',
    (kind) => {
      const close = (event_id: number, actor: string, reason: string): KataTaskEvent => ({
        event_id,
        event_uid: `close-${event_id}`,
        origin_instance_uid: 'instance-1',
        type: 'issue.closed',
        project_id: 1,
        project_uid: 'project-1',
        project_name: 'Inbox',
        actor,
        created_at: '2026-06-02T12:00:00Z',
        payload: { reason },
      })
      const event = close(4, kind === 'evidence' ? 'user-a' : 'user-c', 'done')
      if (kind === 'evidence')
        event.payload!.evidence = [{ type: 'test', command: 'go test ./example' }]
      const view = renderDetail({
        issue: {
          ...makeIssue({
            status: 'closed',
            closed_reason: kind === 'no effective event' ? 'wontfix' : 'done',
            closed_at: '2026-06-03T12:00:00Z',
          }),
          ...(kind === 'no effective event' ? {} : { effective_close_event: event }),
        },
        selfActor: 'user-a',
        events: [close(9, 'user-b', 'wontfix')],
      })
      const line = view.container.querySelector('.state-line')?.textContent ?? ''
      if (kind === 'evidence') {
        expect(line).toMatch(/Done\s*·\s*You/)
        expect(screen.getByText('go test ./example')).toBeTruthy()
      } else if (kind === 'older effective event') {
        expect(line).toMatch(/Done\s*·\s*user-c/)
        expect(line).not.toContain('user-b')
      } else {
        expect(line).toContain("Won't do")
        expect(line).toContain('Jun 3')
        expect(line).not.toContain('Done')
        expect(line).not.toContain('You')
      }
    },
  )

  it('waits for the requested task before opening its completion dialog', async () => {
    const onStatusRequestConsumed = vi.fn()
    const view = renderDetail({
      statusRequest: { uid: 'issue-2', generation: 1 },
      onStatusRequestConsumed,
    })
    expect(screen.queryByRole('dialog', { name: 'Complete task' })).toBeNull()
    await view.rerender({
      issue: makeIssue({ uid: 'issue-2', title: 'Second task' }),
      pending: true,
    })
    expect(screen.queryByRole('dialog', { name: 'Complete task' })).toBeNull()
    await view.rerender({ pending: false })
    expect(screen.getByRole('dialog', { name: 'Complete task' }).textContent).toContain(
      'Second task',
    )
    expect(onStatusRequestConsumed).toHaveBeenCalledOnce()
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

  it('resets title and description drafts after authentication authority changes', async () => {
    const view = renderDetail({ draftFenceGeneration: 0 })

    await fireEvent.click(screen.getByRole('button', { name: 'Edit title' }))
    await fireEvent.input(screen.getByLabelText('Edit title'), {
      target: { value: 'Old authority title' },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Edit description' }))
    await fireEvent.input(screen.getByLabelText('Edit description'), {
      target: { value: 'Old authority body' },
    })

    await view.rerender({ draftFenceGeneration: 1 })

    expect(screen.queryByRole('textbox', { name: 'Edit title' })).toBeNull()
    expect(screen.queryByRole('textbox', { name: 'Edit description' })).toBeNull()
  })

  it('moves the issue from its project property', async () => {
    const onMoveIssue = vi.fn(async () => true)
    renderDetail({ onMoveIssue })

    await fireEvent.click(screen.getByRole('button', { name: 'Project: Inbox' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Roadmap 1 open' }))

    expect(onMoveIssue).toHaveBeenCalledWith('project-2')
  })

  it('offers recurrence creation only on an open task without a rule', async () => {
    const view = renderDetail({ issue: makeIssue({ metadata: {} }), selectedRecurrences: [] })
    expect(screen.getByRole('button', { name: 'Repeats…' })).toBeTruthy()
    await view.rerender({ issue: makeIssue({ status: 'closed', metadata: {} }) })
    expect(screen.queryByRole('button', { name: 'Repeats…' })).toBeNull()
  })

  it('withholds recurrence actions while a mutation is pending', async () => {
    const recurrence = {
      id: 1,
      uid: 'rec-1',
      project_id: 1,
      rrule: 'FREQ=WEEKLY',
      dtstart: '2026-05-20',
      timezone: 'UTC',
      template_title: 'Weekly task',
      template_body: '',
      template_owner: '',
      template_labels: [],
      template_metadata: {},
      next_occurrence_key: '2026-05-27',
      author: 'user-a',
      revision: 1,
      created_at: '2026-05-15T12:00:00Z',
      updated_at: '2026-05-15T12:00:00Z',
    }
    const view = renderDetail({ selectedRecurrences: [recurrence] })
    expect(screen.queryByText('Repeats')).toBeNull()
    await view.rerender({ issue: makeIssue({ recurrence_id: 1 }) })
    expect(screen.getByText(/Weekly task/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull()
  })

  it('closes a recurrence draft after authentication authority changes', async () => {
    const view = renderDetail({
      issue: makeIssue({ metadata: {} }),
      selectedRecurrences: [],
      draftFenceGeneration: 0,
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Repeats…' }))
    const recurrenceDialog = screen.getByRole('dialog')
    await fireEvent.input(within(recurrenceDialog).getByLabelText(/Title/i), {
      target: { value: 'Old authority recurrence' },
    })

    await view.rerender({ draftFenceGeneration: 1 })

    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('passes comments to the selected task', async () => {
    const onAddComment = vi.fn(async () => true)
    renderDetail({ onAddComment })
    await fireEvent.click(screen.getByRole('button', { name: 'Add comment' }))
    await fireEvent.input(screen.getByLabelText('Comment'), {
      target: { value: 'New comment' },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Add comment' }))
    expect(onAddComment).toHaveBeenCalledWith('issue-1', 'New comment')
  })

  it('opens a related issue from the read view links', async () => {
    const onSelectIssue = vi.fn()
    renderDetail({
      onSelectIssue,
      issue: {
        ...makeIssue(),
        links: [
          {
            id: 9,
            project_id: 1,
            type: 'related',
            author: 'user-a',
            created_at: '2026-06-01T12:00:00Z',
            from: { uid: 'issue-1', short_id: 'I-1', qualified_id: 'INBOX-1', status: 'open' },
            to: { uid: 'issue-2', short_id: 'I-2', qualified_id: 'INBOX-2', status: 'closed' },
          },
        ],
      } as KataTaskDetail,
    })

    await fireEvent.click(screen.getByRole('button', { name: /^Related INBOX-2/ }))

    expect(onSelectIssue).toHaveBeenCalledWith({ uid: 'issue-2' })
  })

  it.each([false, true])(
    'opens the reachable graph for the selected task (read-only: %s)',
    async (actionsDisabled) => {
      const onOpenGraph = vi.fn()
      renderDetail({ onOpenGraph, actionsDisabled, authorityBlocked: false })

      await fireEvent.click(screen.getByRole('button', { name: 'Graph' }))

      expect(onOpenGraph).toHaveBeenCalledWith(expect.objectContaining({ uid: 'issue-1' }))
    },
  )

  it.each([{ actionsDisabled: true }, { authorityBlocked: true }])(
    'disables completion when authority is fenced',
    async (fence) => {
      renderDetail(fence)
      const action = screen.getByRole('button', { name: 'Complete…' }) as HTMLButtonElement
      expect(action.disabled).toBe(true)
      await fireEvent.click(action)
      expect(screen.queryByRole('dialog', { name: 'Complete task' })).toBeNull()
    },
  )
})

describe('comment evidence navigation', () => {
  afterEach(cleanup)
  it('keeps the return route available in the task view', async () => {
    const onSelectIssue = vi.fn()
    renderDetail({
      onSelectIssue,
      returnComment: { issueUID: 'source-issue', commentUID: 'original' },
    })
    const back = screen.getByRole('button', { name: 'Return to original comment' })
    await fireEvent.click(back)
    expect(onSelectIssue).toHaveBeenCalledWith({ uid: 'source-issue', commentUID: 'original' })
  })
})

it('disables attachment uploads with blocked authority or disabled actions', async () => {
  const view = renderDetail({
    authorityBlocked: true,
    attachmentsEnabled: true,
    onUploadAttachment: vi.fn(async () => {}),
  })
  expect(screen.getByRole('button', { name: 'Attach files' }).hasAttribute('disabled')).toBe(true)
  await view.rerender({ authorityBlocked: false, actionsDisabled: true })
  expect(screen.getByRole('button', { name: 'Attach files' }).hasAttribute('disabled')).toBe(true)
})
