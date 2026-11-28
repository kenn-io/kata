import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte'
import type { ComponentProps } from 'svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { dismissFlash } from '@kenn-io/kit-ui'

import type { KataTaskSummary } from '../lib/kata/types'
import { maxDoneLimit } from '../lib/kata/board'
import IssueBoard from './IssueBoard.svelte'

function task(overrides: Partial<KataTaskSummary>): KataTaskSummary {
  return {
    id: 1,
    uid: 'issue-uid',
    project_id: 7,
    project_uid: 'project-example',
    project_name: 'example-project',
    short_id: 'abc1',
    qualified_id: 'example-project#abc1',
    title: 'Untitled',
    status: 'open',
    metadata: {},
    revision: 1,
    author: 'user-a',
    labels: [],
    created_at: '2026-05-01T00:00:00Z',
    updated_at: '2026-05-01T00:00:00Z',
    ...overrides,
  }
}

// One card per state: Open (unclaimed), In progress (owned), Needs attention
// (an agent asked for a human), and Done (closed).
const triage = task({ id: 1, uid: 'u-triage', short_id: 'tri1', title: 'Triage inbox' })
const doing = task({
  id: 2,
  uid: 'u-doing',
  short_id: 'doi1',
  title: 'Write the parser',
  labels: ['bug'],
  priority: 1,
  owner: 'user-b',
})
const waiting = task({
  id: 4,
  uid: 'u-waiting',
  short_id: 'wai1',
  title: 'Choose a schema',
  owner: 'agent-a',
  metadata: { 'work.attention': 'needs-human', 'work.attention_msg': 'Pick one of two designs' },
})
const shipped = task({
  id: 3,
  uid: 'u-shipped',
  short_id: 'shp1',
  title: 'Ship the release',
  status: 'closed',
  closed_at: '2026-05-03T00:00:00Z',
})

type Props = ComponentProps<typeof IssueBoard>

function baseProps(overrides: Partial<Props> = {}): Props {
  return {
    issues: [triage, doing, waiting, shipped],
    selectedIssueUID: null,
    showProject: false,
    onOpenIssue: vi.fn(),
    onMoveIssue: vi.fn(async () => true),
    onCompleteIssue: vi.fn(async () => true),
    ...overrides,
  }
}

function renderBoard(overrides: Partial<Props> = {}) {
  const props = baseProps(overrides)
  render(IssueBoard, { props })
  return props
}

// The app refreshes the board after a move, so these tests can hand the moved
// card back to the board the way that refresh does.
function renderMoving(moved: KataTaskSummary, overrides: Partial<Props> = {}) {
  const view = render(IssueBoard, { props: baseProps(overrides) })
  const issues: readonly KataTaskSummary[] = baseProps(overrides).issues
  let moves = 0
  const onMoveIssue = vi.fn(async (issue: KataTaskSummary): Promise<boolean> => {
    moves += 1
    if (moves === 1) {
      await view.rerender({
        issues: issues.map((candidate) => (candidate.uid === issue.uid ? moved : candidate)),
      })
    }
    return true
  })
  void view.rerender({ onMoveIssue })
  return { view, onMoveIssue }
}

function column(name: string): HTMLElement {
  return screen.getByRole('region', { name: new RegExp(`^${name},`) })
}

function cardButton(title: string): HTMLElement {
  return screen.getByRole('button', { name: title })
}

function dragTo(cardTitle: string, target: string, uid: string): Promise<void> {
  const transfer = { setData: vi.fn(), getData: () => uid, effectAllowed: '', dropEffect: '' }
  return (async () => {
    await fireEvent.dragStart(cardButton(cardTitle).closest('li')!, { dataTransfer: transfer })
    await fireEvent.dragOver(column(target), { dataTransfer: transfer })
    await fireEvent.drop(column(target), { dataTransfer: transfer })
  })()
}

afterEach(() => {
  cleanup()
  dismissFlash()
})

describe('IssueBoard columns', () => {
  it('shows the four states Kata records, with counts', () => {
    renderBoard()

    expect(
      within(screen.getByRole('region', { name: 'Issue board' }))
        .getAllByRole('region')
        .map((region) => region.getAttribute('aria-label')),
    ).toEqual(['Open, 1 card', 'In progress, 1 card', 'Needs attention, 1 card', 'Done, 1 card'])
    expect(within(column('Open')).getByText('Triage inbox')).toBeTruthy()
    expect(within(column('In progress')).getByText('Write the parser')).toBeTruthy()
    expect(within(column('Needs attention')).getByText('Choose a schema')).toBeTruthy()
    expect(within(column('Done')).getByText('Ship the release')).toBeTruthy()
  })

  it('shows priority, owner, and labels on the card', () => {
    renderBoard()

    const card = within(column('In progress')).getByRole('listitem')
    expect(within(card).getByText('P1')).toBeTruthy()
    expect(within(card).getByText('user-b')).toBeTruthy()
    expect(within(card).getByText('bug')).toBeTruthy()
  })

  it('says why a card needs attention', () => {
    const review = task({
      id: 5,
      uid: 'u-review',
      title: 'Review the docs',
      labels: ['needs-review'],
    })
    renderBoard({ issues: [waiting, review] })

    const waitingCard = cardButton('Choose a schema').closest('li')!
    expect(within(waitingCard).getByText('Needs you')).toBeTruthy()
    expect(within(waitingCard).getByText('Needs a human: Pick one of two designs')).toBeTruthy()
    const reviewCard = cardButton('Review the docs').closest('li')!
    expect(within(reviewCard).getByText('Review')).toBeTruthy()
    expect(within(reviewCard).queryByText('needs-review')).toBeNull()
    expect(
      screen.getByRole('button', {
        name: 'Choose a schema',
        description: /Pick one of two designs/,
      }),
    ).toBeTruthy()
  })

  it('keeps blocked and parked work in its own column and says why it is not ready', () => {
    const blocked = task({
      id: 6,
      uid: 'u-blocked',
      title: 'Ship after review',
      open_blocked_by: [
        { uid: 'u-review', short_id: 'rev1', qualified_id: 'example-project#rev1' },
      ],
    })
    const someday = task({
      id: 7,
      uid: 'u-someday',
      title: 'Try a dark theme',
      owner: 'agent-a',
      metadata: { someday: true },
    })
    renderBoard({ issues: [blocked, someday] })

    expect(
      within(within(column('Open')).getByRole('listitem')).getByText('Waiting on rev1'),
    ).toBeTruthy()
    expect(
      within(within(column('In progress')).getByRole('listitem')).getByText('Someday'),
    ).toBeTruthy()
    expect(column('Needs attention').getAttribute('aria-label')).toBe('Needs attention, 0 cards')
  })

  it("shows only requests in the viewer's inbox", () => {
    const asked = task({
      id: 8,
      uid: 'u-asked',
      title: 'Pick a release date',
      metadata: { 'notify.Y29vcmRpbmF0b3I': { from: 'agent-a', message: 'Pick a date' } },
    })
    renderBoard({ issues: [asked], viewer: { actor: 'reviewer', inbox: 'reviewer' } })
    expect(within(column('Open')).getByText('Pick a release date')).toBeTruthy()
    cleanup()

    renderBoard({ issues: [asked], viewer: { actor: 'coordinator', inbox: 'coordinator' } })
    expect(within(column('Needs attention')).getByText('Pick a release date')).toBeTruthy()
  })

  it('opens the issue when its card is clicked', async () => {
    const props = renderBoard()

    await fireEvent.click(cardButton('Triage inbox'))

    expect(props.onOpenIssue).toHaveBeenCalledWith(triage)
  })

  it('moves focus between cards and columns with the arrow keys', async () => {
    const second = task({ id: 9, uid: 'u-two', title: 'Second open' })
    renderBoard({ issues: [triage, second, doing] })

    cardButton('Triage inbox').focus()
    await fireEvent.keyDown(cardButton('Triage inbox'), { key: 'ArrowDown' })
    expect(document.activeElement).toBe(cardButton('Second open'))
    await fireEvent.keyDown(cardButton('Second open'), { key: 'ArrowRight' })
    expect(document.activeElement).toBe(cardButton('Write the parser'))
  })
})

describe('IssueBoard column strip', () => {
  it('lists each column with its count and marks the one in view', () => {
    renderBoard()

    const strip = screen.getByRole('navigation', { name: 'Board columns' })
    const buttons = within(strip).getAllByRole('button')
    expect(buttons.map((button) => button.textContent?.replace(/\s+/g, ' ').trim())).toEqual([
      'Open 1',
      'In progress 1',
      'Needs attention 1',
      'Done 1',
    ])
    expect(buttons.map((button) => button.getAttribute('aria-current'))).toEqual([
      'true',
      null,
      null,
      null,
    ])
  })

  it('brings a column into view when its name is tapped', async () => {
    const scrolled: [Element, ScrollIntoViewOptions | boolean | undefined][] = []
    const scrollIntoView = vi
      .spyOn(Element.prototype, 'scrollIntoView')
      .mockImplementation(function (this: Element, options) {
        scrolled.push([this, options])
      })
    renderBoard()

    const strip = screen.getByRole('navigation', { name: 'Board columns' })
    await fireEvent.click(within(strip).getByRole('button', { name: 'Done 1' }))

    // The strip also scrolls its own names; only the Done column moves the board.
    const columns = scrolled.filter(([element]) => element.matches('section'))
    expect(columns).toEqual([
      [column('Done'), expect.objectContaining({ inline: 'start', block: 'nearest' })],
    ])
    expect(within(strip).getByRole('button', { name: 'Done 1' }).getAttribute('aria-current')).toBe(
      'true',
    )
    scrollIntoView.mockRestore()
  })
})

describe('IssueBoard moves', () => {
  it('claims a card moved from Open to In progress with Alt+ArrowRight', async () => {
    const props = renderBoard()

    await fireEvent.keyDown(cardButton('Triage inbox'), { key: 'ArrowRight', altKey: true })

    expect(props.onMoveIssue).toHaveBeenCalledWith(triage, { claim: true })
    await waitFor(() => expect(screen.getByText('Moved Triage inbox to In progress.')).toBeTruthy())
  })

  it('releases a card moved from In progress back to Open', async () => {
    const props = renderBoard()

    await fireEvent.keyDown(cardButton('Write the parser'), { key: 'ArrowLeft', altKey: true })

    expect(props.onMoveIssue).toHaveBeenCalledWith(doing, { release: true })
  })

  it('keeps the card in its target column while the move is pending', async () => {
    let finish: (value: boolean) => void = () => {}
    renderBoard({
      onMoveIssue: vi.fn(() => new Promise<boolean>((resolve) => (finish = resolve))),
    })

    await fireEvent.keyDown(cardButton('Triage inbox'), { key: 'ArrowRight', altKey: true })

    expect(within(column('In progress')).getByText('Triage inbox')).toBeTruthy()
    finish(false)
    await waitFor(() => expect(within(column('Open')).getByText('Triage inbox')).toBeTruthy())
    // An earlier step of the move may have saved, so claim nothing about where
    // the card is; the refreshed board shows that.
    expect(screen.getByText('Could not finish moving Triage inbox.')).toBeTruthy()
    expect(screen.queryByText(/back in/)).toBeNull()
  })

  it('asks the owner from the card menu without moving the card', async () => {
    const onAskIssue = vi.fn(async () => true)
    const props = renderBoard({ onAskIssue })

    await fireEvent.click(screen.getByRole('button', { name: 'Actions for Write the parser' }))
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Ask user-b…' }))
    const dialog = screen.getByRole('dialog', { name: 'Ask user-b' })
    const send = within(dialog).getByRole('button', { name: 'Send request' })
    expect(send.hasAttribute('disabled')).toBe(true)
    // kata keeps one request per person on a task, so asking again replaces it.
    expect(
      within(dialog).getByText(/replaces any earlier request to user-b on this task/),
    ).toBeTruthy()
    await fireEvent.input(within(dialog).getByRole('textbox', { name: 'What do you need?' }), {
      target: { value: '  Which parser API should ship?  ' },
    })
    await fireEvent.click(send)

    expect(onAskIssue).toHaveBeenCalledWith(doing, 'user-b', 'Which parser API should ship?')
    expect(props.onMoveIssue).not.toHaveBeenCalled()
    expect(await screen.findByText('Asked user-b about Write the parser.')).toBeTruthy()
    expect(within(column('In progress')).getByText('Write the parser')).toBeTruthy()
  })

  it('asks the author when nobody owns the card, and never offers to ask yourself', async () => {
    renderBoard({ onAskIssue: vi.fn(async () => true), viewer: { actor: 'user-b' } })

    await fireEvent.click(screen.getByRole('button', { name: 'Actions for Triage inbox' }))
    expect(screen.getByRole('menuitem', { name: 'Ask user-a…' })).toBeTruthy()
    await fireEvent.keyDown(screen.getByRole('menu'), { key: 'Escape' })
    await fireEvent.click(screen.getByRole('button', { name: 'Actions for Write the parser' }))
    expect(screen.queryByRole('menuitem', { name: /^Ask/ })).toBeNull()
  })

  it('clears attention when a card leaves Needs attention for In progress', async () => {
    const props = renderBoard()

    await fireEvent.keyDown(cardButton('Choose a schema'), { key: 'ArrowLeft', altKey: true })

    expect(props.onMoveIssue).toHaveBeenCalledWith(waiting, {
      clearAttention: {
        metadata: { 'work.attention': 'ok', 'work.attention_msg': null },
        removeLabels: [],
      },
    })
  })

  it('says that moving a card out of Needs attention marks it handled', async () => {
    renderBoard()

    expect(
      screen.getByRole('button', {
        name: 'Choose a schema',
        description: /Moving it out of Needs attention marks it handled\./,
      }),
    ).toBeTruthy()
    await fireEvent.click(screen.getByRole('button', { name: 'Actions for Choose a schema' }))
    expect(
      within(screen.getByRole('menu'))
        .getAllByRole('menuitem')
        .map((item) => item.textContent?.trim()),
    ).toContain('Mark handled and move to In progress')
    await fireEvent.click(
      screen.getByRole('menuitem', { name: 'Mark handled and move to In progress' }),
    )
    await screen.findByText('Moved Choose a schema to In progress and marked it handled.')
  })

  it('never moves a card into Needs attention: Alt+→ skips it and it takes no drops', async () => {
    const props = renderBoard()

    await fireEvent.keyDown(cardButton('Write the parser'), { key: 'ArrowRight', altKey: true })
    expect(screen.getByRole('dialog', { name: 'Complete task' })).toBeTruthy()
    await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))

    await dragTo('Triage inbox', 'Needs attention', 'u-triage')
    expect(props.onMoveIssue).not.toHaveBeenCalled()
    expect(within(column('Open')).getByText('Triage inbox')).toBeTruthy()
  })

  it('asks for a completion note before a card dropped on Done closes', async () => {
    const props = renderBoard()

    await dragTo('Triage inbox', 'Done', 'u-triage')

    expect(props.onMoveIssue).not.toHaveBeenCalled()
    const dialog = screen.getByRole('dialog', { name: 'Complete task' })
    await fireEvent.click(within(dialog).getByRole('radio', { name: /Won't do/ }))
    await fireEvent.input(within(dialog).getByRole('textbox'), {
      target: { value: 'No longer needed because the inbox flow was replaced last week.' },
    })
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Complete' }))

    expect(props.onCompleteIssue).toHaveBeenCalledWith(triage, {
      reason: 'wontfix',
      message: 'No longer needed because the inbox flow was replaced last week.',
      evidence: [],
    })
  })

  it('keeps the completion note when the daemon refuses the close', async () => {
    const note = 'No longer needed because the inbox flow was replaced last week.'
    const props = renderBoard({ onCompleteIssue: vi.fn(async () => false) })

    await dragTo('Triage inbox', 'Done', 'u-triage')
    const dialog = screen.getByRole('dialog', { name: 'Complete task' })
    await fireEvent.click(within(dialog).getByRole('radio', { name: /Won't do/ }))
    await fireEvent.input(within(dialog).getByRole('textbox'), { target: { value: note } })
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Complete' }))

    await waitFor(() => expect(props.onCompleteIssue).toHaveBeenCalledOnce())
    const stillOpen = screen.getByRole('dialog', { name: 'Complete task' })
    expect((within(stillOpen).getByRole('textbox') as HTMLTextAreaElement).value).toBe(note)
    expect(within(column('Open')).getByText('Triage inbox')).toBeTruthy()
  })

  it('closes the completion form once the daemon accepts the close', async () => {
    renderBoard()

    await dragTo('Triage inbox', 'Done', 'u-triage')
    const dialog = screen.getByRole('dialog', { name: 'Complete task' })
    await fireEvent.click(within(dialog).getByRole('radio', { name: /Won't do/ }))
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Complete' }))

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Complete task' })).toBeNull())
  })

  it('reopens and claims a Done card dragged into In progress', async () => {
    const props = renderBoard()

    await dragTo('Ship the release', 'In progress', 'u-shipped')

    expect(props.onMoveIssue).toHaveBeenCalledWith(shipped, { reopen: true, claim: true })
  })

  it('offers the columns a card can move to, then Ask, from the card menu', async () => {
    const props = renderBoard({ onAskIssue: vi.fn(async () => true) })

    await fireEvent.click(screen.getByRole('button', { name: 'Actions for Triage inbox' }))
    const menu = screen.getByRole('menu')
    expect(
      within(menu)
        .getAllByRole('menuitem')
        .map((item) => item.textContent?.trim()),
    ).toEqual(['Move to In progress', 'Move to Done', 'Ask user-a…'])
    await fireEvent.click(within(menu).getByRole('menuitem', { name: 'Move to In progress' }))

    expect(props.onMoveIssue).toHaveBeenCalledWith(triage, { claim: true })
  })

  it('starts no move, ask, or add while another change is still saving', async () => {
    const onAskIssue = vi.fn(async () => true)
    const onAddCard = vi.fn(async () => true)
    const props = renderBoard({ busy: true, onAskIssue, onAddCard })

    await fireEvent.keyDown(cardButton('Triage inbox'), { key: 'ArrowRight', altKey: true })
    await dragTo('Triage inbox', 'In progress', 'u-triage')
    expect(props.onMoveIssue).not.toHaveBeenCalled()
    expect(
      screen.getByRole('button', { name: 'Actions for Triage inbox' }).hasAttribute('disabled'),
    ).toBe(true)
    await fireEvent.click(within(column('Open')).getByRole('button', { name: 'Add card to Open' }))
    await fireEvent.input(
      within(column('Open')).getByRole('textbox', { name: 'New card in Open' }),
      {
        target: { value: 'Waits for the save' },
      },
    )
    expect(
      within(column('Open')).getByRole('button', { name: 'Add' }).hasAttribute('disabled'),
    ).toBe(true)
    expect(onAddCard).not.toHaveBeenCalled()
  })

  it('does not move cards and says why when changes are blocked', async () => {
    const props = renderBoard({ moveBlockedReason: 'This credential is read-only.' })

    await fireEvent.keyDown(cardButton('Triage inbox'), { key: 'ArrowRight', altKey: true })

    expect(props.onMoveIssue).not.toHaveBeenCalled()
    expect(screen.queryByRole('button', { name: 'Actions for Triage inbox' })).toBeNull()
    expect(within(column('Open')).getByRole('listitem').getAttribute('draggable')).toBe('false')
    expect(screen.getByText('This credential is read-only.')).toBeTruthy()
  })
})

describe('IssueBoard after a move', () => {
  it('briefly highlights the card where it landed', async () => {
    renderMoving({ ...triage, owner: 'kata-web' })

    await fireEvent.keyDown(cardButton('Triage inbox'), { key: 'ArrowRight', altKey: true })

    await waitFor(() =>
      expect(
        within(column('In progress')).getByRole('button', { name: 'Triage inbox' }).closest('li')!
          .classList,
      ).toContain('just-moved'),
    )
  })

  it('confirms a move without Undo, and leaves Ctrl+Z to the browser', async () => {
    // Every move is one kata action; moving the card back reverses it.
    const { onMoveIssue } = renderMoving({ ...triage, owner: 'kata-web' })

    await fireEvent.keyDown(cardButton('Triage inbox'), { key: 'ArrowRight', altKey: true })
    await screen.findByText('Moved Triage inbox to In progress.')
    expect(screen.queryByRole('button', { name: 'Undo' })).toBeNull()
    await fireEvent.keyDown(cardButton('Triage inbox'), { key: 'z', ctrlKey: true })

    expect(onMoveIssue).toHaveBeenCalledOnce()
  })
})

describe('IssueBoard epics and subtasks', () => {
  const epic = task({
    id: 20,
    uid: 'u-epic',
    short_id: 'epc1',
    title: 'Launch the board',
    priority: 2,
    child_counts: { open: 2, total: 3 },
  })
  const firstChild = task({
    id: 21,
    uid: 'u-child-a',
    title: 'Draw the columns',
    priority: 1,
    parent: { uid: 'u-epic', short_id: 'epc1' },
  })
  const elsewhereChild = task({
    id: 23,
    uid: 'u-child-c',
    title: 'Review the copy',
    owner: 'user-b',
    parent: { uid: 'u-epic', short_id: 'epc1' },
  })

  it('marks epic cards and lists their subtasks underneath in the same column', () => {
    renderBoard({ issues: [firstChild, epic, elsewhereChild] })

    const cards = within(column('Open')).getAllByRole('listitem')
    expect(cards.map((card) => card.querySelector('.card-title')?.textContent)).toEqual([
      'Launch the board',
      'Draw the columns',
    ])
    expect(within(cards[0]!).getByText('Epic')).toBeTruthy()
    expect(cards.map((card) => card.getAttribute('data-depth'))).toEqual(['0', '1'])
    expect(
      screen.getByRole('button', {
        name: 'Draw the columns',
        description: /Subtask of Launch the board/,
      }),
    ).toBeTruthy()
  })

  it('shows which epic a subtask belongs to when the epic is in another column', () => {
    renderBoard({ issues: [epic, elsewhereChild] })

    const card = within(column('In progress')).getByRole('listitem')
    expect(card.getAttribute('data-depth')).toBe('0')
    expect(within(card).getByText('epc1').closest('.card-parent')).toBeTruthy()
  })
})

describe('IssueBoard Done paging', () => {
  const closed = Array.from({ length: 35 }, (_, n) =>
    task({
      id: 100 + n,
      uid: `u-closed-${n}`,
      title: `Closed ${n}`,
      status: 'closed',
      closed_at: `2026-04-01T00:${String(n).padStart(2, '0')}:00Z`,
    }),
  )

  it('shows only the newest Done cards until asked for more', async () => {
    // Given oldest first, as an older daemon without closed paging sends them.
    renderBoard({ issues: closed })

    const shown = within(column('Done'))
      .getAllByRole('listitem')
      .map((card) => card.querySelector('.card-title')?.textContent)
    expect(shown).toHaveLength(30)
    expect(shown[0]).toBe('Closed 34')
    expect(shown).not.toContain('Closed 4')
    await fireEvent.click(screen.getByRole('button', { name: 'Show 5 more' }))
    expect(within(column('Done')).getAllByRole('listitem')).toHaveLength(35)
  })

  it('reports the server total and asks the server for the next page', async () => {
    const onShowMoreDone = vi.fn()
    renderBoard({ issues: closed.slice(0, 30), doneTotal: 75, onShowMoreDone })

    expect(column('Done').getAttribute('aria-label')).toBe('Done, 75 cards')
    await fireEvent.click(screen.getByRole('button', { name: 'Show 30 more' }))
    expect(onShowMoreDone).toHaveBeenCalledOnce()
  })

  it('points to Logbook once Done holds the most closed cards the daemon pages', async () => {
    const page = Array.from({ length: maxDoneLimit }, (_, n) =>
      task({ id: 1000 + n, uid: `u-page-${n}`, title: `Paged ${n}`, status: 'closed' }),
    )
    const onShowMoreDone = vi.fn()
    const onOpenLogbook = vi.fn()
    renderBoard({ issues: page, doneTotal: maxDoneLimit + 250, onShowMoreDone, onOpenLogbook })

    expect(screen.queryByRole('button', { name: /^Show \d+ more$/ })).toBeNull()
    expect(within(column('Done')).getByText(/Showing the newest 1,000/)).toBeTruthy()
    await fireEvent.click(within(column('Done')).getByRole('button', { name: 'Open Logbook' }))
    expect(onOpenLogbook).toHaveBeenCalledOnce()
    expect(onShowMoreDone).not.toHaveBeenCalled()
  }, 20_000)
})

describe('IssueBoard density, collapsing, and comments', () => {
  it("describes each card's ID, and its project on boards that span projects", () => {
    renderBoard({ compact: true, showProject: true })

    expect(
      screen.getByRole('button', {
        name: 'Triage inbox',
        description: /example-project#abc1.*project example-project/,
      }),
    ).toBeTruthy()
  })

  it.each([false, true])('describes why a card needs attention (compact: %s)', (compact) => {
    renderBoard({ compact })

    expect(
      within(column('Needs attention')).getByRole('button', {
        name: 'Choose a schema',
        description: /Needs a human: Pick one of two designs/,
      }),
    ).toBeTruthy()
  })

  it('keeps ID, priority, and title on one line in compact mode', () => {
    renderBoard({ compact: true })

    const card = within(column('In progress')).getByRole('listitem')
    expect(within(card).getByText('doi1')).toBeTruthy()
    expect(within(card).getByText('P1')).toBeTruthy()
    expect(within(card).queryByText('user-b')).toBeNull()
  })

  it('says what each empty column is missing, and offers collapse only where it saves space', () => {
    renderBoard({ issues: [], onToggleColumn: vi.fn() })

    expect(within(column('Open')).getByText('No open tasks')).toBeTruthy()
    expect(within(column('In progress')).getByText('Nothing claimed')).toBeTruthy()
    expect(within(column('Needs attention')).getByText('Nothing needs you')).toBeTruthy()
    expect(within(column('Done')).getByText('Nothing done yet')).toBeTruthy()
    // With every column empty, narrowing three of them would hide nothing.
    expect(
      screen.getAllByRole('button', { name: /^Collapse/ }).map((button) => button.textContent),
    ).toHaveLength(1)
    expect(screen.getByRole('button', { name: 'Collapse Done' })).toBeTruthy()
  })

  it('offers to collapse Done and empty columns, and a collapsed column still takes drops', async () => {
    const props = renderBoard({
      issues: [triage, shipped],
      collapsedColumns: ['done'],
      onToggleColumn: vi.fn(),
    })

    expect(
      within(column('In progress')).getByRole('button', { name: 'Collapse In progress' }),
    ).toBeTruthy()
    expect(within(column('Open')).queryByRole('button', { name: /^Collapse/ })).toBeNull()
    expect(within(column('Done')).queryAllByRole('listitem')).toHaveLength(0)
    await fireEvent.click(within(column('Done')).getByRole('button', { name: 'Expand Done' }))
    expect(props.onToggleColumn).toHaveBeenCalledWith('done')

    await dragTo('Triage inbox', 'Done', 'u-triage')
    expect(screen.getByRole('dialog', { name: 'Complete task' })).toBeTruthy()
  })

  it('shows a comment count only on cards that have comments', () => {
    renderBoard({ issues: [{ ...triage, comment_count: 3 }, doing] })

    expect(
      cardButton('Triage inbox').closest('li')!.querySelector('.comment-pill')?.textContent,
    ).toContain('3')
    expect(
      screen.getByRole('button', { name: 'Triage inbox', description: /3 comments/ }),
    ).toBeTruthy()
    expect(cardButton('Write the parser').closest('li')!.querySelector('.comment-pill')).toBeNull()
  })
})

describe('IssueBoard remote changes', () => {
  const cardOf = (title: string) => cardButton(title).closest('li')!

  it('flashes a card someone else moved or added, but not on first load', async () => {
    const view = render(IssueBoard, { props: baseProps({ scopeKey: 'project-example' }) })
    expect(cardOf('Triage inbox').classList).not.toContain('remote-change')

    const added = task({ id: 50, uid: 'u-added', title: 'Added by an agent' })
    await view.rerender({
      issues: [{ ...triage, owner: 'agent-a' }, doing, waiting, shipped, added],
    })

    expect(within(column('In progress')).getByRole('button', { name: 'Triage inbox' })).toBeTruthy()
    expect(cardOf('Triage inbox').classList).toContain('remote-change')
    expect(cardOf('Added by an agent').classList).toContain('remote-change')
    expect(cardOf('Write the parser').classList).not.toContain('remote-change')
  })

  it('flashes nothing when the highlight is off or the board switches scope', async () => {
    const view = render(IssueBoard, {
      props: baseProps({ scopeKey: 'project-example', highlightRemoteChanges: false }),
    })
    await view.rerender({ issues: [{ ...triage, owner: 'agent-a' }, doing, waiting, shipped] })
    expect(cardOf('Triage inbox').classList).not.toContain('remote-change')

    await view.rerender({
      highlightRemoteChanges: true,
      scopeKey: 'project-other',
      issues: [task({ id: 60, uid: 'u-other', title: 'Other project card' })],
    })
    expect(cardOf('Other project card').classList).not.toContain('remote-change')
  })

  it('does not flash a card the viewer moved', async () => {
    renderMoving({ ...triage, owner: 'kata-web' }, { scopeKey: 'project-example' })

    await fireEvent.keyDown(cardButton('Triage inbox'), { key: 'ArrowRight', altKey: true })

    await waitFor(() => expect(cardOf('Triage inbox').classList).toContain('just-moved'))
    expect(cardOf('Triage inbox').classList).not.toContain('remote-change')
  })
})

describe('IssueBoard add card', () => {
  it('adds a card from the Open column and stays open for the next one', async () => {
    const onAddCard = vi.fn(async () => true)
    renderBoard({ onAddCard })

    expect(within(column('In progress')).queryByRole('button', { name: /^Add card/ })).toBeNull()
    await fireEvent.click(within(column('Open')).getByRole('button', { name: 'Add card to Open' }))
    const input = within(column('Open')).getByRole('textbox', { name: 'New card in Open' })
    expect(document.activeElement).toBe(input)
    await fireEvent.input(input, { target: { value: '  Write release notes  ' } })
    await fireEvent.keyDown(input, { key: 'Enter' })

    expect(onAddCard).toHaveBeenCalledWith('Write release notes')
    await waitFor(() => expect((input as HTMLTextAreaElement).value).toBe(''))
  })

  it('keeps the draft when adding fails, and Escape closes the form', async () => {
    const onAddCard = vi.fn(async () => false)
    renderBoard({ onAddCard })

    await fireEvent.click(within(column('Open')).getByRole('button', { name: 'Add card to Open' }))
    const input = within(column('Open')).getByRole('textbox', { name: 'New card in Open' })
    await fireEvent.input(input, { target: { value: 'Keep me' } })
    await fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => expect(onAddCard).toHaveBeenCalledOnce())
    expect((input as HTMLTextAreaElement).value).toBe('Keep me')

    await fireEvent.keyDown(input, { key: 'Escape' })
    expect(within(column('Open')).queryByRole('textbox')).toBeNull()
  })
})
