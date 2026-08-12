import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { KataTaskDetail } from '../lib/kata/types'

import Checklist from './Checklist.svelte'

function makeIssue(
  checklist: KataTaskDetail['issue']['metadata']['checklist'] = [],
  overrides: Partial<KataTaskDetail['issue']> = {},
): KataTaskDetail {
  return {
    issue: {
      id: 1,
      uid: 'issue-1',
      project_id: 1,
      project_uid: 'project-1',
      project_name: 'example-project',
      short_id: 'I-1',
      qualified_id: 'example-project#1',
      title: 'Ship the thing',
      body: 'Body',
      status: 'open',
      metadata: { checklist },
      revision: 1,
      author: 'user-a',
      created_at: '2026-06-01T12:00:00Z',
      updated_at: '2026-06-01T12:00:00Z',
      ...overrides,
    },
    comments: [],
    labels: [],
    links: [],
  }
}

describe('Checklist', () => {
  afterEach(() => {
    cleanup()
  })

  it('stays hidden until an empty checklist is revealed', async () => {
    const { rerender } = render(Checklist, {
      props: {
        issue: makeIssue(),
        revealed: false,
        onPatchMetadata: vi.fn(async () => true),
        onReveal: vi.fn(),
      },
    })

    expect(screen.queryByRole('region', { name: 'Checklist' })).toBeNull()

    await rerender({
      issue: makeIssue(),
      revealed: true,
      onPatchMetadata: vi.fn(async () => true),
      onReveal: vi.fn(),
    })

    expect(screen.getByRole('region', { name: 'Checklist' })).toBeTruthy()
  })

  it('full-replaces checklist metadata for add, toggle, and remove', async () => {
    const onPatchMetadata = vi.fn(async () => true)
    const onReveal = vi.fn()

    const { rerender } = render(Checklist, {
      props: {
        issue: makeIssue([{ id: 'item-1', text: 'Send', done: false }]),
        revealed: false,
        onPatchMetadata,
        onReveal,
      },
    })

    await fireEvent.input(screen.getByLabelText('New checklist item'), {
      target: { value: 'Confirm' },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Add' }))

    expect(onPatchMetadata).toHaveBeenLastCalledWith('issue-1', {
      checklist: [
        { id: 'item-1', text: 'Send', done: false },
        { id: expect.any(String), text: 'Confirm', done: false },
      ],
    })
    expect((screen.getByLabelText('New checklist item') as HTMLInputElement).value).toBe('Confirm')

    await rerender({
      issue: makeIssue([
        { id: 'item-1', text: 'Send', done: false },
        { id: 'item-2', text: 'Confirm', done: false },
      ]),
      revealed: false,
      draftResetGeneration: 1,
      onPatchMetadata,
      onReveal,
    })
    expect((screen.getByLabelText('New checklist item') as HTMLInputElement).value).toBe('')
    await fireEvent.click(screen.getByLabelText('Send'))

    expect(onPatchMetadata).toHaveBeenLastCalledWith('issue-1', {
      checklist: [
        { id: 'item-1', text: 'Send', done: true },
        { id: 'item-2', text: 'Confirm', done: false },
      ],
    })

    await rerender({
      issue: makeIssue([{ id: 'item-1', text: 'Send', done: false }]),
      revealed: false,
      onPatchMetadata,
      onReveal,
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Remove Send' }))

    expect(onPatchMetadata).toHaveBeenLastCalledWith('issue-1', { checklist: [] })
    await waitFor(() => {
      expect(onReveal).toHaveBeenCalledTimes(1)
    })
  })

  it('clears the add-item draft when the selected task changes', async () => {
    const { rerender } = render(Checklist, {
      props: {
        issue: makeIssue(),
        revealed: true,
        onPatchMetadata: vi.fn(async () => true),
        onReveal: vi.fn(),
      },
    })

    await fireEvent.input(screen.getByLabelText('New checklist item'), {
      target: { value: 'Leaked draft' },
    })
    await rerender({
      issue: makeIssue([], { uid: 'issue-2', short_id: 'I-2', qualified_id: 'example-project#2' }),
      revealed: true,
      onPatchMetadata: vi.fn(async () => true),
      onReveal: vi.fn(),
    })

    expect((screen.getByLabelText('New checklist item') as HTMLInputElement).value).toBe('')
  })

  it('preserves the add-item draft when the mutation transport fails', async () => {
    const onPatchMetadata = vi.fn(async () => false)
    render(Checklist, {
      props: {
        issue: makeIssue(),
        revealed: true,
        onPatchMetadata,
        onReveal: vi.fn(),
      },
    })

    await fireEvent.input(screen.getByLabelText('New checklist item'), {
      target: { value: 'Keep this draft' },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Add' }))

    await waitFor(() => expect(onPatchMetadata).toHaveBeenCalledOnce())
    expect((screen.getByLabelText('New checklist item') as HTMLInputElement).value).toBe(
      'Keep this draft',
    )
  })

  it('resets the add-item draft after authentication authority changes', async () => {
    const view = render(Checklist, {
      props: {
        issue: makeIssue(),
        revealed: true,
        draftFenceGeneration: 0,
        onPatchMetadata: vi.fn(async () => true),
        onReveal: vi.fn(),
      },
    })

    await fireEvent.input(screen.getByLabelText('New checklist item'), {
      target: { value: 'Old authority item' },
    })
    await view.rerender({ draftFenceGeneration: 1 })

    expect((screen.getByLabelText('New checklist item') as HTMLInputElement).value).toBe('')
  })

  it('keeps checklist mutations disabled while the owning snapshot is stale', async () => {
    const onPatchMetadata = vi.fn(async () => true)
    render(Checklist, {
      props: {
        issue: makeIssue([{ id: 'item-1', text: 'Send', done: false }]),
        revealed: false,
        disabled: true,
        onPatchMetadata,
        onReveal: vi.fn(),
      },
    })

    expect((screen.getByLabelText('Send') as HTMLInputElement).disabled).toBe(true)
    expect(
      (screen.getByRole('button', { name: 'Remove Send' }) as HTMLButtonElement).disabled,
    ).toBe(true)
    expect((screen.getByLabelText('New checklist item') as HTMLInputElement).disabled).toBe(true)
    expect((screen.getByRole('button', { name: 'Add' }) as HTMLButtonElement).disabled).toBe(true)

    await fireEvent.click(screen.getByLabelText('Send'))
    expect(onPatchMetadata).not.toHaveBeenCalled()
  })
})
