import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte'
import { afterEach, expect, it, vi } from 'vitest'
import ProjectRecurrences from './ProjectRecurrences.svelte'
import { listRecurrences } from '../lib/api/generated/default/default'
import { recurrence as sample } from '../lib/recurrence/__fixtures__/recurrence'

const recurrence = {
  ...sample,
  id: 1,
  uid: 'r1',
  rrule: 'FREQ=WEEKLY',
  dtstart: '2026-11-22',
  timezone: 'UTC',
  template_title: 'Weekly task',
  template_body: '',
  template_labels: [],
  template_metadata: {},
  revision: 1,
  next_occurrence_key: '2026-11-29',
}

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
    expect(screen.queryByRole('button', { name: /repeating task/ })).toBeNull()
    if (status === 403) expect(await screen.findByRole('alert')).toBeTruthy()
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

it('reloads the rule after an edit conflict', async () => {
  vi.mocked(listRecurrences)
    .mockResolvedValueOnce({
      status: 200,
      headers: new Headers(),
      data: { recurrences: [recurrence] },
    })
    .mockResolvedValueOnce({
      status: 200,
      headers: new Headers(),
      data: { recurrences: [{ ...recurrence, revision: 2, template_title: 'Updated task' }] },
    })
  render(ProjectRecurrences, {
    props: {
      projectID: 1,
      actor: 'user-a',
      onCreate: vi.fn(),
      onPatch: vi.fn().mockRejectedValue({ status: 412 }),
      onDelete: vi.fn(),
    },
  })
  await fireEvent.click(await screen.findByRole('button', { name: /1 repeating task/ }))
  await fireEvent.click(screen.getByRole('button', { name: 'Edit' }))
  const dialog = screen.getByRole('dialog')
  await fireEvent.input(within(dialog).getByLabelText('Title'), { target: { value: 'My edit' } })
  await fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect((within(dialog).getByLabelText('Title') as HTMLInputElement).value).toBe('Updated task'),
  )
})

it('keeps the loaded rules available after a failed refresh and retries', async () => {
  vi.mocked(listRecurrences)
    .mockResolvedValueOnce({
      status: 200,
      headers: new Headers(),
      data: { recurrences: [recurrence] },
    })
    .mockResolvedValueOnce({
      status: 503,
      headers: new Headers(),
      data: { error: { code: 'unavailable', message: 'Unavailable' } },
    } as Awaited<ReturnType<typeof listRecurrences>>)
    .mockResolvedValueOnce({
      status: 200,
      headers: new Headers(),
      data: { recurrences: [recurrence] },
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
  await screen.findByRole('button', { name: /1 repeating task/ })
  await view.rerender({ refreshGeneration: 1 })
  expect(await screen.findByRole('alert')).toBeTruthy()
  expect(screen.getByRole('button', { name: /1 repeating task/ })).toBeTruthy()
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
  await waitFor(() => expect(screen.queryByRole('alert')).toBeNull())
})

it('uses the selected task snapshot until it becomes unavailable', async () => {
  vi.mocked(listRecurrences).mockResolvedValue({
    status: 200,
    headers: new Headers(),
    data: { recurrences: [recurrence] },
  })
  const view = render(ProjectRecurrences, {
    props: {
      projectID: 1,
      actor: 'user-a',
      snapshotRecurrences: [recurrence],
      onCreate: vi.fn(),
      onPatch: vi.fn(),
      onDelete: vi.fn(),
    },
  })
  expect(await screen.findByRole('button', { name: /1 repeating task/ })).toBeTruthy()
  expect(listRecurrences).not.toHaveBeenCalled()
  await view.rerender({ snapshotRecurrences: undefined, projectID: 2 })
  await waitFor(() => expect(listRecurrences).toHaveBeenCalledWith({ projectId: 2 }))
})

it('preserves the draft when a conflict reload fails despite a stale response', async () => {
  vi.mocked(listRecurrences)
    .mockResolvedValueOnce({
      status: 200,
      headers: new Headers(),
      data: { recurrences: [recurrence] },
    })
    .mockResolvedValueOnce({
      status: 503,
      headers: new Headers(),
      data: { error: { code: 'unavailable', message: 'Unavailable' } },
    } as Awaited<ReturnType<typeof listRecurrences>>)
  const error = Object.assign(new Error('conflict'), {
    status: 412,
    response: { recurrence, etag: '"rev-1"' },
  })
  render(ProjectRecurrences, {
    props: {
      projectID: 1,
      actor: 'user-a',
      onCreate: vi.fn(),
      onPatch: vi.fn().mockRejectedValue(error),
      onDelete: vi.fn(),
    },
  })
  await fireEvent.click(await screen.findByRole('button', { name: /1 repeating task/ }))
  await fireEvent.click(screen.getByRole('button', { name: 'Edit' }))
  const dialog = screen.getByRole('dialog')
  await fireEvent.input(within(dialog).getByLabelText('Title'), { target: { value: 'My draft' } })
  await fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }))
  expect(await within(dialog).findByText(/Could not load the latest version/)).toBeTruthy()
  expect((within(dialog).getByLabelText('Title') as HTMLInputElement).value).toBe('My draft')
})

it('uses the refreshed snapshot on conflict without listing again', async () => {
  const updated = { ...recurrence, revision: 2, template_title: 'Server update' }
  const conflict = Object.assign(new Error('conflict'), {
    status: 412,
    response: { recurrence: updated, etag: '"rev-2"' },
  })
  render(ProjectRecurrences, {
    props: {
      projectID: 1,
      actor: 'user-a',
      snapshotRecurrences: [recurrence],
      onCreate: vi.fn(),
      onPatch: vi.fn().mockRejectedValue(conflict),
      onDelete: vi.fn(),
    },
  })
  await fireEvent.click(await screen.findByRole('button', { name: /1 repeating task/ }))
  await fireEvent.click(screen.getByRole('button', { name: 'Edit' }))
  const dialog = screen.getByRole('dialog')
  await fireEvent.input(within(dialog).getByLabelText('Title'), { target: { value: 'My draft' } })
  await fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect((within(dialog).getByLabelText('Title') as HTMLInputElement).value).toBe(
      'Server update',
    ),
  )
  expect(listRecurrences).not.toHaveBeenCalled()
})

it('explains a rejected stop action and retries with the refreshed rule', async () => {
  vi.mocked(listRecurrences).mockResolvedValue({
    status: 200,
    headers: new Headers(),
    data: { recurrences: [recurrence] },
  })
  const onDelete = vi.fn().mockResolvedValueOnce(false).mockResolvedValueOnce(true)
  const view = render(ProjectRecurrences, {
    props: {
      projectID: 1,
      actor: 'user-a',
      onCreate: vi.fn(),
      onPatch: vi.fn(),
      onDelete,
      mutationMessage: 'Rule changed on the server.',
    },
  })
  await fireEvent.click(await screen.findByRole('button', { name: /1 repeating task/ }))
  await fireEvent.click(screen.getByRole('button', { name: 'Stop repeating…' }))
  const dialog = screen.getByRole('dialog', { name: 'Stop repeating' })
  await fireEvent.click(within(dialog).getByRole('button', { name: 'Stop repeating' }))
  expect(within(dialog).getByRole('alert').textContent).toContain('Rule changed on the server.')
  await view.rerender({ snapshotRecurrences: [{ ...recurrence, revision: 2 }] })
  await fireEvent.click(within(dialog).getByRole('button', { name: 'Stop repeating' }))
  expect(onDelete).toHaveBeenLastCalledWith(expect.objectContaining({ revision: 2 }))
})

it.each(['edit', 'stop'])(
  'lists rules once per accepted snapshot revision after %s',
  async (action) => {
    vi.mocked(listRecurrences).mockResolvedValue({
      status: 200,
      headers: new Headers(),
      data: { recurrences: [recurrence] },
    })
    const onPatch = vi.fn(async () => {
      await view.rerender({ refreshGeneration: 1 })
    })
    const onDelete = vi.fn(async () => {
      await view.rerender({ refreshGeneration: 1 })
      return true
    })
    const view = render(ProjectRecurrences, {
      props: { projectID: 1, actor: 'user-a', onCreate: vi.fn(), onPatch, onDelete },
    })
    await fireEvent.click(await screen.findByRole('button', { name: /1 repeating task/ }))
    if (action === 'edit') {
      await fireEvent.click(screen.getByRole('button', { name: 'Edit' }))
      const dialog = screen.getByRole('dialog', { name: 'Edit repeat' })
      await fireEvent.input(within(dialog).getByLabelText('Title'), {
        target: { value: 'Changed' },
      })
      await fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }))
    } else {
      await fireEvent.click(screen.getByRole('button', { name: 'Stop repeating…' }))
      await fireEvent.click(
        within(screen.getByRole('dialog', { name: 'Stop repeating' })).getByRole('button', {
          name: 'Stop repeating',
        }),
      )
    }
    await waitFor(() => expect(listRecurrences).toHaveBeenCalledTimes(2))
  },
)
