import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'

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
