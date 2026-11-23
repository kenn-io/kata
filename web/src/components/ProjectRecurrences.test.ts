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

it.each([200, 403])(
  'hides the count when the list is empty or unavailable (%i)',
  async (status) => {
    vi.mocked(listRecurrences).mockResolvedValue({
      status,
      headers: new Headers(),
      data:
        status === 200
          ? { recurrences: [] }
          : { error: { code: 'forbidden', message: 'Forbidden' } },
    } as Awaited<ReturnType<typeof listRecurrences>>)
    render(ProjectRecurrences, {
      props: {
        projectID: 1,
        actor: 'user-a',
        onCreate: vi.fn(),
        onPatch: vi.fn(),
        onDelete: vi.fn(),
      },
    })
    await waitFor(() => expect(listRecurrences).toHaveBeenCalled())
    if (status === 403) expect(await screen.findByRole('alert')).toBeTruthy()
    else await tick()
    expect(screen.queryByRole('button', { name: /repeating task/ })).toBeNull()
  },
)

it('lists a project rule and opens its stop dialog', async () => {
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
  await view.rerender({ disabled: true })
  expect(screen.queryByRole('button', { name: 'Edit' })).toBeNull()
})

it('refetches on open and after an edit conflict', async () => {
  vi.mocked(listRecurrences).mockResolvedValue({
    status: 200,
    headers: new Headers(),
    data: { recurrences: [recurrence] },
  })
  const onPatch = vi.fn().mockRejectedValue({ status: 412 })
  render(ProjectRecurrences, {
    props: { projectID: 1, actor: 'user-a', onCreate: vi.fn(), onPatch, onDelete: vi.fn() },
  })
  await fireEvent.click(await screen.findByRole('button', { name: /1 repeating task/ }))
  await waitFor(() => expect(listRecurrences).toHaveBeenCalledTimes(2))
  await fireEvent.click(screen.getByRole('button', { name: 'Edit' }))
  const dialog = screen.getByRole('dialog')
  await fireEvent.input(within(dialog).getByLabelText('Title'), { target: { value: 'My edit' } })
  await fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(listRecurrences).toHaveBeenCalledTimes(3))
})

it('refreshes a refused stop before retrying with the current revision', async () => {
  const updated = { ...recurrence, revision: 2 }
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
  const onDelete = vi.fn().mockResolvedValueOnce(false).mockResolvedValueOnce(true)
  render(ProjectRecurrences, {
    props: { projectID: 1, actor: 'user-a', onCreate: vi.fn(), onPatch: vi.fn(), onDelete },
  })
  await fireEvent.click(await screen.findByRole('button', { name: /1 repeating task/ }))
  await fireEvent.click(screen.getByRole('button', { name: 'Stop repeating…' }))
  await fireEvent.click(
    within(screen.getByRole('dialog')).getByRole('button', { name: 'Stop repeating' }),
  )
  await waitFor(() =>
    expect(
      (
        within(screen.getByRole('dialog')).getByRole('button', {
          name: 'Stop repeating',
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(false),
  )
  await fireEvent.click(
    within(screen.getByRole('dialog')).getByRole('button', { name: 'Stop repeating' }),
  )
  await waitFor(() => expect(onDelete).toHaveBeenLastCalledWith(updated))
})
