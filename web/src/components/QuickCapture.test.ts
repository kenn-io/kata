import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, it, onTestFinished, vi } from 'vitest'

import QuickCapture from './QuickCapture.svelte'

describe('QuickCapture', () => {
  afterEach(cleanup)

  it('uses shared Kit UI actions', () => {
    render(QuickCapture, {
      props: {
        open: true,
        onClose: vi.fn(),
        onSubmit: vi.fn(),
      },
    })

    expect(screen.getByRole('button', { name: 'Cancel' }).classList).toContain('kit-button')
    expect(screen.getByRole('button', { name: 'Capture' }).classList).toContain('kit-button')
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

  it('keeps the task title after capture fails', async () => {
    const rejections: unknown[] = []
    const onRejection = (error: unknown) => rejections.push(error)
    process.on('unhandledRejection', onRejection)
    onTestFinished(() => {
      process.off('unhandledRejection', onRejection)
    })
    const onSubmit = vi.fn(async () => {
      throw new Error('Task creation failed')
    })
    render(QuickCapture, { props: { open: true, onClose: vi.fn(), onSubmit } })
    const title = screen.getByRole('textbox', { name: 'Quick capture' }) as HTMLInputElement
    await fireEvent.input(title, { target: { value: 'Task from Settings' } })
    await fireEvent.click(screen.getByRole('button', { name: 'Capture' }))
    await waitFor(() => expect(screen.getByRole('alert').textContent).toBe('Task creation failed'))
    await waitFor(() => expect(rejections).toEqual([new Error('Task creation failed')]))
    expect(title.value).toBe('Task from Settings')
    expect(onSubmit).toHaveBeenCalledWith('Task from Settings')
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
