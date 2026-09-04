import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { UIIssueReference } from '../lib/api/generated'
import type { KataTaskDetail } from '../lib/kata/types'
import Comments from './Comments.svelte'

type Reference = UIIssueReference

describe('Comments', () => {
  afterEach(cleanup)

  it('submits comments and preserves an uncertain draft until accepted authority replaces it', async () => {
    const onAddComment = vi.fn(async () => false)
    const view = renderComments({ onAddComment, draftResetGeneration: 0 })

    await fireEvent.input(screen.getByLabelText('Comment'), {
      target: { value: 'Keep this reply' },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Add comment' }))

    expect(onAddComment).toHaveBeenCalledWith('issue-1', 'Keep this reply')
    expect((screen.getByLabelText('Comment') as HTMLTextAreaElement).value).toBe('Keep this reply')

    await view.rerender({
      onAddComment: vi.fn(async () => true),
      draftResetGeneration: 1,
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Add comment' }))
    expect((screen.getByLabelText('Comment') as HTMLTextAreaElement).value).toBe('Keep this reply')

    await view.rerender({ draftResetGeneration: 2 })
    expect((screen.getByLabelText('Comment') as HTMLTextAreaElement).value).toBe('')
  })

  it('fences an old-authority draft until the user edits it under current authority', async () => {
    const onAddComment = vi.fn(async () => true)
    const view = renderComments({ onAddComment, draftFenceGeneration: 0 })

    const composer = screen.getByLabelText('Comment') as HTMLTextAreaElement
    await fireEvent.input(composer, { target: { value: 'Old authority draft' } })
    await view.rerender({ draftFenceGeneration: 1 })

    expect(
      (screen.getByRole('button', { name: 'Add comment' }) as HTMLButtonElement).disabled,
    ).toBe(true)

    await fireEvent.input(composer, { target: { value: 'Old authority draft, reviewed' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Add comment' }))

    expect(onAddComment).toHaveBeenCalledWith('issue-1', 'Old authority draft, reviewed')
  })

  it('does not accept draft edits made while replacement authority is pending', async () => {
    const view = renderComments({ draftFenceGeneration: 0 })
    const composer = screen.getByLabelText('Comment') as HTMLTextAreaElement

    await fireEvent.input(composer, { target: { value: 'Old authority draft' } })
    await view.rerender({ actionsDisabled: true, draftFenceGeneration: 1 })
    await fireEvent.input(composer, { target: { value: 'Edited during renewal' } })
    await view.rerender({ actionsDisabled: false })

    expect(
      (screen.getByRole('button', { name: 'Add comment' }) as HTMLButtonElement).disabled,
    ).toBe(true)
  })

  it('inserts short references and qualifies duplicate names', async () => {
    const searchReferences = vi.fn(
      async (): Promise<Reference[]> => [
        reference({ uid: 'issue-a', project_name: 'example-project' }),
        reference({
          uid: 'issue-b',
          project_uid: 'project-b',
          project_name: 'example-workspace',
          title: 'Second shared issue',
        }),
      ],
    )
    renderComments({ searchReferences })

    const composer = screen.getByLabelText('Comment') as HTMLTextAreaElement
    await fireEvent.input(composer, { target: { value: 'see #shared' } })
    await waitFor(() =>
      expect(screen.getByRole('listbox', { name: 'Insert reference' })).not.toBeNull(),
    )
    await fireEvent.keyDown(composer, { key: 'Enter' })

    await waitFor(() => expect(composer.value).toBe('see #example-project#shared-1 '))
    expect(searchReferences).toHaveBeenCalledWith('shared')
  })

  it('renders accepted comments newest first', () => {
    renderComments()

    const comments = screen.getAllByRole('article')
    expect(comments[0]?.textContent).toContain('Newest comment')
    expect(comments[1]?.textContent).toContain('First comment')
  })
})

function renderComments(overrides: Record<string, unknown> = {}) {
  return render(Comments, {
    props: {
      issue: issue(),
      searchReferences: vi.fn(async () => []),
      actionsDisabled: false,
      draftResetGeneration: 0,
      onAddComment: vi.fn(async () => true),
      ...overrides,
    },
  })
}

function issue(): KataTaskDetail {
  return {
    issue: {
      id: 1,
      uid: 'issue-1',
      project_id: 1,
      project_uid: 'project-1',
      project_name: 'example-project',
      short_id: 'example-1',
      qualified_id: 'example-project#example-1',
      title: 'Example issue',
      body: 'Body',
      status: 'open',
      metadata: {},
      revision: 1,
      author: 'user-a',
      created_at: '2026-08-01T12:00:00Z',
      updated_at: '2026-08-01T12:00:00Z',
    },
    comments: [
      {
        id: 1,
        issue_id: 1,
        author: 'user-a',
        body: 'First comment',
        created_at: '2026-08-01T12:30:00Z',
      },
      {
        id: 2,
        issue_id: 1,
        author: 'user-a',
        body: 'Newest comment',
        created_at: '2026-08-01T12:45:00Z',
      },
    ],
    labels: [],
    links: [],
  }
}

function reference(overrides: Partial<Reference> = {}): Reference {
  return {
    uid: 'issue-a',
    project_uid: 'project-a',
    project_name: 'example-project',
    short_id: 'shared-1',
    qualified_id: 'example-project#shared-1',
    title: 'Shared issue',
    status: 'open',
    ...overrides,
  }
}
