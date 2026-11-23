import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte'
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

let openPicker: () => Promise<void>

function renderMenu(overrides: Partial<MenuProps> = {}) {
  const view = render(MoveIssueDialog, {
    props: {
      issue: makeIssue(),
      projects,
      onMoveIssue: vi.fn(async () => true),
      ...overrides,
    },
  })
  openPicker = () => view.component.open()
  return view
}

async function openMovePicker(): Promise<void> {
  await openPicker()
}

describe('MoveIssueDialog', () => {
  afterEach(() => {
    cleanup()
  })

  it('names the task', async () => {
    renderMenu({ issue: makeIssue('open') })
    await openMovePicker()
    const dialog = screen.getByRole('dialog', { name: 'Move task' })
    expect(within(dialog).getByText('Ship the thing')).toBeTruthy()
    expect(within(dialog).getByText('INBOX-1')).toBeTruthy()
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
  })

  it('updates destinations reactively and retains the pending destination snapshot', async () => {
    let finishMove!: (moved: boolean) => void
    const pendingMove = new Promise<boolean>((resolve) => {
      finishMove = resolve
    })
    const onMoveIssue = vi.fn().mockReturnValueOnce(pendingMove).mockResolvedValueOnce(true)
    const view = renderMenu({ onMoveIssue })

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

    await view.rerender({ issue: makeIssue('open', { uid: 'issue-2' }) })
    await view.rerender({ issue: makeIssue() })
    await openMovePicker()
    expect(screen.queryByRole('dialog', { name: 'Move task' })).toBeNull()

    finishMove(false)
    await pendingMove
    await view.rerender({
      projects: projects.filter((project) => project.uid !== 'project-roadmap'),
    })
    await openMovePicker()
    expect(screen.queryByRole('button', { name: /Roadmap 2027/ })).toBeNull()
    expect(screen.getByRole('dialog', { name: 'Move task' })).toBeTruthy()
    await fireEvent.click(screen.getByRole('button', { name: /Shared.*Work/ }))
    expect(onMoveIssue).toHaveBeenLastCalledWith('project-shared-work')
    expect(onMoveIssue).toHaveBeenCalledTimes(2)
  })

  it('clears a move search before Escape dismisses the picker', async () => {
    renderMenu()
    await openMovePicker()

    const search = screen.getByRole('searchbox', { name: 'Find project' })
    await fireEvent.input(search, { target: { value: 'road' } })
    await fireEvent.keyDown(search, { key: 'Escape' })

    expect((search as HTMLInputElement).value).toBe('')
    expect(screen.getByRole('dialog', { name: 'Move task' })).toBeTruthy()

    await fireEvent.keyDown(search, { key: 'Escape' })
    expect(screen.queryByRole('dialog', { name: 'Move task' })).toBeNull()
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
    await fireEvent.keyDown(screen.getByRole('dialog', { name: 'Move task' }), {
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
})
