import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { KataTaskDetail } from '../lib/kata/types'

import IssueStateDialog from './IssueStateDialog.svelte'

function makeIssue(status: 'open' | 'closed' = 'open'): KataTaskDetail {
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
      body: 'Body',
      status,
      metadata: {},
      revision: 1,
      author: 'user-a',
      created_at: '2026-06-01T12:00:00Z',
      updated_at: '2026-06-01T12:00:00Z',
    },
    comments: [],
    labels: [],
    links: [],
  }
}

describe('IssueStateDialog', () => {
  afterEach(() => {
    cleanup()
  })

  it('submits a valid wont-do close request', async () => {
    const onCloseIssue = vi.fn(async () => true)

    render(IssueStateDialog, {
      props: {
        issue: makeIssue(),
        onCloseIssue,
        onReopenIssue: vi.fn(),
      },
    })

    await fireEvent.click(screen.getAllByRole('button', { name: 'Complete' })[0]!)
    const dialog = screen.getByRole('dialog', { name: 'Complete task' })

    await fireEvent.click(within(dialog).getByRole('radio', { name: /Won't do/ }))
    await fireEvent.input(within(dialog).getByLabelText(/Completion note/), {
      target: {
        value:
          'This task is no longer aligned with the current example workflow and will not be pursued.',
      },
    })
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Complete' }))

    expect(onCloseIssue).toHaveBeenCalledWith({
      reason: 'wontfix',
      message:
        'This task is no longer aligned with the current example workflow and will not be pursued.',
      evidence: [],
    })
  })

  it('collects completion evidence before enabling a done close', async () => {
    const onCloseIssue = vi.fn(async () => true)

    render(IssueStateDialog, {
      props: { issue: makeIssue(), onCloseIssue, onReopenIssue: vi.fn() },
    })
    await fireEvent.click(screen.getAllByRole('button', { name: 'Complete' })[0]!)
    const dialog = screen.getByRole('dialog', { name: 'Complete task' })
    const submit = within(dialog).getByRole('button', { name: 'Complete' })
    expect(submit.hasAttribute('disabled')).toBe(true)

    await fireEvent.input(within(dialog).getByLabelText(/Completion note/), {
      target: {
        value: 'Completed the example behavior and confirmed the requested interaction works.',
      },
    })
    await fireEvent.input(within(dialog).getByLabelText('Evidence value'), {
      target: { value: 'go test ./internal/example' },
    })
    await fireEvent.click(submit)

    expect(onCloseIssue).toHaveBeenCalledWith({
      reason: 'done',
      message: 'Completed the example behavior and confirmed the requested interaction works.',
      evidence: [{ type: 'test', command: 'go test ./internal/example' }],
    })
  })

  it('submits an external account for work completed outside a repository', async () => {
    const onCloseIssue = vi.fn(async () => true)

    render(IssueStateDialog, {
      props: { issue: makeIssue(), onCloseIssue, onReopenIssue: vi.fn() },
    })
    await fireEvent.click(screen.getAllByRole('button', { name: 'Complete' })[0]!)
    const dialog = screen.getByRole('dialog', { name: 'Complete task' })
    await fireEvent.change(within(dialog).getByLabelText('Evidence type'), {
      target: { value: 'external' },
    })
    await fireEvent.input(within(dialog).getByLabelText('Evidence value'), {
      target: { value: 'email thread archived; calendar hold sent' },
    })
    await fireEvent.input(within(dialog).getByLabelText(/Completion note/), {
      target: { value: 'Arranged the meeting by email and sent the calendar hold.' },
    })
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Complete' }))

    expect(onCloseIssue).toHaveBeenCalledWith({
      reason: 'done',
      message: 'Arranged the meeting by email and sent the calendar hold.',
      evidence: [{ type: 'external', account: 'email thread archived; calendar hold sent' }],
    })
  })

  it('collects the target reference for duplicate closes', async () => {
    const onCloseIssue = vi.fn(async () => true)

    render(IssueStateDialog, {
      props: { issue: makeIssue(), onCloseIssue, onReopenIssue: vi.fn() },
    })
    await fireEvent.click(screen.getAllByRole('button', { name: 'Complete' })[0]!)
    const dialog = screen.getByRole('dialog', { name: 'Complete task' })
    await fireEvent.click(within(dialog).getByRole('radio', { name: /Duplicate/ }))
    await fireEvent.input(within(dialog).getByLabelText('Target issue'), {
      target: { value: 'example-project#d4ex' },
    })
    await fireEvent.input(within(dialog).getByLabelText(/Completion note/), {
      target: { value: 'The same work is already tracked by the referenced example issue.' },
    })
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Complete' }))

    expect(onCloseIssue).toHaveBeenCalledWith({
      reason: 'duplicate',
      message: 'The same work is already tracked by the referenced example issue.',
      evidence: [{ type: 'duplicate-of', issue_ref: 'example-project#d4ex' }],
    })
  })

  it('reopens a closed task', async () => {
    const onReopenIssue = vi.fn()

    render(IssueStateDialog, {
      props: {
        issue: makeIssue('closed'),
        onCloseIssue: vi.fn(),
        onReopenIssue,
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Reopen' }))

    expect(onReopenIssue).toHaveBeenCalledTimes(1)
  })
})
