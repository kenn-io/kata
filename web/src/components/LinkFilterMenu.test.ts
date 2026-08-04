import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createKataLinkFilters } from '../lib/kata/linkFilters'
import LinkFilterMenu from './LinkFilterMenu.svelte'

describe('LinkFilterMenu', () => {
  afterEach(cleanup)

  it('emits independent task-state and relationship changes', async () => {
    const filters = createKataLinkFilters('open')
    const onChange = vi.fn()
    render(LinkFilterMenu, { props: { filters, onChange } })

    await fireEvent.click(screen.getByRole('button', { name: 'Filter links' }))
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Closed' }))
    expect(onChange).toHaveBeenLastCalledWith({
      ...filters,
      statuses: { open: true, closed: true },
    })

    await fireEvent.click(screen.getByRole('checkbox', { name: 'Blocked by' }))
    expect(onChange).toHaveBeenLastCalledWith({
      ...filters,
      relations: { ...filters.relations, blocked_by: false },
    })
  })

  it('closes on Escape and returns focus to the trigger', async () => {
    render(LinkFilterMenu, {
      props: { filters: createKataLinkFilters('all'), onChange: vi.fn() },
    })

    const trigger = screen.getByRole('button', { name: 'Filter links' })
    await fireEvent.click(trigger)
    await fireEvent.keyDown(document, { key: 'Escape' })

    expect(screen.queryByRole('group', { name: 'Link filters' })).toBeNull()
    expect(document.activeElement).toBe(trigger)
  })
})
