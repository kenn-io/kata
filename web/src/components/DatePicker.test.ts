import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'
import DatePicker from './DatePicker.svelte'

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-06-01T12:00:00Z'))
})

afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

describe('DatePicker', () => {
  test('opens kit Calendar with the selected day', async () => {
    const { container } = render(DatePicker, {
      props: {
        value: '2026-06-05',
        ariaLabel: 'Due',
        onchange: vi.fn(),
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: /Due:/ }))

    expect(container.querySelector('.kit-calendar')).not.toBeNull()
    expect(screen.getByRole('button', { name: /Jun 5, 2026/ }).getAttribute('aria-pressed')).toBe(
      'true',
    )
  })

  test('opens on the current month when the stored date is malformed', async () => {
    render(DatePicker, {
      props: {
        value: '2026-13-40',
        ariaLabel: 'Due',
        onchange: vi.fn(),
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: /Due:/ }))

    expect(screen.getByRole('button', { name: /June 2026\. Choose month/ })).toBeTruthy()
    expect(screen.queryByRole('button', { pressed: true })).toBeNull()
  })

  test('reports an ISO date, closes, and restores trigger focus', async () => {
    const onchange = vi.fn()
    render(DatePicker, {
      props: {
        value: '2026-06-05',
        ariaLabel: 'Due',
        onchange,
      },
    })

    const trigger = screen.getByRole('button', { name: /Due:/ })
    await fireEvent.click(trigger)
    await fireEvent.click(screen.getByRole('button', { name: /Jun 8, 2026/ }))

    expect(onchange).toHaveBeenCalledWith('2026-06-08')
    expect(screen.queryByRole('dialog', { name: 'Due' })).toBeNull()
    expect(document.activeElement).toBe(trigger)
  })

  test('supports Calendar month navigation and month drill-down', async () => {
    render(DatePicker, {
      props: {
        value: '2026-06-05',
        ariaLabel: 'Due',
        onchange: vi.fn(),
      },
    })

    await fireEvent.click(screen.getByRole('button', { name: /Due:/ }))
    await fireEvent.click(screen.getByRole('button', { name: 'Next month' }))

    const monthHeading = screen.getByRole('button', { name: /July 2026\. Choose month/ })
    expect(monthHeading).not.toBeNull()
    await fireEvent.click(monthHeading)
    expect(screen.getByRole('button', { name: 'July 2026' })).not.toBeNull()
  })

  test('does not open while disabled', async () => {
    render(DatePicker, {
      props: {
        value: '2026-06-05',
        ariaLabel: 'Due',
        disabled: true,
        onchange: vi.fn(),
      },
    })

    const trigger = screen.getByRole('button', { name: /Due:/ })
    await fireEvent.click(trigger)

    expect((trigger as HTMLButtonElement).disabled).toBe(true)
    expect(screen.queryByRole('dialog', { name: 'Due' })).toBeNull()
  })

  test('clear button reports an empty date and restores trigger focus', async () => {
    const onchange = vi.fn()
    render(DatePicker, {
      props: {
        value: '2026-06-05',
        ariaLabel: 'Scheduled',
        clearable: true,
        onchange,
      },
    })

    const trigger = screen.getByRole('button', { name: /Scheduled:/ })
    await fireEvent.click(screen.getByRole('button', { name: /Clear scheduled/i }))

    expect(onchange).toHaveBeenCalledWith('')
    expect(document.activeElement).toBe(trigger)
  })

  test('Escape on clear button and calendar controls calls onEscape', async () => {
    const onEscape = vi.fn()
    render(DatePicker, {
      props: {
        value: '2026-06-05',
        ariaLabel: 'Scheduled',
        clearable: true,
        onchange: vi.fn(),
        onEscape,
      },
    })

    await fireEvent.keyDown(screen.getByRole('button', { name: /Clear scheduled/i }), {
      key: 'Escape',
    })
    expect(onEscape).toHaveBeenCalledTimes(1)

    await fireEvent.click(screen.getByRole('button', { name: /Scheduled:/ }))
    await fireEvent.keyDown(screen.getByRole('button', { name: 'Next month' }), { key: 'Escape' })

    expect(onEscape).toHaveBeenCalledTimes(2)
  })

  test('Escape bubbles when the picker is closed and has no escape handler', async () => {
    const onDocumentKeydown = vi.fn()
    document.addEventListener('keydown', onDocumentKeydown)
    try {
      render(DatePicker, {
        props: {
          value: '2026-06-05',
          ariaLabel: 'Scheduled',
          clearable: true,
          onchange: vi.fn(),
        },
      })

      await fireEvent.keyDown(screen.getByRole('button', { name: /Clear scheduled/i }), {
        key: 'Escape',
      })

      expect(onDocumentKeydown).toHaveBeenCalledTimes(1)
    } finally {
      document.removeEventListener('keydown', onDocumentKeydown)
    }
  })
})
