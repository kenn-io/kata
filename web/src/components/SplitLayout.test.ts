import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import SplitLayoutTestHarness from './SplitLayoutTestHarness.svelte'

function mockRect(width = 800, height = 600): void {
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue({
    width,
    height,
    x: 0,
    y: 0,
    top: 0,
    right: width,
    bottom: height,
    left: 0,
    toJSON: () => ({}),
  })
}

describe('SplitLayout', () => {
  beforeEach(() => {
    vi.stubGlobal(
      'ResizeObserver',
      class {
        observe(): void {}
        unobserve(): void {}
        disconnect(): void {}
      },
    )
  })

  afterEach(() => {
    cleanup()
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
  })

  it('resizes horizontal panes with normal and accelerated keyboard steps', async () => {
    mockRect()
    const onResize = vi.fn()
    render(SplitLayoutTestHarness, { props: { onResize } })

    const handle = screen.getByRole('separator', { name: 'Resize Kata panes' })
    expect(handle.getAttribute('aria-orientation')).toBe('vertical')
    expect(handle.getAttribute('aria-valuemin')).toBe('200')
    expect(handle.getAttribute('aria-valuemax')).toBe('600')
    expect(handle.getAttribute('aria-valuenow')).toBe('300')

    await fireEvent.keyDown(handle, { key: 'ArrowRight' })
    expect(onResize).toHaveBeenLastCalledWith(316)

    onResize.mockClear()
    await fireEvent.keyDown(handle, { key: 'ArrowRight', shiftKey: true })
    expect(onResize).toHaveBeenLastCalledWith(364)

    onResize.mockClear()
    await fireEvent.keyDown(handle, { key: 'ArrowDown' })
    expect(onResize).not.toHaveBeenCalled()
  })

  it('keeps a saved size that does not fit, so it returns when space does', async () => {
    mockRect()
    const onResize = vi.fn()
    render(SplitLayoutTestHarness, { props: { primarySize: 700, onResize } })

    const handle = screen.getByRole('separator', { name: 'Resize Kata panes' })
    expect(handle.getAttribute('aria-valuenow')).toBe('600')
    expect(onResize).not.toHaveBeenCalled()
  })

  it('uses the vertical axis and clamps both resize bounds', async () => {
    mockRect()
    const onResize = vi.fn()
    render(SplitLayoutTestHarness, {
      props: { orientation: 'vertical', primarySize: 210, onResize },
    })

    const handle = screen.getByRole('separator', { name: 'Resize Kata panes' })
    expect(handle.getAttribute('aria-orientation')).toBe('horizontal')
    expect(handle.getAttribute('aria-valuemax')).toBe('400')
    expect(handle.getAttribute('aria-valuenow')).toBe('210')

    await fireEvent.keyDown(handle, { key: 'ArrowUp', shiftKey: true })
    expect(onResize).toHaveBeenLastCalledWith(200)

    onResize.mockClear()
    await fireEvent.keyDown(handle, { key: 'ArrowDown', shiftKey: true })
    expect(onResize).toHaveBeenLastCalledWith(274)
  })

  it('reports when the secondary pane sits beside the primary pane', async () => {
    mockRect()
    const horizontal = render(SplitLayoutTestHarness)
    await waitFor(() => expect(screen.getByTestId('secondary-beside').textContent).toBe('true'))

    horizontal.unmount()
    vi.restoreAllMocks()
    mockRect()
    const vertical = render(SplitLayoutTestHarness, { props: { orientation: 'vertical' } })
    await waitFor(() => expect(screen.getByTestId('secondary-beside').textContent).toBe('false'))

    vertical.unmount()
    vi.restoreAllMocks()
    mockRect(620, 800)
    const stacked = render(SplitLayoutTestHarness, {
      props: { orientation: 'horizontal', responsiveBreakpoint: 700 },
    })
    await waitFor(() => {
      expect(stacked.container.querySelector('.kata-sash')?.getAttribute('data-orientation')).toBe(
        'vertical',
      )
      expect(screen.getByTestId('secondary-beside').textContent).toBe('false')
    })
  })

  it('preserves the split preference while the detail pane is closed', () => {
    mockRect(400, 600)
    const onResize = vi.fn()
    render(SplitLayoutTestHarness, {
      props: { primarySize: 300, showSecondary: false, onResize },
    })

    expect(screen.queryByRole('separator')).toBeNull()
    expect(onResize).not.toHaveBeenCalled()
  })

  it('starts at the default share and reports a resize in pixels', async () => {
    mockRect(1200, 600)
    const onResize = vi.fn()
    render(SplitLayoutTestHarness, { props: { primarySize: null, defaultShare: 2 / 3, onResize } })

    const handle = screen.getByRole('separator', { name: 'Resize Kata panes' })
    expect(handle.getAttribute('aria-valuenow')).toBe('800')
    expect(onResize).not.toHaveBeenCalled()
    await fireEvent.keyDown(handle, { key: 'ArrowRight' })
    expect(onResize).toHaveBeenLastCalledWith(816)
  })
})
