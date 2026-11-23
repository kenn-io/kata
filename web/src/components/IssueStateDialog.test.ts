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

  it('shows what closing preserves and the refusal', async () => {
    const issue = makeIssue()
    issue.links.push({
      id: 1,
      project_id: 1,
      type: 'blocks',
      from: { uid: 'issue-1', short_id: 'I-1', qualified_id: 'INBOX-1', status: 'open' },
      to: { uid: 'issue-2', short_id: 'I-2', qualified_id: 'INBOX-2', status: 'open' },
      author: 'user-a',
      created_at: '',
    })
    render(IssueStateDialog, {
      props: {
        issue,
        error: 'Open children remain',
        onCloseIssue: vi.fn(async () => false),
        onReopenIssue: vi.fn(),
      },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Complete…' }))
    const dialog = screen.getByRole('dialog', { name: 'Complete task' })
    expect(within(dialog).getByText(/comments, links, and attachments stay/i)).toBeTruthy()
    expect(within(dialog).getByText('1 open task it blocks loses this blocker.')).toBeTruthy()
    await fireEvent.click(within(dialog).getByRole('radio', { name: "Won't do" }))
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Complete' }))
    expect(within(dialog).getByText('Open children remain')).toBeTruthy()
  })

  it('uses shared Kit UI actions', async () => {
    render(IssueStateDialog, {
      props: { issue: makeIssue(), onCloseIssue: vi.fn(), onReopenIssue: vi.fn() },
    })

    const trigger = screen.getByRole('button', { name: 'Complete…' })
    expect(trigger.classList).toContain('kit-button')
    await fireEvent.click(trigger)

    const dialog = screen.getByRole('dialog', { name: 'Complete task' })
    await fireEvent.click(within(dialog).getByRole('radio', { name: /Won't do/ }))
    expect(within(dialog).getByLabelText('Why not (optional)')).toBeTruthy()
    expect(within(dialog).getByRole('button', { name: 'Cancel' }).classList).toContain('kit-button')
    expect(within(dialog).getByRole('button', { name: 'Complete' }).classList).toContain(
      'kit-button',
    )
  })

  it.each([
    {
      name: 'valid note',
      note: 'This task is no longer aligned with the current example workflow and will not be pursued.',
      message:
        'This task is no longer aligned with the current example workflow and will not be pursued.',
      enabled: true,
    },
    {
      name: 'blank note',
      note: '',
      message:
        'Removed from active work through the web issue detail because this task is no longer intended to be completed.',
      enabled: true,
    },
    { name: 'short note', note: 'Not needed', message: '', enabled: false },
  ])("handles Won't do with a $name", async ({ note, message, enabled }) => {
    const onCloseIssue = vi.fn(async () => true)
    render(IssueStateDialog, {
      props: { issue: makeIssue(), onCloseIssue, onReopenIssue: vi.fn() },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Complete…' }))
    const dialog = screen.getByRole('dialog', { name: 'Complete task' })
    await fireEvent.click(within(dialog).getByRole('radio', { name: /Won't do/ }))
    await fireEvent.input(
      within(dialog).getByRole('textbox', { name: /What was done|Why not|Note/ }),
      {
        target: { value: note },
      },
    )
    const submit = within(dialog).getByRole('button', { name: 'Complete' })
    expect(submit.hasAttribute('disabled')).toBe(!enabled)
    await fireEvent.click(submit)
    if (enabled)
      expect(onCloseIssue).toHaveBeenCalledWith({ reason: 'wontfix', message, evidence: [] })
    else expect(onCloseIssue).not.toHaveBeenCalled()
  })

  it('collects completion evidence before enabling a done close', async () => {
    const onCloseIssue = vi.fn(async () => true)

    render(IssueStateDialog, {
      props: { issue: makeIssue(), onCloseIssue, onReopenIssue: vi.fn() },
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Complete…' }))
    const dialog = screen.getByRole('dialog', { name: 'Complete task' })
    const submit = within(dialog).getByRole('button', { name: 'Complete' })
    expect(submit.hasAttribute('disabled')).toBe(true)

    await fireEvent.input(
      within(dialog).getByRole('textbox', { name: /What was done|Why not|Note/ }),
      {
        target: {
          value: 'Completed the example behavior and confirmed the requested interaction works.',
        },
      },
    )
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
    await fireEvent.click(screen.getByRole('button', { name: 'Complete…' }))
    const dialog = screen.getByRole('dialog', { name: 'Complete task' })
    await fireEvent.change(within(dialog).getByLabelText('Evidence type'), {
      target: { value: 'external' },
    })
    await fireEvent.input(within(dialog).getByLabelText('Evidence value'), {
      target: { value: 'email thread archived; calendar hold sent' },
    })
    await fireEvent.input(
      within(dialog).getByRole('textbox', { name: /What was done|Why not|Note/ }),
      {
        target: { value: 'Arranged the meeting by email and sent the calendar hold.' },
      },
    )
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
    await fireEvent.click(screen.getByRole('button', { name: 'Complete…' }))
    const dialog = screen.getByRole('dialog', { name: 'Complete task' })
    await fireEvent.click(within(dialog).getByRole('radio', { name: /Duplicate/ }))
    await fireEvent.input(within(dialog).getByLabelText('Target issue'), {
      target: { value: 'example-project#d4ex' },
    })
    await fireEvent.input(
      within(dialog).getByRole('textbox', { name: /What was done|Why not|Note/ }),
      {
        target: { value: 'The same work is already tracked by the referenced example issue.' },
      },
    )
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
