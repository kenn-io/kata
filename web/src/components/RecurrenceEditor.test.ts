import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, test, vi } from 'vitest'
import RecurrenceEditor from './RecurrenceEditor.svelte'
import RecurrenceEditorDialog from './RecurrenceEditorDialog.svelte'
import RecurrenceDeleteDialog from './RecurrenceDeleteDialog.svelte'
import { ADVANCED_LAST_WEEKDAY } from '../lib/recurrence/__fixtures__/advancedRules'
import { MONTH_LONG } from '../lib/recurrence/rrule'
import type { KataRecurrence } from '../lib/kata/types'

afterEach(cleanup)

const createMode = { kind: 'create', projectID: 1, initialIssueRef: 'abc4' } as const

const sample: KataRecurrence = {
  id: 7,
  uid: 'rec-uid',
  project_id: 1,
  rrule: 'FREQ=WEEKLY;INTERVAL=1;BYDAY=MO',
  dtstart: '2026-05-20',
  timezone: 'America/Chicago',
  template_title: 'Weekly review',
  template_body: 'Body',
  template_owner: 'user-a',
  template_priority: 2,
  template_labels: ['routine'],
  template_metadata: { k: 'v' },
  next_occurrence_key: '2026-05-27',
  last_materialized_uid: undefined,
  author: 'user-a',
  revision: 1,
  created_at: '2026-05-15T12:00:00.000Z',
  updated_at: '2026-05-15T12:00:00.000Z',
}
const editMode = { kind: 'edit' as const, recurrence: sample, etag: '"rev-1"' }

async function chooseDropdown(title: string, option: string): Promise<void> {
  await fireEvent.click(screen.getByRole('combobox', { name: new RegExp(`^${title}:`) }))
  await fireEvent.click(screen.getByRole('option', { name: option }))
}

async function pickStartDate(label: string, nextMonths = 0): Promise<void> {
  await fireEvent.click(screen.getByRole('button', { name: /^Start date:/ }))
  for (let i = 0; i < nextMonths; i += 1) {
    await fireEvent.click(screen.getByRole('button', { name: 'Next month' }))
  }
  await fireEvent.click(screen.getByRole('button', { name: label }))
}

function expectDropdown(title: string, value: string): void {
  expect(screen.getByRole('combobox', { name: `${title}: ${value}` })).toBeTruthy()
}

describe('RecurrenceEditor — Common-mode rendering', () => {
  test('create mode renders frequency, interval, end, dtstart, timezone', () => {
    render(RecurrenceEditor, {
      props: {
        mode: createMode,
        actor: 'user-a',
        onCreate: vi.fn(),
        onPatch: vi.fn(),
        onSaved: vi.fn(),
      },
    })
    expect(screen.getByLabelText(/Frequency/i)).toBeTruthy()
    expect(screen.getByLabelText(/Every/i)).toBeTruthy()
    expect(screen.getByLabelText(/Start date/i)).toBeTruthy()
    expect(screen.getByLabelText(/Timezone/i)).toBeTruthy()
    expect(screen.getByText(/Never/i)).toBeTruthy()
  })

  test('default frequency is Weekly with INTERVAL=1 and one weekday selected', () => {
    render(RecurrenceEditor, {
      props: {
        mode: createMode,
        actor: 'user-a',
        onCreate: vi.fn(),
        onPatch: vi.fn(),
        onSaved: vi.fn(),
      },
    })
    expectDropdown('Frequency', 'Weekly')
    const interval = screen.getByLabelText(/Every/i) as HTMLInputElement
    expect(interval.value).toBe('1')
    // Exactly one weekday chip is pre-selected (matches dtstart's weekday)
    const pressed = screen
      .getAllByRole('button', { pressed: true })
      .filter((b) => /^(Mon|Tue|Wed|Thu|Fri|Sat|Sun)$/.test(b.textContent ?? ''))
    expect(pressed).toHaveLength(1)
  })

  test('switching to Monthly shows day-of-month and nth-weekday radio', async () => {
    render(RecurrenceEditor, {
      props: {
        mode: createMode,
        actor: 'user-a',
        onCreate: vi.fn(),
        onPatch: vi.fn(),
        onSaved: vi.fn(),
      },
    })
    await chooseDropdown('Frequency', 'Monthly')
    expect(screen.getByLabelText(/On day/i)).toBeTruthy()
    expect(screen.getByLabelText(/On the/i)).toBeTruthy()
  })

  test('switching to Yearly shows month select and optional day-in-month controls', async () => {
    render(RecurrenceEditor, {
      props: {
        mode: createMode,
        actor: 'user-a',
        onCreate: vi.fn(),
        onPatch: vi.fn(),
        onSaved: vi.fn(),
      },
    })
    await chooseDropdown('Frequency', 'Yearly')
    expectDropdown('Month', MONTH_LONG[new Date().getMonth()]!)
  })

  test('Monthly last-day → Yearly On-day: yearly day input is enabled (no leaked Monthly state)', async () => {
    // Regression: the Monthly "Last day of month" checkbox
    // shares the dayInMonthLastDay flag with the Yearly day field, and
    // historically the Yearly day input was wired with
    // disabled={... || dayInMonthLastDay}, so switching freq from
    // Monthly→Yearly left it disabled. The fix in buildCommonRuleFromForm
    // is to ignore dayInMonthLastDay for the Yearly path; this test
    // pins the editor behavior end-to-end via the rendered inputs.
    render(RecurrenceEditor, {
      props: {
        mode: createMode,
        actor: 'user-a',
        onCreate: vi.fn(),
        onPatch: vi.fn(),
        onSaved: vi.fn(),
      },
    })
    await chooseDropdown('Frequency', 'Monthly')
    const lastDay = screen.getByLabelText(/Last day of month/i) as HTMLInputElement
    await fireEvent.click(lastDay)
    expect(lastDay.checked).toBe(true)
    await chooseDropdown('Frequency', 'Yearly')
    // Click the "On day" radio for yearly mode so the day input becomes
    // the user-active control (the editor leaves yearlyDayMode at
    // "none" by default).
    const onDayRadios = screen.getAllByLabelText(/On day/i)
    const yearlyOnDay = onDayRadios.find(
      (el) => (el as HTMLInputElement).getAttribute('name') === 'yearlyDayMode',
    ) as HTMLInputElement | undefined
    expect(yearlyOnDay).toBeTruthy()
    await fireEvent.click(yearlyOnDay!)
    // The number input in the yearly "On day" row should be enabled,
    // not blocked by the leaked Monthly lastDay flag.
    const dayInputs = screen.getAllByRole('spinbutton') as HTMLInputElement[]
    const yearlyDayInput = dayInputs.find((el) => el.max === '31' && !el.disabled)
    expect(yearlyDayInput).toBeTruthy()
  })

  test('End=After N reveals a numeric input', async () => {
    render(RecurrenceEditor, {
      props: {
        mode: createMode,
        actor: 'user-a',
        onCreate: vi.fn(),
        onPatch: vi.fn(),
        onSaved: vi.fn(),
      },
    })
    await fireEvent.click(screen.getByLabelText(/After/i))
    expect(screen.getByLabelText(/occurrences/i)).toBeTruthy()
  })
})

describe('RecurrenceEditor — delete', () => {
  test('confirms recoverable recurrence deletion and respects authority fencing', async () => {
    const onConfirm = vi.fn()
    const view = render(RecurrenceDeleteDialog, {
      props: {
        open: true,
        recurrence: sample,
        onConfirm,
        onCancel: vi.fn(),
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: 'Delete' }))
    expect(onConfirm).toHaveBeenCalledOnce()

    await view.rerender({ disabled: true })
    const deleteButton = screen.getByRole('button', { name: 'Delete' }) as HTMLButtonElement
    expect(deleteButton.disabled).toBe(true)
  })
})

describe('RecurrenceEditor — Template block + summary', () => {
  test('renders title (required), body, owner, priority, labels, metadata', () => {
    render(RecurrenceEditor, {
      props: {
        mode: createMode,
        actor: 'user-a',
        onCreate: vi.fn(),
        onPatch: vi.fn(),
        onSaved: vi.fn(),
      },
    })
    expect(screen.getByLabelText(/Title/i)).toBeTruthy()
    expect((screen.getByLabelText(/Title/i) as HTMLInputElement).required).toBe(true)
    expect(screen.getByLabelText(/Body/i)).toBeTruthy()
    expect(screen.getByLabelText(/Owner/i)).toBeTruthy()
    expect(screen.getByLabelText(/Priority/i)).toBeTruthy()
    expect(screen.getByLabelText(/Labels/i)).toBeTruthy()
    expect(screen.getByLabelText(/Metadata/i)).toBeTruthy()
  })

  test('compact summary updates as the form changes', async () => {
    render(RecurrenceEditor, {
      props: {
        mode: createMode,
        actor: 'user-a',
        onCreate: vi.fn(),
        onPatch: vi.fn(),
        onSaved: vi.fn(),
      },
    })
    const summary = screen.getByTestId('recurrence-summary')
    expect(summary.textContent ?? '').toMatch(/^Weekly on /)
    await chooseDropdown('Frequency', 'Daily')
    expect(summary.textContent).toBe('Daily')
  })

  test('compact summary reflects Advanced-mode RRULE', async () => {
    render(RecurrenceEditor, {
      props: {
        mode: createMode,
        actor: 'user-a',
        onCreate: vi.fn(),
        onPatch: vi.fn(),
        onSaved: vi.fn(),
      },
    })
    await fireEvent.click(screen.getByRole('button', { name: /Advanced/i }))
    const textarea = screen.getByLabelText(/RRULE/i) as HTMLTextAreaElement
    await fireEvent.input(textarea, { target: { value: 'FREQ=DAILY;INTERVAL=2' } })
    const summary = screen.getByTestId('recurrence-summary')
    expect(summary.textContent).toBe('Every 2 days')
  })

  test('compact summary is hidden when Advanced-mode RRULE is invalid', async () => {
    render(RecurrenceEditor, {
      props: {
        mode: createMode,
        actor: 'user-a',
        onCreate: vi.fn(),
        onPatch: vi.fn(),
        onSaved: vi.fn(),
      },
    })
    await fireEvent.click(screen.getByRole('button', { name: /Advanced/i }))
    const textarea = screen.getByLabelText(/RRULE/i) as HTMLTextAreaElement
    await fireEvent.input(textarea, { target: { value: 'FREQ=FORTNIGHTLY' } })
    // The empty styled block was rendering as a visible blank stripe
    // above the parser error; the editor should drop the element when
    // there's nothing to show.
    expect(screen.queryByTestId('recurrence-summary')).toBeNull()
  })
})

describe('RecurrenceEditor — mode toggle', () => {
  test('toggle to Advanced fills textarea with canonical serialization of current form', async () => {
    render(RecurrenceEditor, {
      props: {
        mode: createMode,
        actor: 'user-a',
        onCreate: vi.fn(),
        onPatch: vi.fn(),
        onSaved: vi.fn(),
      },
    })
    await fireEvent.click(screen.getByRole('button', { name: /Advanced/i }))
    const textarea = screen.getByLabelText(/RRULE/i) as HTMLTextAreaElement
    expect(textarea.value).toMatch(/^FREQ=WEEKLY;INTERVAL=1;BYDAY=/)
  })

  test('Advanced → Common: common rule switches modes and populates the form', async () => {
    render(RecurrenceEditor, {
      props: {
        mode: createMode,
        actor: 'user-a',
        onCreate: vi.fn(),
        onPatch: vi.fn(),
        onSaved: vi.fn(),
      },
    })
    await fireEvent.click(screen.getByRole('button', { name: /Advanced/i }))
    const textarea = screen.getByLabelText(/RRULE/i) as HTMLTextAreaElement
    await fireEvent.input(textarea, { target: { value: 'FREQ=DAILY;INTERVAL=2' } })
    await fireEvent.click(screen.getByRole('button', { name: /Use Common mode/i }))
    expectDropdown('Frequency', 'Daily')
    expect((screen.getByLabelText(/Every/i) as HTMLInputElement).value).toBe('2')
  })

  test('Advanced → Common: advanced-only rule stays in Advanced with reason text', async () => {
    render(RecurrenceEditor, {
      props: {
        mode: createMode,
        actor: 'user-a',
        onCreate: vi.fn(),
        onPatch: vi.fn(),
        onSaved: vi.fn(),
      },
    })
    await fireEvent.click(screen.getByRole('button', { name: /Advanced/i }))
    const textarea = screen.getByLabelText(/RRULE/i) as HTMLTextAreaElement
    await fireEvent.input(textarea, { target: { value: ADVANCED_LAST_WEEKDAY } })
    expect(screen.queryByRole('button', { name: /Use Common mode/i })).toBeNull()
    expect(screen.getByTestId('rrule-feedback').textContent ?? '').toMatch(/Advanced/)
  })

  test('invalid RRULE in Advanced disables Save and shows parse error', async () => {
    render(RecurrenceEditor, {
      props: {
        mode: createMode,
        actor: 'user-a',
        onCreate: vi.fn(),
        onPatch: vi.fn(),
        onSaved: vi.fn(),
      },
    })
    await fireEvent.click(screen.getByRole('button', { name: /Advanced/i }))
    const textarea = screen.getByLabelText(/RRULE/i) as HTMLTextAreaElement
    await fireEvent.input(textarea, { target: { value: 'FREQ=FORTNIGHTLY' } })
    expect(screen.getByTestId('rrule-feedback').textContent ?? '').toMatch(/unknown FREQ value/i)
    expect(textarea.getAttribute('aria-invalid')).toBe('true')
  })
})

describe('RecurrenceEditor (via dialog) — submit create', () => {
  test('happy path: builds {actor, rrule, dtstart, timezone, template} and calls onCreate; onClose fires on success', async () => {
    const onCreate = vi.fn().mockResolvedValue(undefined)
    const onClose = vi.fn()
    render(RecurrenceEditorDialog, {
      props: { open: true, mode: createMode, actor: 'user-a', onClose, onCreate, onPatch: vi.fn() },
    })
    await fireEvent.input(screen.getByLabelText(/Title/i), { target: { value: 'Weekly review' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(onCreate).toHaveBeenCalledTimes(1)
    const [projectID, payload] = onCreate.mock.calls[0]!
    expect(projectID).toBe(1)
    expect(payload.initialIssueRef).toBe('abc4')
    expect(payload.actor).toBe('user-a')
    expect(payload.rrule).toMatch(/^FREQ=WEEKLY;INTERVAL=1;BYDAY=/)
    expect(payload.template.title).toBe('Weekly review')
    expect(payload.timezone).not.toBe('System default')
    expect(payload.timezone).toBeTruthy()
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  test('400 validation error: dialog stays open, inline message shown, onClose not called', async () => {
    const apiError = Object.assign(new Error('template_title is required'), {
      status: 400,
      code: 'validation',
    })
    const onCreate = vi.fn().mockRejectedValue(apiError)
    const onClose = vi.fn()
    render(RecurrenceEditorDialog, {
      props: { open: true, mode: createMode, actor: 'user-a', onClose, onCreate, onPatch: vi.fn() },
    })
    await fireEvent.input(screen.getByLabelText(/Title/i), { target: { value: 'X' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(onClose).not.toHaveBeenCalled()
    expect(screen.getByText('template_title is required')).toBeTruthy()
  })

  test('invalid Advanced RRULE: Save is disabled and onCreate is never called', async () => {
    const onCreate = vi.fn()
    render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: createMode,
        actor: 'user-a',
        onClose: vi.fn(),
        onCreate,
        onPatch: vi.fn(),
      },
    })
    await fireEvent.click(screen.getByRole('button', { name: /Advanced/i }))
    const textarea = screen.getByLabelText(/RRULE/i) as HTMLTextAreaElement
    await fireEvent.input(textarea, { target: { value: 'FREQ=FORTNIGHTLY' } })
    const save = screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement
    expect(save.disabled).toBe(true)
    await fireEvent.click(save)
    expect(onCreate).not.toHaveBeenCalled()
  })

  test('authority fencing disables Save without closing or discarding the recurrence draft', async () => {
    const onCreate = vi.fn().mockResolvedValue(undefined)
    const onClose = vi.fn()
    const view = render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: createMode,
        actor: 'user-a',
        onClose,
        onCreate,
        onPatch: vi.fn(),
      },
    })
    await fireEvent.input(screen.getByLabelText(/Title/i), {
      target: { value: 'Preserve this draft' },
    })
    await view.rerender({ disabled: true })

    const save = screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement
    expect(save.disabled).toBe(true)
    await fireEvent.click(save)

    expect(onCreate).not.toHaveBeenCalled()
    expect(onClose).not.toHaveBeenCalled()
    expect((screen.getByLabelText(/Title/i) as HTMLInputElement).value).toBe('Preserve this draft')

    await view.rerender({ disabled: false })
    expect(save.disabled).toBe(false)
    await fireEvent.click(save)
    expect(onCreate).toHaveBeenCalledOnce()
    expect(onClose).toHaveBeenCalledOnce()
  })

  test('WEEKLY with no weekdays selected: Save is disabled', async () => {
    const onCreate = vi.fn()
    render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: createMode,
        actor: 'user-a',
        onClose: vi.fn(),
        onCreate,
        onPatch: vi.fn(),
      },
    })
    await fireEvent.input(screen.getByLabelText(/Title/i), { target: { value: 'x' } })
    const pressed = screen
      .getAllByRole('button', { pressed: true })
      .filter((b) => /^(Mon|Tue|Wed|Thu|Fri|Sat|Sun)$/.test(b.textContent ?? ''))
    expect(pressed).toHaveLength(1)
    await fireEvent.click(pressed[0]!)
    const save = screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement
    expect(save.disabled).toBe(true)
    await fireEvent.click(save)
    expect(onCreate).not.toHaveBeenCalled()
  })

  test('invalid Common-mode numeric values disable Save', async () => {
    const onCreate = vi.fn()
    render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: createMode,
        actor: 'user-a',
        onClose: vi.fn(),
        onCreate,
        onPatch: vi.fn(),
      },
    })
    await fireEvent.input(screen.getByLabelText(/Title/i), { target: { value: 'x' } })
    await fireEvent.input(screen.getByLabelText(/Every/i), { target: { value: '0' } })
    const save = screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement
    expect(save.disabled).toBe(true)
    await fireEvent.click(save)
    expect(onCreate).not.toHaveBeenCalled()
  })
})

describe('RecurrenceEditor — edit mode prefill', () => {
  test('Common rule loads into Common mode with form prefilled', () => {
    render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: editMode,
        actor: 'user-a',
        onClose: vi.fn(),
        onCreate: vi.fn(),
        onPatch: vi.fn(),
      },
    })
    expectDropdown('Frequency', 'Weekly')
    expect((screen.getByLabelText(/Title/i) as HTMLInputElement).value).toBe('Weekly review')
  })

  test('Advanced rule opens in Advanced mode', () => {
    const advEdit = {
      kind: 'edit' as const,
      recurrence: { ...sample, rrule: ADVANCED_LAST_WEEKDAY },
      etag: '"rev-1"',
    }
    render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: advEdit,
        actor: 'user-a',
        onClose: vi.fn(),
        onCreate: vi.fn(),
        onPatch: vi.fn(),
      },
    })
    expect((screen.getByLabelText(/RRULE/i) as HTMLTextAreaElement).value).toBe(
      ADVANCED_LAST_WEEKDAY,
    )
  })
})

describe('RecurrenceEditor — edit diff', () => {
  test('unchanged form: Save is disabled, onPatch is never called', async () => {
    const onPatch = vi.fn()
    render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: editMode,
        actor: 'user-a',
        onClose: vi.fn(),
        onCreate: vi.fn(),
        onPatch,
      },
    })
    const save = screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement
    expect(save.disabled).toBe(true)
    await fireEvent.click(save)
    expect(onPatch).not.toHaveBeenCalled()
  })

  test('template title changed: patch contains only template.title', async () => {
    const onPatch = vi.fn().mockResolvedValue(undefined)
    render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: editMode,
        actor: 'user-a',
        onClose: vi.fn(),
        onCreate: vi.fn(),
        onPatch,
      },
    })
    await fireEvent.input(screen.getByLabelText(/Title/i), { target: { value: 'New title' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    const [, payload, etag] = onPatch.mock.calls[0]!
    expect(payload).toEqual({ actor: 'user-a', template: { title: 'New title' } })
    expect(etag).toBe('"rev-1"')
  })

  test('schedule-only edit preserves template body whitespace', async () => {
    const onPatch = vi.fn().mockResolvedValue(undefined)
    render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: {
          kind: 'edit',
          recurrence: { ...sample, template_body: '  Body\n' },
          etag: '"rev-1"',
        },
        actor: 'user-a',
        onClose: vi.fn(),
        onCreate: vi.fn(),
        onPatch,
      },
    })

    await pickStartDate('May 18, 2026')
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    const [, payload] = onPatch.mock.calls[0]!
    expect(payload).not.toHaveProperty('template')
  })

  test('clearing owner and priority sends explicit clear operations', async () => {
    const onPatch = vi.fn().mockResolvedValue(undefined)
    render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: editMode,
        actor: 'user-a',
        onClose: vi.fn(),
        onCreate: vi.fn(),
        onPatch,
      },
    })
    await fireEvent.input(screen.getByLabelText(/Owner/i), { target: { value: '' } })
    await chooseDropdown('Priority', '(none)')
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    const [, payload] = onPatch.mock.calls[0]!
    expect(payload).toEqual({
      actor: 'user-a',
      template: { clearOwner: true, clearPriority: true },
    })
  })

  test('common mode canonicalizes but unchanged semantic rule does not patch rrule', async () => {
    const onPatch = vi.fn().mockResolvedValue(undefined)
    render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: editMode,
        actor: 'user-a',
        onClose: vi.fn(),
        onCreate: vi.fn(),
        onPatch,
      },
    })
    await fireEvent.input(screen.getByLabelText(/Title/i), { target: { value: 'Different title' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    const [, payload] = onPatch.mock.calls[0]!
    expect(payload).not.toHaveProperty('rrule')
  })

  test.each([
    ['weekly omitted BYDAY', { rrule: 'FREQ=WEEKLY;INTERVAL=1', dtstart: '2026-05-20' }],
    ['monthly omitted BYMONTHDAY', { rrule: 'FREQ=MONTHLY;INTERVAL=1', dtstart: '2026-05-20' }],
    ['yearly omitted BYMONTH', { rrule: 'FREQ=YEARLY;INTERVAL=1', dtstart: '2026-05-20' }],
  ])('template-only edit preserves DTSTART-derived defaults for %s', async (_label, overrides) => {
    const onPatch = vi.fn().mockResolvedValue(undefined)
    render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: { kind: 'edit', recurrence: { ...sample, ...overrides }, etag: '"rev-1"' },
        actor: 'user-a',
        onClose: vi.fn(),
        onCreate: vi.fn(),
        onPatch,
      },
    })
    await fireEvent.input(screen.getByLabelText(/Title/i), { target: { value: 'Different title' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    const [, payload] = onPatch.mock.calls[0]!
    expect(payload).toEqual({ actor: 'user-a', template: { title: 'Different title' } })
  })

  test.each([
    [
      'weekly omitted BYDAY',
      { rrule: 'FREQ=WEEKLY;INTERVAL=1', dtstart: '2026-05-20' },
      'May 18, 2026',
      '2026-05-18',
      'FREQ=WEEKLY;INTERVAL=1;BYDAY=WE',
      0,
    ],
    [
      'monthly omitted BYMONTHDAY',
      { rrule: 'FREQ=MONTHLY;INTERVAL=1', dtstart: '2026-05-20' },
      'May 18, 2026',
      '2026-05-18',
      'FREQ=MONTHLY;INTERVAL=1;BYMONTHDAY=20',
      0,
    ],
    [
      'yearly omitted BYMONTH',
      { rrule: 'FREQ=YEARLY;INTERVAL=1', dtstart: '2026-05-20' },
      'Jun 20, 2026',
      '2026-06-20',
      'FREQ=YEARLY;INTERVAL=1;BYMONTH=5',
      1,
    ],
  ])(
    'dtstart edit preserves DTSTART-derived defaults for %s',
    async (_label, overrides, calendarLabel, nextStart, expectedRRule, nextMonths) => {
      const onPatch = vi.fn().mockResolvedValue(undefined)
      render(RecurrenceEditorDialog, {
        props: {
          open: true,
          mode: { kind: 'edit', recurrence: { ...sample, ...overrides }, etag: '"rev-1"' },
          actor: 'user-a',
          onClose: vi.fn(),
          onCreate: vi.fn(),
          onPatch,
        },
      })
      await pickStartDate(calendarLabel, nextMonths)
      await fireEvent.click(screen.getByRole('button', { name: 'Save' }))
      const [, payload] = onPatch.mock.calls[0]!
      expect(payload).toEqual({ actor: 'user-a', dtstart: nextStart, rrule: expectedRRule })
    },
  )

  test('advanced mode unchanged rrule (byte-for-byte): no rrule in patch', async () => {
    const advEdit = {
      kind: 'edit' as const,
      recurrence: { ...sample, rrule: ADVANCED_LAST_WEEKDAY },
      etag: '"rev-1"',
    }
    const onPatch = vi.fn().mockResolvedValue(undefined)
    render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: advEdit,
        actor: 'user-a',
        onClose: vi.fn(),
        onCreate: vi.fn(),
        onPatch,
      },
    })
    await fireEvent.input(screen.getByLabelText(/Title/i), { target: { value: 'Different title' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    const [, payload] = onPatch.mock.calls[0]!
    expect(payload).not.toHaveProperty('rrule')
  })

  test('toggling weekdays out of order produces canonical byDay (no spurious rrule patch)', async () => {
    // Edit-mode regression: a Recurrence with BYDAY=MO,WE,FR loaded, user
    // toggles WE off then on. The form's byDay should equal [MO,WE,FR]
    // (canonical), not [MO,FR,WE]. We assert by checking that Save stays
    // disabled (no changes detected) after the redundant toggle.
    const rec: KataRecurrence = {
      id: 9,
      uid: 'rec-9',
      project_id: 1,
      rrule: 'FREQ=WEEKLY;INTERVAL=1;BYDAY=MO,WE,FR',
      dtstart: '2026-05-20',
      timezone: 'America/Chicago',
      template_title: 'Triweekly',
      template_body: '',
      template_labels: [],
      template_metadata: {},
      next_occurrence_key: '2026-05-27',
      last_materialized_uid: undefined,
      author: 'user-a',
      revision: 1,
      created_at: '2026-05-15T12:00:00.000Z',
      updated_at: '2026-05-15T12:00:00.000Z',
    }
    const triweeklyEdit = { kind: 'edit' as const, recurrence: rec, etag: '"rev-1"' }
    render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: triweeklyEdit,
        actor: 'user-a',
        onClose: vi.fn(),
        onCreate: vi.fn(),
        onPatch: vi.fn(),
      },
    })
    // Toggle WE off then on — final byDay should equal original.
    const we = screen.getAllByRole('button', { pressed: true }).find((b) => b.textContent === 'Wed')
    expect(we).toBeTruthy()
    await fireEvent.click(we!)
    await fireEvent.click(we!)
    const save = screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement
    expect(save.disabled).toBe(true)
  })
})

describe('RecurrenceEditor — 412 conflict', () => {
  test('412 response repopulates form with server payload and shows the exact banner text', async () => {
    const updatedServer: KataRecurrence = {
      ...sample,
      template_title: 'Updated upstream',
      revision: 2,
    }
    const apiError = Object.assign(new Error('revision mismatch'), {
      status: 412,
      code: 'precondition_failed',
      response: { recurrence: updatedServer, etag: '"rev-2"' },
    })
    const onPatch = vi.fn().mockRejectedValue(apiError)
    render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: editMode,
        actor: 'user-a',
        onClose: vi.fn(),
        onCreate: vi.fn(),
        onPatch,
      },
    })
    await fireEvent.input(screen.getByLabelText(/Title/i), { target: { value: 'My edit' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(
      screen.getByText('Server version loaded; your unsaved edits were not applied.'),
    ).toBeTruthy()
    expect((screen.getByLabelText(/Title/i) as HTMLInputElement).value).toBe('Updated upstream')
  })

  test('412 with changed RRULE: form schedule fields reset to server values', async () => {
    const updatedServer: KataRecurrence = {
      ...sample,
      rrule: 'FREQ=DAILY;INTERVAL=3',
      revision: 2,
    }
    const apiError = Object.assign(new Error('revision mismatch'), {
      status: 412,
      code: 'precondition_failed',
      response: { recurrence: updatedServer, etag: '"rev-2"' },
    })
    const onPatch = vi.fn().mockRejectedValue(apiError)
    render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: editMode,
        actor: 'user-a',
        onClose: vi.fn(),
        onCreate: vi.fn(),
        onPatch,
      },
    })
    // User changes title; server rule changes underneath.
    await fireEvent.input(screen.getByLabelText(/Title/i), { target: { value: 'My edit' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    // After 412 reload: form shows server's DAILY with interval 3.
    expectDropdown('Frequency', 'Daily')
    expect((screen.getByLabelText(/Every/i) as HTMLInputElement).value).toBe('3')
  })

  test('412 then re-Save: second onPatch call uses the server-attached etag', async () => {
    const updatedServer: KataRecurrence = { ...sample, template_title: 'Server title', revision: 2 }
    let callCount = 0
    const onPatch = vi.fn().mockImplementation(() => {
      callCount += 1
      if (callCount === 1) {
        const err = Object.assign(new Error('revision mismatch'), {
          status: 412,
          code: 'precondition_failed',
          response: { recurrence: updatedServer, etag: '"rev-2"' },
        })
        return Promise.reject(err)
      }
      return Promise.resolve()
    })
    render(RecurrenceEditorDialog, {
      props: {
        open: true,
        mode: editMode,
        actor: 'user-a',
        onClose: vi.fn(),
        onCreate: vi.fn(),
        onPatch,
      },
    })
    await fireEvent.input(screen.getByLabelText(/Title/i), { target: { value: 'First edit' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    // 412 fires; banner appears; form now shows server values.
    await fireEvent.input(screen.getByLabelText(/Title/i), { target: { value: 'Second edit' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(onPatch).toHaveBeenCalledTimes(2)
    expect(onPatch.mock.calls[1]![2]).toBe('"rev-2"')
  })
})
