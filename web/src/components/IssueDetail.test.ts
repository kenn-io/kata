import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte'
import type { ComponentProps } from 'svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { KataProjectSummary, KataTaskDetail, KataTaskSummary } from '../lib/kata/types'
import { createCredentialedFetch, setGeneratedFetch } from '../lib/api/client'

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

  it('edits the task in one view', () => {
    renderDetail()
    expect(screen.getByRole('heading', { name: 'Ship the thing' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Edit title' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Edit issue' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Done editing' })).toBeNull()
  })

  it('keeps an empty description as a one-line section with its Edit action', async () => {
    const onEditIssue = vi.fn(async () => true)
    renderDetail({ issue: makeIssue({ body: '' }), onEditIssue })
    const section = screen.getByRole('region', { name: 'Description' })
    expect(within(section).getByText('No description')).toBeTruthy()
    await fireEvent.click(within(section).getByRole('button', { name: 'Edit description' }))
    expect(within(section).queryByText('No description')).toBeNull()
    await fireEvent.input(within(section).getByRole('textbox', { name: 'Edit description' }), {
      target: { value: 'First description' },
    })
    await fireEvent.click(within(section).getByRole('button', { name: 'Save' }))
    expect(onEditIssue).toHaveBeenCalledWith('issue-1', { body: 'First description' })
  })

  it('resolves a blocker title when the selected task list omits the blocker', async () => {
    const blocker = makeSummary()
    const searchReferences = vi.fn(async (query: string) =>
      query === blocker.qualified_id ? [blocker] : [],
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

  it('updates the overdue count when the current date changes', async () => {
    const view = renderDetail({
      issue: makeIssue({ metadata: { deadline_on: '2026-11-21' } }),
      today: '2026-11-22',
    })

    expect(view.container.querySelector('.state-line')?.textContent).toContain('1 day late')
    await view.rerender({ today: '2026-11-23' })
    expect(view.container.querySelector('.state-line')?.textContent).toContain('2 days late')
  })

  it('shows no overdue warning on the due date', async () => {
    const view = renderDetail({
      issue: makeIssue({ metadata: { deadline_on: '2026-11-22' } }),
      today: '2026-11-22',
    })

    expect(view.container.querySelector('.state-line')).toBeNull()
    await view.rerender({ today: '2026-11-23' })
    expect(view.container.querySelector('.state-line')?.textContent).toContain('1 day late')
  })

  it('keeps a renamed title draft and retries against the new revision', async () => {
    let finish!: (ok: boolean) => void
    const pending = new Promise<boolean>((resolve) => {
      finish = resolve
    })
    const onEditIssue = vi.fn().mockReturnValueOnce(pending).mockResolvedValueOnce(true)
    const view = renderDetail({ issue: makeIssue({ content_revision: 2 }), onEditIssue })
    await fireEvent.click(screen.getByRole('button', { name: 'Edit title' }))
    await fireEvent.input(screen.getByRole('textbox', { name: 'Edit title' }), {
      target: { value: 'My title' },
    })
    await fireEvent.keyDown(screen.getByRole('textbox', { name: 'Edit title' }), { key: 'Enter' })
    await view.rerender({ issue: makeIssue({ title: 'Their title', content_revision: 3 }) })
    finish(false)
    await vi.waitFor(() => expect(screen.getByText(/while you were editing/)).toBeTruthy())
    expect((screen.getByRole('textbox', { name: 'Edit title' }) as HTMLInputElement).value).toBe(
      'My title',
    )
    await fireEvent.keyDown(screen.getByRole('textbox', { name: 'Edit title' }), { key: 'Enter' })
    expect(onEditIssue).toHaveBeenLastCalledWith('issue-1', {
      title: 'My title',
      expected_content_revision: 3,
    })
  })

  it('keeps a description draft and warns when another edit changes the content', async () => {
    let finish!: (ok: boolean) => void
    const pending = new Promise<boolean>((resolve) => {
      finish = resolve
    })
    const onEditIssue = vi.fn().mockReturnValueOnce(pending).mockResolvedValueOnce(true)
    const view = renderDetail({ issue: makeIssue({ content_revision: 2 }), onEditIssue })
    await fireEvent.click(screen.getByRole('button', { name: 'Edit description' }))
    await fireEvent.input(screen.getByRole('textbox', { name: 'Edit description' }), {
      target: { value: 'My description' },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    await view.rerender({ issue: makeIssue({ body: 'Their description', content_revision: 3 }) })
    finish(false)
    await vi.waitFor(() =>
      expect(screen.getByText(/changed the description while you were editing/)).toBeTruthy(),
    )
    expect(
      (screen.getByRole('textbox', { name: 'Edit description' }) as HTMLTextAreaElement).value,
    ).toBe('My description')
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(onEditIssue).toHaveBeenLastCalledWith('issue-1', {
      body: 'My description',
      expected_content_revision: 3,
    })
  })

  it('saves a content draft after a metadata-only update', async () => {
    const onEditIssue = vi.fn(async () => true)
    const view = renderDetail({ issue: makeIssue({ content_revision: 2 }), onEditIssue })
    await fireEvent.click(screen.getByRole('button', { name: 'Edit title' }))
    await fireEvent.input(screen.getByRole('textbox', { name: 'Edit title' }), {
      target: { value: 'New title' },
    })
    await view.rerender({ issue: makeIssue({ content_revision: 2, revision: 2, priority: 1 }) })
    await fireEvent.keyDown(screen.getByRole('textbox', { name: 'Edit title' }), { key: 'Enter' })
    expect(onEditIssue).toHaveBeenCalledWith('issue-1', {
      title: 'New title',
      expected_content_revision: 2,
    })
  })

  it('shows the close reason, actor, and evidence on a closed task', () => {
    const view = renderDetail({
      issue: makeIssue({ status: 'closed', closed_reason: 'done' }),
      selfActor: 'user-a',
      events: [
        {
          event_id: 1,
          event_uid: 'event-1',
          origin_instance_uid: 'instance-1',
          type: 'issue.closed',
          project_id: 1,
          project_uid: 'project-1',
          project_name: 'Inbox',
          actor: 'user-a',
          created_at: '2026-06-01T12:00:00Z',
          payload: { reason: 'done', evidence: [{ type: 'test', command: 'go test ./example' }] },
        },
      ],
    })
    expect(view.container.querySelector('.state-line')?.textContent).toMatch(/Done\s*·\s*You/)
    expect(screen.getByText('go test ./example')).toBeTruthy()
  })

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

  it('moves the issue from the task actions menu', async () => {
    const onMoveIssue = vi.fn(async () => true)
    renderDetail({ onMoveIssue })

    await fireEvent.click(screen.getByRole('button', { name: 'Project: Inbox' }))
    await fireEvent.click(screen.getByRole('button', { name: 'Roadmap 1' }))

    expect(onMoveIssue).toHaveBeenCalledWith('project-2')
  })

  it('offers recurrence creation only on an open task without a rule', async () => {
    const view = renderDetail({ issue: makeIssue({ metadata: {} }), selectedRecurrences: [] })
    expect(screen.getByRole('button', { name: 'Repeats…' })).toBeTruthy()
    await view.rerender({ issue: makeIssue({ status: 'closed', metadata: {} }) })
    expect(screen.queryByRole('button', { name: 'Repeats…' })).toBeNull()
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
            teammate: 'teammate-1',
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

    expect(screen.getByRole('region', { name: 'Checklist' })).not.toBeNull()
    expect(screen.getByRole('button', { name: 'Stop' })).not.toBeNull()
    expect(await screen.findByText('Accepted comment')).not.toBeNull()
    expect(screen.getByText('user-a / teammate-1')).not.toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: 'Show history' }))
    expect(screen.getByText(/commented/)).not.toBeNull()

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

  it('opens the reachable graph for the selected task', async () => {
    const onOpenGraph = vi.fn()
    renderDetail({ onOpenGraph })

    await fireEvent.click(screen.getByRole('button', { name: 'Graph' }))

    expect(onOpenGraph).toHaveBeenCalledWith(expect.objectContaining({ uid: 'issue-1' }))
  })

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

describe('editor evidence navigation', () => {
  afterEach(cleanup)
  it('keeps the return route available in the issue editor', async () => {
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

describe('typed reply draft across edit mode', () => {
  afterEach(cleanup)
  it('keeps an unsent reply after editing the issue', async () => {
    const issue = makeIssue()
    issue.comments = [
      {
        id: 1,
        uid: 'target-comment',
        issue_id: 1,
        author: 'finder',
        body: 'Finding',
        created_at: '2030-01-01T00:00:00Z',
        handle: 'c:abc123',
      },
    ]
    renderDetail({ issue, onAddComment: vi.fn(async () => false) })
    await fireEvent.click(screen.getByRole('button', { name: 'Reply' }))
    await fireEvent.click(screen.getByRole('radio', { name: 'Confirm, with evidence' }))
    const draft = 'Reproduced on the current build with the documented steps.'
    await fireEvent.input(screen.getByRole('textbox', { name: 'Reply evidence' }), {
      target: { value: draft },
    })
    const composer = screen.getByRole('textbox', { name: 'Reply evidence' }) as HTMLTextAreaElement
    expect(composer.value).toBe(draft)
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

it('refreshes a task attachment preview when its origin changes', async () => {
  const attachment = {
    name: 'example.png',
    media_type: 'image/png',
    size: 12,
    docbank_version: '6f1c2e8a-2222-4222-8222-222222222222',
    origin: 'instance-a',
  }
  const fetchAttachment = vi.fn<typeof fetch>(
    async () => new Response(new Blob(['image'], { type: 'image/png' })),
  )
  setGeneratedFetch(fetchAttachment)
  vi.stubGlobal(
    'URL',
    Object.assign(class extends URL {}, {
      createObjectURL: vi.fn(() => 'blob:preview'),
      revokeObjectURL: vi.fn(),
    }),
  )
  try {
    const view = renderDetail({
      issue: makeIssue({ metadata: { 'attachment.file': attachment } }),
      attachmentsEnabled: true,
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Preview example.png' }))
    await vi.waitFor(() => expect(screen.getByAltText('example.png')).toBeTruthy())
    expect(String(fetchAttachment.mock.calls[0]![0])).toBe(
      'http://localhost:3000/api/v1/projects/1/issues/issue-1/attachments/file/content',
    )
    await view.rerender({
      issue: makeIssue({
        metadata: { 'attachment.file': { ...attachment, origin: 'instance-b' } },
      }),
    })
    await vi.waitFor(() => expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:preview'))
    expect(screen.queryByAltText('example.png')).toBeNull()
  } finally {
    cleanup()
    vi.unstubAllGlobals()
    setGeneratedFetch(createCredentialedFetch())
  }
})
