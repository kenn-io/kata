import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte'
import type { ComponentProps } from 'svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { KataProjectSummary, KataTaskDetail } from '../lib/kata/types'

import MoveIssueDialog from './MoveIssueDialog.svelte'

function makeIssue(
  status: 'open' | 'closed' = 'open',
  overrides: Partial<KataTaskDetail['issue']> = {},
): KataTaskDetail {
  return {
    issue: {
      id: 1,
      uid: 'issue-1',
      project_id: 2,
      project_uid: 'project-alpha',
      project_name: 'Alpha',
      short_id: 'I-1',
      qualified_id: 'INBOX-1',
      title: 'Ship the thing',
      body: 'Body',
      status,
      metadata: {},
      revision: 1,
      author: 'user-a',
      created_at: '2026-06-01T12:00:00Z',
      updated_at: '2026-06-01T12:00:00Z',
      ...overrides,
    },
    comments: [],
    labels: [],
    links: [],
  }
}

function makeProject(
  uid: string,
  name: string,
  openCount: number,
  metadata: KataProjectSummary['metadata'] = {},
): KataProjectSummary {
  return {
    id: uid === 'project-alpha' ? 2 : uid.length,
    uid,
    name,
    metadata,
    open_count: openCount,
    revision: 1,
    created_at: '2026-06-01T12:00:00Z',
  }
}

const projects = [
  makeProject('project-inbox', 'Inbox', 2, { role: 'inbox' }),
  makeProject('project-alpha', 'Alpha', 3),
  makeProject('project-roadmap', 'Roadmap', 5),
  makeProject('project-shared-work', 'Shared', 2, { area: 'Work' }),
  makeProject('project-shared-home', 'Shared', 4, { area: 'Home' }),
  makeProject('project-shared-home-2', 'Shared', 1, { area: 'Home' }),
]

type MenuProps = ComponentProps<typeof MoveIssueDialog>

function renderMenu(overrides: Partial<MenuProps> = {}) {
  return render(MoveIssueDialog, {
    props: {
      issue: makeIssue(),
      projects,
      hasChecklist: false,
      hasRecurrence: false,
      onMoveIssue: vi.fn(async () => true),
      onAddChecklist: vi.fn(),
      onCreateRecurrence: vi.fn(),
      onDeleteIssue: vi.fn(async () => true),
      ...overrides,
    },
  })
}

async function openMovePicker(): Promise<void> {
  await fireEvent.click(screen.getByRole('button', { name: 'More actions' }))
  await fireEvent.click(screen.getByRole('menuitem', { name: 'Move to another project' }))
}

describe('MoveIssueDialog', () => {
  afterEach(() => {
    cleanup()
  })

  it('routes menu actions to their callbacks', async () => {
    const onAddChecklist = vi.fn()
    const onCreateRecurrence = vi.fn()
    renderMenu({ onAddChecklist, onCreateRecurrence })

    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }))
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Add checklist' }))
    expect(onAddChecklist).toHaveBeenCalledTimes(1)

    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }))
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Create recurrence...' }))
    expect(onCreateRecurrence).toHaveBeenCalledTimes(1)
  })

  it('hides move when only the current and inbox projects exist', async () => {
    renderMenu({
      projects: [
        makeProject('project-alpha', 'Alpha', 1),
        makeProject('project-inbox', 'Inbox', 2, { role: 'inbox' }),
      ],
    })

    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }))
    expect(screen.queryByRole('menuitem', { name: 'Move to another project' })).toBeNull()
  })

  it('shows sorted eligible destinations with unambiguous duplicate context', async () => {
    renderMenu({ projects: [...projects].reverse() })
    await openMovePicker()

    const destinations = within(screen.getByLabelText('Project destinations')).getAllByRole(
      'button',
    )
    expect(
      destinations.slice(0, 2).map((button) => button.textContent?.replaceAll(/\s/g, '')),
    ).toEqual(['Roadmap5', 'Sharedproject-shared-home4'])
    expect(screen.getByRole('button', { name: /Shared.*project-shared-home-2.*1/ })).toBeTruthy()
    expect(screen.getByRole('button', { name: /Shared.*Work.*2/ })).toBeTruthy()
    expect(screen.queryByText('Inbox')).toBeNull()
  })

  it('filters destinations and closes after a successful move', async () => {
    const onMoveIssue = vi.fn(async () => true)
    renderMenu({ onMoveIssue })

    await openMovePicker()
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Find project' }), {
      target: { value: 'road' },
    })
    expect(screen.queryByRole('button', { name: /Shared/ })).toBeNull()
    await fireEvent.click(screen.getByRole('button', { name: /Roadmap/ }))

    expect(onMoveIssue).toHaveBeenCalledWith('project-roadmap')
    expect(screen.queryByRole('searchbox', { name: 'Find project' })).toBeNull()
    expect(screen.queryByRole('menu', { name: 'Task actions' })).toBeNull()
  })

  it('keeps the picker open when the workspace reports move failure', async () => {
    const onMoveIssue = vi.fn(async () => false)
    renderMenu({ onMoveIssue })
    await openMovePicker()
    await fireEvent.click(screen.getByRole('button', { name: /Roadmap/ }))

    expect(onMoveIssue).toHaveBeenCalledWith('project-roadmap')
    expect(screen.getByRole('searchbox', { name: 'Find project' })).toBeTruthy()
  })

  it('resets the destination view when the selected issue changes', async () => {
    const view = renderMenu()
    await openMovePicker()
    await fireEvent.input(screen.getByRole('searchbox', { name: 'Find project' }), {
      target: { value: 'road' },
    })

    await view.rerender({ issue: makeIssue('open', { uid: 'issue-2' }) })
    expect(screen.queryByRole('searchbox', { name: 'Find project' })).toBeNull()
    expect(screen.getByRole('button', { name: 'More actions' }).getAttribute('aria-expanded')).toBe(
      'false',
    )
  })

  it('blocks a second A move until the old A move settles after A to B to A navigation', async () => {
    let finishOldMove!: (moved: boolean) => void
    const oldMove = new Promise<boolean>((resolve) => {
      finishOldMove = resolve
    })
    const onMoveIssue = vi
      .fn()
      .mockImplementationOnce(() => oldMove)
      .mockResolvedValueOnce(true)
    const view = renderMenu({ onMoveIssue })

    await openMovePicker()
    await fireEvent.click(screen.getByRole('button', { name: /Roadmap/ }))
    await view.rerender({ issue: makeIssue('open', { uid: 'issue-b' }) })
    await view.rerender({ issue: makeIssue('open', { uid: 'issue-1' }) })
    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }))

    expect(screen.queryByRole('menuitem', { name: 'Move to another project' })).toBeNull()
    expect(onMoveIssue).toHaveBeenCalledTimes(1)

    finishOldMove(true)
    await oldMove
    await waitFor(() => {
      expect(
        screen.getByRole('button', { name: 'More actions' }).getAttribute('aria-expanded'),
      ).toBe('false')
    })
    await Promise.resolve()
    await openMovePicker()
    const destination = screen.getByRole('button', { name: /Roadmap/ }) as HTMLButtonElement
    expect(destination.disabled).toBe(false)
    await fireEvent.click(destination)
    expect(onMoveIssue).toHaveBeenCalledTimes(2)
    expect(screen.queryByRole('searchbox', { name: 'Find project' })).toBeNull()
  })

  it('updates destinations reactively and retains the pending destination snapshot', async () => {
    let finishMove!: (moved: boolean) => void
    const pendingMove = new Promise<boolean>((resolve) => {
      finishMove = resolve
    })
    const view = renderMenu({ onMoveIssue: vi.fn(() => pendingMove) })

    await openMovePicker()
    await view.rerender({
      projects: projects.map((project) =>
        project.uid === 'project-roadmap' ? { ...project, name: 'Roadmap 2027' } : project,
      ),
    })
    expect(screen.getByRole('button', { name: /Roadmap 2027/ })).toBeTruthy()

    await fireEvent.click(screen.getByRole('button', { name: /Roadmap 2027/ }))
    await view.rerender({
      projects: projects.filter((project) => project.uid !== 'project-roadmap'),
    })
    expect(
      (screen.getByRole('button', { name: /Roadmap 2027/ }) as HTMLButtonElement).disabled,
    ).toBe(true)

    finishMove(false)
    await pendingMove
    await view.rerender({
      projects: projects.filter((project) => project.uid !== 'project-roadmap'),
    })
    expect(screen.queryByRole('button', { name: /Roadmap 2027/ })).toBeNull()
  })

  it('dismisses an empty move picker with Escape and restores trigger focus', async () => {
    const onMoveIssue = vi.fn(async () => true)
    renderMenu({ onMoveIssue })

    const trigger = screen.getByRole('button', { name: 'More actions' })
    trigger.focus()
    await fireEvent.click(trigger)
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Move to another project' }))

    const search = screen.getByRole('searchbox', { name: 'Find project' })
    await waitFor(() => expect(search).toBe(document.activeElement))
    await fireEvent.keyDown(search, { key: 'Escape' })

    expect(onMoveIssue).not.toHaveBeenCalled()
    expect(screen.queryByRole('dialog', { name: 'Move to another project' })).toBeNull()
    await waitFor(() => expect(trigger).toBe(document.activeElement))
  })

  it('clears a move search before Escape dismisses the picker', async () => {
    renderMenu()
    await openMovePicker()

    const search = screen.getByRole('searchbox', { name: 'Find project' })
    await fireEvent.input(search, { target: { value: 'road' } })
    await fireEvent.keyDown(search, { key: 'Escape' })

    expect((search as HTMLInputElement).value).toBe('')
    expect(screen.getByRole('dialog', { name: 'Move to another project' })).toBeTruthy()

    await fireEvent.keyDown(search, { key: 'Escape' })
    expect(screen.queryByRole('dialog', { name: 'Move to another project' })).toBeNull()
  })

  it('does not dismiss while a move is pending', async () => {
    let finishMove!: (moved: boolean) => void
    const pendingMove = new Promise<boolean>((resolve) => {
      finishMove = resolve
    })
    const onMoveIssue = vi.fn(() => pendingMove)
    renderMenu({ onMoveIssue })

    await openMovePicker()
    await fireEvent.click(screen.getByRole('button', { name: /Roadmap/ }))
    await fireEvent.keyDown(screen.getByRole('dialog', { name: 'Move to another project' }), {
      key: 'Escape',
    })
    await fireEvent.pointerDown(document.body)

    expect(screen.getByRole('searchbox', { name: 'Find project' })).toBeTruthy()
    expect((screen.getByRole('button', { name: /Roadmap/ }) as HTMLButtonElement).disabled).toBe(
      true,
    )
    expect(onMoveIssue).toHaveBeenCalledTimes(1)
    finishMove(false)
    await pendingMove
  })

  it('confirms delete through the menu', async () => {
    const onDeleteIssue = vi.fn(async () => true)
    renderMenu({
      projects: [],
      hasChecklist: true,
      hasRecurrence: true,
      onDeleteIssue,
    })

    await fireEvent.click(screen.getByRole('button', { name: 'More actions' }))
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Delete issue' }))

    const dialog = screen.getByRole('dialog', { name: 'Delete issue' })
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Delete' }))

    expect(onDeleteIssue).toHaveBeenCalledTimes(1)
  })

  it('hides the trigger when no actions are available', () => {
    renderMenu({
      issue: makeIssue('closed'),
      projects: [],
      hasChecklist: true,
      hasRecurrence: true,
    })

    expect(screen.queryByRole('button', { name: 'More actions' })).toBeNull()
  })
})
