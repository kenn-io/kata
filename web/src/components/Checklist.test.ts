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

  it('reveals the add field when requested', async () => {
    render(Checklist, { props: { issue: makeIssue(), onPatchMetadata: vi.fn(async () => true) } })
    expect(screen.queryByRole('textbox', { name: 'New checklist item' })).toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }))
    expect(screen.getByRole('textbox', { name: 'New checklist item' })).toBeTruthy()
  })

  it('full-replaces checklist metadata for add, toggle, and remove', async () => {
    const onPatchMetadata = vi.fn(async () => true)

    const { rerender } = render(Checklist, {
      props: {
        issue: makeIssue([{ id: 'item-1', text: 'Send', done: false }]),
        onPatchMetadata,
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }))
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
      draftResetGeneration: 1,
      onPatchMetadata,
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
      onPatchMetadata,
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Remove Send' }))

    expect(onPatchMetadata).toHaveBeenLastCalledWith('issue-1', { checklist: [] })
  })

  it('keeps a failed rename draft for retry', async () => {
    const onPatchMetadata = vi.fn().mockResolvedValueOnce(false).mockResolvedValueOnce(true)
    render(Checklist, {
      props: { issue: makeIssue([{ id: 'item-1', text: 'Send', done: false }]), onPatchMetadata },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Rename Send' }))
    await fireEvent.input(screen.getByRole('textbox', { name: 'Rename checklist item' }), {
      target: { value: 'Send report' },
    })
    await fireEvent.keyDown(screen.getByRole('textbox', { name: 'Rename checklist item' }), {
      key: 'Enter',
    })
    await vi.waitFor(() => expect(onPatchMetadata).toHaveBeenCalledOnce())
    expect(
      (screen.getByRole('textbox', { name: 'Rename checklist item' }) as HTMLInputElement).value,
    ).toBe('Send report')
    await fireEvent.keyDown(screen.getByRole('textbox', { name: 'Rename checklist item' }), {
      key: 'Enter',
    })
    expect(onPatchMetadata).toHaveBeenCalledTimes(2)
    expect(onPatchMetadata).toHaveBeenLastCalledWith('issue-1', {
      checklist: [{ id: 'item-1', text: 'Send report', done: false }],
    })
  })

  it('clears the add-item draft when the selected task changes', async () => {
    const { rerender } = render(Checklist, {
      props: {
        issue: makeIssue(),
        onPatchMetadata: vi.fn(async () => true),
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }))
    await fireEvent.input(screen.getByLabelText('New checklist item'), {
      target: { value: 'Leaked draft' },
    })
    await rerender({
      issue: makeIssue([], { uid: 'issue-2', short_id: 'I-2', qualified_id: 'example-project#2' }),
      onPatchMetadata: vi.fn(async () => true),
    })

    expect((screen.getByLabelText('New checklist item') as HTMLInputElement).value).toBe('')
  })

  it('preserves the add-item draft when the mutation transport fails', async () => {
    const onPatchMetadata = vi.fn(async () => false)
    render(Checklist, {
      props: {
        issue: makeIssue(),
        onPatchMetadata,
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }))
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
        draftFenceGeneration: 0,
        onPatchMetadata: vi.fn(async () => true),
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Add item' }))
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
        disabled: true,
        onPatchMetadata,
      },
    })

    expect((screen.getByLabelText('Send') as HTMLInputElement).disabled).toBe(true)
    expect(
      (screen.getByRole('button', { name: 'Remove Send' }) as HTMLButtonElement).disabled,
    ).toBe(true)
    expect((screen.getByRole('button', { name: 'Add item' }) as HTMLButtonElement).disabled).toBe(
      true,
    )

    await fireEvent.click(screen.getByLabelText('Send'))
    expect(onPatchMetadata).not.toHaveBeenCalled()
  })
})
