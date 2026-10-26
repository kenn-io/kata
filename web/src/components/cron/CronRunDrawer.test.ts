import { cleanup, render, screen } from '@testing-library/svelte'
import { afterEach, expect, test, vi } from 'vitest'
import CronRunDrawer from './CronRunDrawer.svelte'
import type { CronRun, ShowIssueResponseBody } from '../../lib/cron/types'
afterEach(cleanup)
const run = {
  uid: 'run',
  status: 'failed',
  issue_uid: 'issue',
  created_at: '2026-01-01',
  summary: { version: 1, message: 'Run summary' },
} as CronRun
const props = () => ({ run, onOpenIssue: vi.fn(), onOpenJob: vi.fn() })
test('newest issue comment uses CommentCard author and age', () => {
  const issue = {
    issue: { title: 'Linked issue', short_id: 'abc4' },
    comments: [
      { uid: 'old', author: 'older-author', created_at: '2026-01-01T00:00:00Z', body: 'Old body' },
      {
        uid: 'new',
        author: 'current-author',
        created_at: '2026-01-02T00:00:00Z',
        body: 'Newest body',
      },
    ],
  } as ShowIssueResponseBody
  render(CronRunDrawer, { ...props(), peek: { status: 'ready', issue } })
  expect(screen.getByText('Newest body')).toBeTruthy()
  expect(screen.getByText('current-author')).toBeTruthy()
  expect(screen.queryByText('Old body')).toBeNull()
})
for (const [status, text] of [
  ['loading', 'Loading latest on the issue…'],
  ['no-issue', 'No issue'],
  ['unreadable', 'Issue not readable'],
  ['empty', 'No comments'],
] as const)
  test(`peek ${status} is explicit`, () => {
    render(CronRunDrawer, { ...props(), peek: { status } })
    expect(screen.getByText(text)).toBeTruthy()
  })
test('newest comment compares exact instants rather than fractional timestamp text', () => {
  const issue = {
    issue: { title: 'Linked issue' },
    comments: [
      {
        uid: 'z',
        author: 'older',
        created_at: '2026-10-25T10:00:00.12Z',
        body: 'Earlier fractional body',
      },
      {
        uid: 'a',
        author: 'newer',
        created_at: '2026-10-25T10:00:00.121Z',
        body: 'Latest fractional body',
      },
    ],
  } as ShowIssueResponseBody
  render(CronRunDrawer, { ...props(), peek: { status: 'ready', issue } })
  expect(screen.getByText('Latest fractional body')).toBeTruthy()
  expect(screen.queryByText('Earlier fractional body')).toBeNull()
})
