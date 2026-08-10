import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, test, vi } from 'vitest'
import type { KataRecurrence } from '../lib/kata/types'
import RecurrencePanel from './RecurrencePanel.svelte'

afterEach(cleanup)

const base: KataRecurrence = {
  id: 1,
  uid: 'rec-1',
  project_id: 2,
  rrule: 'FREQ=WEEKLY;INTERVAL=1;COUNT=2',
  dtstart: '2026-05-20',
  timezone: 'America/Chicago',
  template_title: 'Weekly review',
  template_body: 'Body',
  template_owner: 'user-a',
  template_priority: 2,
  template_labels: ['routine', 'weekly'],
  template_metadata: {},
  next_occurrence_key: '2026-05-27',
  last_materialized_uid: 'issue-last',
  author: 'user-a',
  revision: 1,
  created_at: '2026-05-15T12:00:00.000Z',
  updated_at: '2026-05-15T12:00:00.000Z',
}

describe('RecurrencePanel', () => {
  test('renders recurrence details without controls in read-only mode', () => {
    render(RecurrencePanel, {
      props: {
        recurrences: [base],
        readOnly: true,
      },
    })

    expect(screen.getByText('Weekly review')).toBeTruthy()
    expect(screen.getByText('Weekly, 2 times')).toBeTruthy()
    expect(screen.queryByRole('button')).toBeNull()
  })

  test('uses formatRRule for the summary (Weekly, 2 times)', () => {
    render(RecurrencePanel, {
      props: {
        recurrences: [base],
        canCreate: false,
        onCreate: vi.fn(),
        onEdit: vi.fn(),
        onDelete: vi.fn(),
      },
    })
    expect(screen.getByText('Weekly, 2 times')).toBeTruthy()
  })

  test("empty state still shows '+ New' button", () => {
    const onCreate = vi.fn()
    render(RecurrencePanel, {
      props: {
        recurrences: [],
        canCreate: true,
        onCreate,
        onEdit: vi.fn(),
        onDelete: vi.fn(),
      },
    })
    expect(screen.getByText('No recurring tasks')).toBeTruthy()
    expect(screen.getByRole('button', { name: /\+ New/i })).toBeTruthy()
  })

  test("clicking '+ New' fires onCreate", async () => {
    const onCreate = vi.fn()
    render(RecurrencePanel, {
      props: {
        recurrences: [base],
        canCreate: true,
        onCreate,
        onEdit: vi.fn(),
        onDelete: vi.fn(),
      },
    })
    await fireEvent.click(screen.getByRole('button', { name: /\+ New/i }))
    expect(onCreate).toHaveBeenCalledTimes(1)
  })

  test("disables '+ New' when the selected issue cannot create a recurrence", () => {
    render(RecurrencePanel, {
      props: {
        recurrences: [base],
        canCreate: false,
        onCreate: vi.fn(),
        onEdit: vi.fn(),
        onDelete: vi.fn(),
      },
    })

    expect((screen.getByRole('button', { name: /\+ New/i }) as HTMLButtonElement).disabled).toBe(
      true,
    )
  })

  test('clicking a row fires onEdit with the recurrence', async () => {
    const onEdit = vi.fn()
    render(RecurrencePanel, {
      props: {
        recurrences: [base],
        canCreate: false,
        onCreate: vi.fn(),
        onEdit,
        onDelete: vi.fn(),
      },
    })
    await fireEvent.click(screen.getByRole('button', { name: /Weekly review/ }))
    expect(onEdit).toHaveBeenCalledWith(base)
  })

  test('clicking the delete icon fires onDelete with the recurrence', async () => {
    const onDelete = vi.fn()
    render(RecurrencePanel, {
      props: {
        recurrences: [base],
        canCreate: false,
        onCreate: vi.fn(),
        onEdit: vi.fn(),
        onDelete,
      },
    })
    await fireEvent.click(screen.getByRole('button', { name: /Delete recurrence/i }))
    expect(onDelete).toHaveBeenCalledWith(base)
  })
})
