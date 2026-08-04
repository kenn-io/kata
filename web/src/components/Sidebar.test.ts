import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte'
import type { ComponentProps } from 'svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { KataProjectSummary, KataTaskSearchFilters } from '../lib/kata/types'
import type { KataAreaSummary, KataCurrentView } from '../lib/kata/authority'
import Sidebar from './Sidebar.svelte'

const projects: KataProjectSummary[] = [
  project({
    id: 1,
    uid: 'project-inbox',
    name: 'Inbox',
    metadata: { role: 'inbox' },
    open_count: 2,
  }),
  project({
    id: 2,
    uid: 'project-example',
    name: 'example-project',
    metadata: { area: 'Personal' },
    open_count: 1,
  }),
  project({
    id: 3,
    uid: 'project-workspace',
    name: 'example-workspace',
    metadata: { area: 'Work' },
    open_count: 4,
  }),
]

const areas: KataAreaSummary[] = [
  { name: 'Personal', projects: [projects[1]!] },
  { name: 'Work', projects: [projects[2]!] },
]

const currentView: KataCurrentView = {
  name: 'today',
  fetched_at: '2026-05-16T10:00:00Z',
  groups: [{ id: 'today', title: 'Today', issues: [] }],
}

const allScopeFilters: KataTaskSearchFilters = {
  scope: { kind: 'all' },
  status: 'open',
  owner: '',
  label: '',
  query: '',
}

type SidebarProps = ComponentProps<typeof Sidebar>

function renderSidebar(overrides: Partial<SidebarProps> = {}) {
  return render(Sidebar, {
    props: {
      areas,
      projects,
      currentView,
      searchFilters: allScopeFilters,
      projectCreationDisabled: false,
      inboxProjectUID: 'project-inbox',
      inboxDesignationDisabled: false,
      onOpenView: vi.fn(),
      onOpenProject: vi.fn(),
      onCreateProject: vi.fn(),
      onDesignateInbox: vi.fn(),
      ...overrides,
    },
  })
}

describe('Sidebar', () => {
  afterEach(() => {
    cleanup()
    vi.restoreAllMocks()
  })

  it('renders system views, expanded area groups, and project creation in order', () => {
    renderSidebar()

    const navigation = screen.getByRole('region', { name: 'Kata navigation' })
    const inbox = within(navigation).getByRole('button', { name: 'Inbox 2' })
    const personal = within(navigation).getByRole('button', { name: /^Personal\s+1$/ })
    const work = within(navigation).getByRole('button', { name: /^Work\s+1$/ })
    const create = within(navigation).getByRole('button', { name: 'New project' })

    expect(personal.getAttribute('aria-expanded')).toBe('true')
    expect(work.getAttribute('aria-expanded')).toBe('true')
    const ordered = [inbox, personal, work, create]
    for (let index = 0; index < ordered.length - 1; index += 1) {
      expect(ordered[index]!.compareDocumentPosition(ordered[index + 1]!)).toBe(
        Node.DOCUMENT_POSITION_FOLLOWING,
      )
    }
  })

  it('keeps area collapse state while mounted and resets it after remount', async () => {
    const view = renderSidebar()
    const personal = screen.getByRole('button', { name: /^Personal\s+1$/ })

    await fireEvent.click(personal)
    expect(personal.getAttribute('aria-expanded')).toBe('false')
    expect(screen.queryByRole('button', { name: /^example-project\b/ })).toBeNull()

    await view.rerender({ areas: [...areas] })
    expect(
      screen.getByRole('button', { name: /^Personal\s+1$/ }).getAttribute('aria-expanded'),
    ).toBe('false')

    view.unmount()
    renderSidebar()
    expect(
      screen.getByRole('button', { name: /^Personal\s+1$/ }).getAttribute('aria-expanded'),
    ).toBe('true')
  })

  it('opens system views and project scopes from the restored sidebar', async () => {
    const onOpenView = vi.fn()
    const onOpenProject = vi.fn()

    renderSidebar({ onOpenView, onOpenProject })

    await fireEvent.click(screen.getByRole('button', { name: 'Inbox 2' }))
    expect(onOpenView).toHaveBeenCalledWith('inbox')

    await fireEvent.click(screen.getByRole('button', { name: /^example-project\b/ }))
    expect(onOpenProject).toHaveBeenCalledWith('project-example')
  })

  it('keeps project rows navigation-only without rename affordances', async () => {
    const onOpenProject = vi.fn()
    renderSidebar({
      onOpenProject,
      searchFilters: {
        ...allScopeFilters,
        scope: { kind: 'project', project_uid: 'project-example' },
      },
    })

    const projectButton = screen.getByRole('button', { name: /^example-project\b/ })
    expect(projectButton.classList.contains('active')).toBe(true)
    expect(screen.queryByRole('button', { name: 'Rename example-project' })).toBeNull()
    expect(screen.queryByRole('textbox', { name: 'Rename project' })).toBeNull()

    await fireEvent.click(projectButton)
    await fireEvent.doubleClick(projectButton)
    expect(screen.queryByRole('textbox', { name: 'Rename project' })).toBeNull()
    expect(onOpenProject).toHaveBeenCalledWith('project-example')
  })

  it('submits project creation without deriving navigation from the mutation result', async () => {
    const onCreateProject = vi.fn(async () => ({ changed: true }))
    const onOpenProject = vi.fn()

    renderSidebar({ onCreateProject, onOpenProject })

    await fireEvent.click(screen.getByRole('button', { name: 'New project' }))
    const input = screen.getByRole('textbox', { name: 'New project name' })
    await waitFor(() => expect(input).toBe(document.activeElement))
    await fireEvent.input(input, { target: { value: 'New Project' } })
    await fireEvent.submit(input.closest('form')!)

    await waitFor(() => {
      expect(onCreateProject).toHaveBeenCalledWith('New Project')
    })
    expect(onOpenProject).not.toHaveBeenCalled()
    expect(screen.queryByRole('textbox', { name: 'New project name' })).toBeNull()
  })

  it('blocks project creation while mutation authority is disabled', async () => {
    const onCreateProject = vi.fn(async () => ({ changed: true }))
    const view = renderSidebar({ onCreateProject, projectCreationDisabled: true })

    const create = screen.getByRole('button', { name: 'New project' }) as HTMLButtonElement
    expect(create.disabled).toBe(true)
    await fireEvent.click(create)
    expect(screen.queryByRole('textbox', { name: 'New project name' })).toBeNull()

    await view.rerender({ projectCreationDisabled: false })
    await fireEvent.click(screen.getByRole('button', { name: 'New project' }))
    const input = screen.getByRole('textbox', { name: 'New project name' }) as HTMLInputElement
    await fireEvent.input(input, { target: { value: 'Blocked Project' } })

    await view.rerender({ projectCreationDisabled: true })
    expect(input.disabled).toBe(true)
    await fireEvent.submit(input.closest('form')!)
    expect(onCreateProject).not.toHaveBeenCalled()
  })
})

function project(overrides: Partial<KataProjectSummary>): KataProjectSummary {
  return {
    id: 1,
    uid: 'project',
    name: 'Project',
    metadata: {},
    open_count: 0,
    ...overrides,
  }
}
