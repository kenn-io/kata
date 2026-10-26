import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, expect, test, vi } from 'vitest'
import CronPage from './CronPage.svelte'
import { emptyCronState } from '../../lib/cron/controller'
import { defaultCronRoute } from '../../lib/cron/route'
afterEach(cleanup)
test('read-only page uses URL callbacks for tabs, no mutation controls', async () => {
  const navigate = vi.fn()
  render(CronPage, {
    route: defaultCronRoute(),
    state: emptyCronState(),
    onNavigate: navigate,
    onLoadMore: vi.fn(),
    onRefresh: vi.fn(),
    onOpenIssue: vi.fn(),
  })
  expect(screen.getByRole('heading', { name: 'Cron' })).toBeTruthy()
  await fireEvent.click(screen.getByRole('radio', { name: 'Runs' }))
  expect(navigate).toHaveBeenCalledWith(expect.objectContaining({ tab: 'runs' }))
  expect(screen.queryByRole('button', { name: 'New job' })).toBeNull()
})
test('core page omits cost columns and preserves denial/loading distinction', () => {
  const state = emptyCronState()
  state.projects = [
    {
      project: { id: 1, uid: 'p1', name: 'example-project' },
      jobs: [],
      runs: [],
      workflows: [],
      hasRecords: false,
      error: 'Project not readable',
    },
  ]
  render(CronPage, {
    route: { ...defaultCronRoute(), tab: 'runs' },
    state,
    onNavigate: vi.fn(),
    onLoadMore: vi.fn(),
    onRefresh: vi.fn(),
    onOpenIssue: vi.fn(),
  })
  expect(screen.queryByRole('columnheader', { name: 'Cost' })).toBeNull()
  expect(screen.getByText(/Project not readable/)).toBeTruthy()
})

test('run rows use a readable job name from their project catalog', () => {
  const state = emptyCronState()
  state.projects = [
    {
      project: { id: 1, uid: 'p1', name: 'example-project' },
      hasRecords: true,
      jobs: [{ uid: 'job', name: 'Inspect workspace' } as never],
      workflows: [],
      runs: [
        {
          uid: 'r',
          status: 'failed',
          job_uid: 'job',
          created_at: '2026-01-01',
          summary: { version: 1 },
        } as never,
      ],
    },
  ]
  render(CronPage, {
    route: { ...defaultCronRoute(), tab: 'runs' },
    state,
    onNavigate: vi.fn(),
    onLoadMore: vi.fn(),
    onRefresh: vi.fn(),
    onOpenIssue: vi.fn(),
  })
  expect(screen.getByRole('button', { name: 'Inspect workspace' })).toBeTruthy()
})

test('switching from Jobs to Runs clears the job-only never-ran status', async () => {
  const onNavigate = vi.fn()
  render(CronPage, {
    route: { ...defaultCronRoute(), statuses: ['never_ran'] },
    state: emptyCronState(),
    onNavigate,
    onLoadMore: vi.fn(),
    onRefresh: vi.fn(),
    onOpenIssue: vi.fn(),
  })
  await fireEvent.click(screen.getByRole('radio', { name: 'Runs' }))
  expect(onNavigate).toHaveBeenCalledWith(expect.objectContaining({ tab: 'runs', statuses: [] }))
})
