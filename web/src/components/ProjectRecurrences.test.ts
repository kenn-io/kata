import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte'
import { afterEach, expect, it, vi } from 'vitest'
import ProjectRecurrences from './ProjectRecurrences.svelte'
import { listRecurrences } from '../lib/api/generated/default/default'
import type { KataRecurrence } from '../lib/kata/types'
import { tick } from 'svelte'

const recurrence = {
  id: 1,
  uid: 'r1',
  project_id: 1,
  rrule: 'FREQ=WEEKLY',
  dtstart: '2026-11-22',
  timezone: 'UTC',
  template_title: 'Weekly task',
  template_body: '',
  template_labels: [],
  template_metadata: {},
  next_occurrence_key: '2026-11-29',
  author: 'user-a',
  revision: 1,
  created_at: '2026-11-22T12:00:00.000Z',
  updated_at: '2026-11-22T12:00:00.000Z',
} satisfies KataRecurrence

vi.mock('../lib/api/generated/default/default', () => ({ listRecurrences: vi.fn() }))
afterEach(() => {
  cleanup()
  vi.resetAllMocks()
})

function renderList(props: Partial<{ onPatch: unknown; onDelete: unknown }> = {}) {
  return render(ProjectRecurrences, {
    props: {
      projectID: 1,
      actor: 'user-a',
      onCreate: vi.fn(),
      onPatch: vi.fn(),
      onDelete: vi.fn(),
      ...props,
    } as never,
  })
}

it('hides the count when the project has no rules', async () => {
  vi.mocked(listRecurrences).mockResolvedValue({
    status: 200,
    headers: new Headers(),
    data: { recurrences: [] },
  })
  renderList()
  await waitFor(() => expect(listRecurrences).toHaveBeenCalled())
  await tick()
  expect(screen.queryByRole('alert')).toBeNull()
  expect(screen.queryByRole('button', { name: /repeating task/ })).toBeNull()
})

it('offers a retry instead of the count when the list is unavailable', async () => {
  vi.mocked(listRecurrences).mockResolvedValue({
    status: 403,
    headers: new Headers(),
    data: { error: { code: 'forbidden', message: 'Forbidden' } },
  } as Awaited<ReturnType<typeof listRecurrences>>)
  renderList()
  const alert = await screen.findByRole('alert')
  expect(within(alert).getByRole('button', { name: 'Retry' })).toBeTruthy()
  expect(screen.queryByRole('button', { name: /repeating task/ })).toBeNull()
})

it('lists a rule, opens its stop dialog, and responds to authority changes', async () => {
  vi.mocked(listRecurrences).mockResolvedValue({
    status: 200,
    headers: new Headers(),
    data: {
      recurrences: [recurrence],
    },
  })
  const view = render(ProjectRecurrences, {
    props: {
      projectID: 1,
      actor: 'user-a',
      onCreate: vi.fn(),
      onPatch: vi.fn(),
      onDelete: vi.fn(),
    },
  })
  await fireEvent.click(await screen.findByRole('button', { name: /1 repeating task/ }))
  expect(screen.getByText('Weekly task')).toBeTruthy()
  await fireEvent.click(screen.getByRole('button', { name: /Stop repeating/ }))
  expect(screen.getByRole('dialog', { name: 'Stop repeating' })).toBeTruthy()
  await view.rerender({ draftFenceGeneration: 1 })
  expect(screen.queryByRole('dialog', { name: 'Stop repeating' })).toBeNull()
  expect(screen.getByRole('button', { name: 'Edit' })).toBeTruthy()
  await view.rerender({ disabled: true })
  expect(screen.queryByRole('button', { name: 'Edit' })).toBeNull()
})

it("shows each rule's latest materialized task", async () => {
  vi.mocked(listRecurrences).mockResolvedValue({
    status: 200,
    headers: new Headers(),
    data: {
      recurrences: [
        { ...recurrence, last_materialized_uid: 'issue-1' },
        {
          ...recurrence,
          id: 2,
          uid: 'r2',
          template_title: 'Monthly task',
          last_materialized_uid: 'issue-2',
        },
      ],
    },
  })
  renderList()
  await fireEvent.click(await screen.findByRole('button', { name: /2 repeating tasks/ }))
  expect(screen.getByText('Weekly task').parentElement?.textContent).toContain('issue-1')
  expect(screen.getByText('Monthly task').parentElement?.textContent).toContain('issue-2')
})

it('refreshes the list after an edit conflict', async () => {
  const theirs = { ...recurrence, template_title: 'Their title', revision: 2 }
  vi.mocked(listRecurrences)
    .mockResolvedValueOnce({
      status: 200,
      headers: new Headers(),
      data: { recurrences: [recurrence] },
    })
    .mockResolvedValueOnce({
      status: 200,
      headers: new Headers(),
      data: { recurrences: [recurrence] },
    })
    .mockResolvedValue({ status: 200, headers: new Headers(), data: { recurrences: [theirs] } })
  const onPatch = vi.fn().mockRejectedValue(Object.assign(new Error('Conflict'), { status: 412 }))
  renderList({ onPatch })
  await fireEvent.click(await screen.findByRole('button', { name: /1 repeating task/ }))
  await waitFor(() => expect(listRecurrences).toHaveBeenCalledTimes(2))
  await fireEvent.click(screen.getByRole('button', { name: 'Edit' }))
  const dialog = screen.getByRole('dialog')
  await fireEvent.input(within(dialog).getByLabelText('Title'), { target: { value: 'My edit' } })
  await fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(listRecurrences).toHaveBeenCalledTimes(3))
  await within(dialog).findByRole('alert')
  await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
  await fireEvent.click(screen.getByRole('button', { name: 'Edit' }))
  expect(
    (within(screen.getByRole('dialog')).getByLabelText('Title') as HTMLInputElement).value,
  ).toBe('Their title')
})

it('retries a refused stop with the current revision', async () => {
  const updated = {
    ...recurrence,
    template_title: 'Changed rule',
    rrule: 'FREQ=DAILY',
    revision: 2,
  }
  vi.mocked(listRecurrences)
    .mockResolvedValueOnce({
      status: 200,
      headers: new Headers(),
      data: { recurrences: [recurrence] },
    })
    .mockResolvedValueOnce({
      status: 200,
      headers: new Headers(),
      data: { recurrences: [recurrence] },
    })
    .mockResolvedValue({ status: 200, headers: new Headers(), data: { recurrences: [updated] } })
  const onDelete = vi
    .fn()
    .mockRejectedValueOnce(
      Object.assign(
        new Error('revision_conflict: recurrence revision is 2. Your draft was preserved.'),
        { status: 412 },
      ),
    )
    .mockResolvedValueOnce(undefined)
  render(ProjectRecurrences, {
    props: { projectID: 1, actor: 'user-a', onCreate: vi.fn(), onPatch: vi.fn(), onDelete },
  })
  await fireEvent.click(await screen.findByRole('button', { name: /1 repeating task/ }))
  await fireEvent.click(screen.getByRole('button', { name: 'Stop repeating…' }))
  await fireEvent.click(
    within(screen.getByRole('dialog')).getByRole('button', { name: 'Stop repeating' }),
  )
  expect(
    await within(screen.getByRole('dialog')).findByText(
      'This rule changed elsewhere. Review the current rule before stopping it.',
    ),
  ).toBeTruthy()
  const dialog = screen.getByRole('dialog')
  const stop = within(dialog).getByRole('button', { name: 'Stop repeating' }) as HTMLButtonElement
  await waitFor(() => expect(within(dialog).getByText('Changed rule')).toBeTruthy())
  expect(within(dialog).getByText(/Daily · Next/)).toBeTruthy()
  expect(stop.disabled).toBe(true)
  await fireEvent.click(stop)
  expect(onDelete).toHaveBeenCalledTimes(1)
  await fireEvent.click(within(dialog).getByRole('button', { name: 'Review updated rule' }))
  expect(within(dialog).queryByRole('button', { name: 'Review updated rule' })).toBeNull()
  expect(stop.disabled).toBe(false)
  await fireEvent.click(stop)
  await waitFor(() => expect(onDelete).toHaveBeenLastCalledWith(updated))
  await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
})
