import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'

import QuickCapture from './QuickCapture.svelte'

describe('QuickCapture', () => {
  afterEach(cleanup)

  it('requires a project before adding a task', async () => {
    render(QuickCapture, {
      props: {
        open: true,
        projects: [{ uid: 'a', name: 'Alpha' }],
        onChooseInbox: vi.fn(),
        onClose: vi.fn(),
        onSubmit: vi.fn(),
      },
    })
    await fireEvent.input(screen.getByRole('textbox', { name: 'Quick capture' }), {
      target: { value: 'Draft task' },
    })
    expect((screen.getByRole('button', { name: 'Add' }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('chooses the Inbox project without losing the draft on refusal', async () => {
    const onChooseInbox = vi.fn().mockRejectedValue(new Error('Cannot change project'))
    render(QuickCapture, {
      props: {
        open: true,
        projects: [
          { uid: 'a', name: 'Alpha' },
          { uid: 'b', name: 'Beta' },
        ],
        inboxUID: 'a',
        onChooseInbox,
        onClose: vi.fn(),
        onSubmit: vi.fn(),
      },
    })
    const input = screen.getByRole('textbox', { name: 'Quick capture' }) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'Draft task' } })
    await fireEvent.click(screen.getByRole('combobox', { name: 'Project: Alpha' }))
    await fireEvent.click(screen.getByRole('option', { name: 'Beta' }))
    expect(onChooseInbox).toHaveBeenCalledWith('b')
    expect((await screen.findByRole('alert')).textContent).toBe('Cannot change project')
    expect(input.value).toBe('Draft task')
  })

  it('keeps the draft and shows the error when capture fails', async () => {
    const onClose = vi.fn()
    const onSubmit = vi.fn().mockRejectedValue(new Error('Server unavailable'))
    render(QuickCapture, { props: { open: true, onClose, onSubmit } })
    const input = screen.getByRole('textbox', { name: 'Quick capture' }) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'Draft task' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Add' }))

    expect((await screen.findByRole('alert')).textContent).toBe('Server unavailable')
    expect(input.value).toBe('Draft task')
    expect(onSubmit).toHaveBeenCalledWith('Draft task')
    expect(onClose).not.toHaveBeenCalled()
  })

  it('uses shared Kit UI actions', () => {
    render(QuickCapture, {
      props: {
        open: true,
        onClose: vi.fn(),
        onSubmit: vi.fn(),
      },
    })

    expect(screen.getByRole('button', { name: 'Cancel' }).classList).toContain('kit-button')
    expect(screen.getByRole('button', { name: 'Add' }).classList).toContain('kit-button')
  })

  it('focuses the task title when an opening refresh finishes', async () => {
    const view = render(QuickCapture, {
      props: { open: true, disabled: true, onClose: vi.fn(), onSubmit: vi.fn() },
    })
    screen.getByRole('button', { name: 'Cancel' }).focus()
    await view.rerender({ disabled: false })
    await waitFor(() =>
      expect(document.activeElement).toBe(screen.getByRole('textbox', { name: 'Quick capture' })),
    )
  })

  it('resets a task draft after authentication authority changes', async () => {
    const view = render(QuickCapture, {
      props: {
        open: true,
        draftFenceGeneration: 0,
        onClose: vi.fn(),
        onSubmit: vi.fn(),
      },
    })
    await fireEvent.input(screen.getByRole('textbox', { name: 'Quick capture' }), {
      target: { value: 'Old authority task' },
    })

    await view.rerender({ draftFenceGeneration: 1 })

    expect((screen.getByRole('textbox', { name: 'Quick capture' }) as HTMLInputElement).value).toBe(
      '',
    )
  })
})
