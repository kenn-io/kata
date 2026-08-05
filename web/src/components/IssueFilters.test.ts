import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, describe, expect, test, vi } from 'vitest'

import type { KataProjectSummary, KataTaskSearchFilters } from '../lib/kata/types'
import IssueFilters from './IssueFilters.svelte'

const filters: KataTaskSearchFilters = {
  scope: { kind: 'all' },
  status: 'open',
  owner: '',
  label: '',
  query: '',
}

afterEach(cleanup)

describe('IssueFilters', () => {
  test('emits compact search and filter control changes', async () => {
    const changes: KataTaskSearchFilters[] = []
    const onChange = vi.fn((next: KataTaskSearchFilters) => {
      changes.push(next)
    })

    const { rerender } = render(IssueFilters, {
      props: {
        filters,
        projects,
        onChange,
      },
    })
    const applyLatest = async () => {
      const next = changes[changes.length - 1]
      expect(next).toBeTruthy()
      await rerender({ filters: next!, projects, onChange })
    }

    await fireEvent.input(screen.getByLabelText('Search tasks'), { target: { value: 'rent' } })
    await applyLatest()
    await fireEvent.click(screen.getByRole('combobox', { name: 'Status: Open' }))
    await fireEvent.click(screen.getByRole('option', { name: 'All' }))
    await applyLatest()
    await fireEvent.input(screen.getByLabelText('Owner'), { target: { value: 'user-a' } })
    await applyLatest()
    await fireEvent.input(screen.getByLabelText('Label'), { target: { value: 'health' } })
    await applyLatest()
    await fireEvent.click(screen.getByRole('button', { name: /Project scope: All projects/i }))
    const projectInput = screen.getByRole('combobox', { name: 'Project scope' })
    expect(document.activeElement).toBe(projectInput)
    await fireEvent.input(projectInput, { target: { value: 'exam' } })
    await fireEvent.keyDown(projectInput, { key: 'Enter' })

    await waitFor(() => expect(changes.length).toBeGreaterThanOrEqual(5))
    expect(changes[changes.length - 1]).toMatchObject({
      query: 'rent',
      status: 'all',
      owner: 'user-a',
      label: 'health',
      scope: { kind: 'project', project_uid: 'project-example' },
    })
  })

  test('emits the Ready status filter', async () => {
    const onChange = vi.fn()
    render(IssueFilters, { props: { filters, projects, onChange } })

    await fireEvent.click(screen.getByRole('combobox', { name: 'Status: Open' }))
    await fireEvent.click(screen.getByRole('option', { name: 'Ready' }))

    expect(onChange).toHaveBeenCalledWith({ ...filters, status: 'ready' }, 'status')
  })

  test('emits a relationship filter', async () => {
    const onChange = vi.fn()
    render(IssueFilters, { props: { filters, projects, onChange } })

    await fireEvent.click(screen.getByRole('combobox', { name: 'Relationship: Any' }))
    await fireEvent.click(screen.getByRole('option', { name: 'Blocked by' }))

    expect(onChange).toHaveBeenCalledWith(
      { ...filters, relationships: ['blocked_by'] },
      'relationships',
    )
  })

  test('keeps fast filter edits when parent state has not rerendered yet', async () => {
    const changes: KataTaskSearchFilters[] = []
    const onChange = vi.fn((next: KataTaskSearchFilters) => {
      changes.push(next)
    })

    render(IssueFilters, {
      props: {
        filters,
        projects,
        onChange,
      },
    })

    await fireEvent.input(screen.getByLabelText('Search tasks'), { target: { value: 'rent' } })
    await fireEvent.click(screen.getByRole('button', { name: /Project scope: All projects/i }))
    const projectInput = screen.getByRole('combobox', { name: 'Project scope' })
    await fireEvent.input(projectInput, { target: { value: 'exam' } })
    await fireEvent.keyDown(projectInput, { key: 'Enter' })

    await waitFor(() => expect(changes.length).toBeGreaterThanOrEqual(2))
    expect(changes[changes.length - 1]).toMatchObject({
      query: 'rent',
      scope: { kind: 'project', project_uid: 'project-example' },
    })
  })
})

const projects: KataProjectSummary[] = [
  {
    id: 1,
    uid: 'project-workspace',
    name: 'example-workspace',
    open_count: 4,
    metadata: {},
  },
  {
    id: 2,
    uid: 'project-example',
    name: 'example-project',
    open_count: 2,
    metadata: {},
  },
]
