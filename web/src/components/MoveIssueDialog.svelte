<script lang="ts">
  /* eslint-disable svelte/prefer-svelte-reactivity */
  import { IconButton, SearchInput } from '@kenn-io/kit-ui'
  import MoreHorizontalIcon from '@lucide/svelte/icons/more-horizontal'
  import { tick } from 'svelte'
  import type { KataProjectSummary, KataTaskDetail } from '../lib/kata/types'
  import DeleteIssueDialog from './DeleteIssueDialog.svelte'

  interface Props {
    issue: KataTaskDetail
    projects: KataProjectSummary[]
    hasChecklist: boolean
    hasRecurrence: boolean
    movePending?: boolean | undefined
    onMoveIssue: (toProjectUID: string) => boolean | Promise<boolean>
    onAddChecklist: () => void
    onCreateRecurrence: () => void
    onDeleteIssue: () => boolean | Promise<boolean>
  }

  let {
    issue,
    projects,
    hasChecklist,
    hasRecurrence,
    movePending = false,
    onMoveIssue,
    onAddChecklist,
    onCreateRecurrence,
    onDeleteIssue,
  }: Props = $props()

  let menuOpen = $state(false)
  let menuView = $state<'actions' | 'move'>('actions')
  let menuRoot: HTMLDivElement | null = $state(null)
  let moveSearchInput = $state() as HTMLInputElement
  let moveQuery = $state('')
  let pendingMoves = $state.raw(
    new Map<string, { operation: number; project: KataProjectSummary }>(),
  )
  let interactionGeneration = 0
  let moveOperationGeneration = 0
  let deleteOpen = $state(false)
  let trackedUID = $state<string | null>(null)

  const activeMove = $derived(pendingMoves.get(issue.issue.uid) ?? null)
  const activeMoveOperation = $derived(activeMove?.operation ?? null)
  const movingProjectUID = $derived(activeMove?.project.uid ?? null)
  const pendingProject = $derived(activeMove?.project ?? null)
  const canAddChecklist = $derived(!hasChecklist)
  const canCreateRecurrence = $derived(!hasRecurrence)
  const canDeleteIssue = $derived(issue.issue.status !== 'closed')
  const eligibleProjects = $derived.by(() =>
    projects
      .filter(
        (project) => project.uid !== issue.issue.project_uid && project.metadata.role !== 'inbox',
      )
      .toSorted(
        (a, b) =>
          a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }) ||
          a.uid.localeCompare(b.uid),
      ),
  )
  const duplicateContexts = $derived.by(() => {
    const groups = new Map<string, KataProjectSummary[]>()
    for (const project of eligibleProjects) {
      const key = project.name.trim().toLocaleLowerCase()
      groups.set(key, [...(groups.get(key) ?? []), project])
    }

    const contexts = new Map<string, string>()
    for (const group of groups.values()) {
      if (group.length < 2) continue
      const areaCounts = new Map<string, number>()
      for (const project of group) {
        const area = projectArea(project)
        if (area) areaCounts.set(area, (areaCounts.get(area) ?? 0) + 1)
      }
      for (const project of group) {
        const area = projectArea(project)
        contexts.set(project.uid, area && areaCounts.get(area) === 1 ? area : project.uid)
      }
    }
    return contexts
  })
  const displayedProjects = $derived.by(() => {
    const available = [...eligibleProjects]
    if (pendingProject && !available.some((project) => project.uid === pendingProject?.uid)) {
      available.push(pendingProject)
    }
    const query = moveQuery.trim().toLocaleLowerCase()
    return query
      ? available.filter((project) =>
          [project.name, destinationContext(project) ?? ''].some((value) =>
            value.toLocaleLowerCase().includes(query),
          ),
        )
      : available
  })
  const hasAnyAction = $derived(
    canAddChecklist || canCreateRecurrence || canDeleteIssue || eligibleProjects.length > 0,
  )

  $effect(() => {
    if (issue.issue.uid === trackedUID) return
    trackedUID = issue.issue.uid
    interactionGeneration += 1
    menuOpen = false
    menuView = 'actions'
    moveQuery = ''
    deleteOpen = false
  })

  $effect(() => {
    if (!menuOpen) return
    function onPointerDown(event: PointerEvent): void {
      if (!menuRoot || movePending || activeMoveOperation !== null) return
      if (event.target instanceof Node && menuRoot.contains(event.target)) return
      closeMenu()
    }
    window.addEventListener('pointerdown', onPointerDown, true)
    return () => {
      window.removeEventListener('pointerdown', onPointerDown, true)
    }
  })

  function projectArea(project: KataProjectSummary): string {
    return typeof project.metadata.area === 'string' ? project.metadata.area.trim() : ''
  }

  function destinationContext(project: KataProjectSummary): string | null {
    return duplicateContexts.get(project.uid) ?? null
  }

  function triggerButton(): HTMLButtonElement | null {
    return menuRoot?.querySelector<HTMLButtonElement>('.overflow-trigger') ?? null
  }

  function closeMenu(force = false, restoreFocus = false): void {
    if ((movePending || activeMoveOperation !== null) && !force) return
    interactionGeneration += 1
    menuOpen = false
    menuView = 'actions'
    moveQuery = ''
    if (restoreFocus) queueMicrotask(() => triggerButton()?.focus())
  }

  function revealChecklist(): void {
    closeMenu()
    onAddChecklist()
  }

  function openCreateRecurrence(): void {
    closeMenu()
    onCreateRecurrence()
  }

  async function openMovePicker(): Promise<void> {
    menuView = 'move'
    moveQuery = ''
    await tick()
    moveSearchInput?.focus()
  }

  function handleMoveKeydown(event: KeyboardEvent): void {
    if (event.key !== 'Escape' || movePending || activeMoveOperation !== null) return
    event.preventDefault()
    closeMenu(false, true)
  }

  async function moveIssue(project: KataProjectSummary): Promise<void> {
    const sourceIssueUID = issue.issue.uid
    if (pendingMoves.has(sourceIssueUID)) return
    const sourceInteraction = interactionGeneration
    const operation = ++moveOperationGeneration
    pendingMoves = new Map(pendingMoves).set(sourceIssueUID, { operation, project })
    try {
      const moved = await onMoveIssue(project.uid)
      if (
        pendingMoves.get(sourceIssueUID)?.operation !== operation ||
        interactionGeneration !== sourceInteraction ||
        issue.issue.uid !== sourceIssueUID
      ) {
        return
      }
      if (moved !== false) closeMenu(true)
    } finally {
      if (pendingMoves.get(sourceIssueUID)?.operation === operation) {
        const nextPendingMoves = new Map(pendingMoves)
        nextPendingMoves.delete(sourceIssueUID)
        pendingMoves = nextPendingMoves
      }
    }
  }

  function openDeleteDialog(): void {
    closeMenu()
    deleteOpen = true
  }
</script>

{#if hasAnyAction}
  <div class="overflow-host" bind:this={menuRoot} role="presentation">
    <IconButton
      class="overflow-trigger"
      ariaLabel="More actions"
      ariaHaspopup={menuView === 'move' ? 'dialog' : 'menu'}
      ariaExpanded={menuOpen}
      onclick={() => {
        if (movePending || activeMoveOperation !== null) return
        if (menuOpen) closeMenu()
        else menuOpen = true
      }}
    >
      <MoreHorizontalIcon size={14} strokeWidth={1.9} />
    </IconButton>
    {#if menuOpen && menuView === 'actions'}
      <ul class="overflow-menu kit-popover-card" role="menu" aria-label="Task actions">
        {#if eligibleProjects.length > 0}
          <li>
            <button
              type="button"
              class="overflow-item"
              role="menuitem"
              onclick={() => void openMovePicker()}
            >
              Move to another project
            </button>
          </li>
        {/if}
        {#if canAddChecklist}
          <li>
            <button type="button" class="overflow-item" role="menuitem" onclick={revealChecklist}>
              Add checklist
            </button>
          </li>
        {/if}
        {#if canCreateRecurrence}
          <li>
            <button
              type="button"
              class="overflow-item"
              role="menuitem"
              onclick={openCreateRecurrence}
            >
              Create recurrence...
            </button>
          </li>
        {/if}
        {#if canDeleteIssue}
          {#if eligibleProjects.length > 0 || canAddChecklist || canCreateRecurrence}
            <li class="overflow-separator" role="separator"></li>
          {/if}
          <li>
            <button
              type="button"
              class="overflow-item overflow-item--danger"
              role="menuitem"
              onclick={openDeleteDialog}
            >
              Delete issue
            </button>
          </li>
        {/if}
      </ul>
    {:else if menuOpen}
      <div
        class="move-picker kit-popover-card"
        role="dialog"
        aria-label="Move to another project"
        tabindex="-1"
        onkeydown={handleMoveKeydown}
      >
        <SearchInput
          bind:value={moveQuery}
          bind:inputEl={moveSearchInput}
          block
          ariaLabel="Find project"
          placeholder="Search projects"
        />
        <div class="move-options" aria-label="Project destinations">
          {#each displayedProjects as project (project.uid)}
            <button
              type="button"
              disabled={movePending || activeMoveOperation !== null}
              aria-busy={movingProjectUID === project.uid}
              onclick={() => void moveIssue(project)}
            >
              <span class="move-project-name">{project.name}</span>
              {#if destinationContext(project)}
                <span class="move-project-context">{destinationContext(project)}</span>
              {/if}
              <span class="move-project-count">{project.open_count}</span>
            </button>
          {:else}
            <p role="status">No matching projects</p>
          {/each}
        </div>
      </div>
    {/if}
  </div>
{/if}

<DeleteIssueDialog
  open={deleteOpen}
  {issue}
  onClose={() => {
    deleteOpen = false
  }}
  {onDeleteIssue}
/>

<style>
  .overflow-host {
    position: relative;
    display: inline-flex;
  }

  .overflow-menu,
  .move-picker {
    position: absolute;
    top: calc(100% + 6px);
    right: 0;
    z-index: 35;
    margin: 0;
  }

  .overflow-menu {
    min-width: 210px;
    padding: 5px;
    list-style: none;
  }

  .overflow-item {
    width: 100%;
    border-radius: 5px;
    padding: 7px 9px;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    text-align: left;
  }

  .overflow-item:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .overflow-item--danger {
    color: var(--accent-red);
  }

  .overflow-separator {
    height: 1px;
    margin: 5px 2px;
    background: var(--border-muted);
  }

  .move-picker {
    display: grid;
    gap: var(--space-3);
    width: min(320px, calc(100vw - 24px));
    padding: var(--space-4);
  }

  .move-options {
    display: grid;
    max-height: 280px;
    overflow-y: auto;
  }

  .move-options button {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    gap: 2px var(--space-3);
    width: 100%;
    padding: 7px 9px;
    border-radius: 5px;
    color: var(--text-secondary);
    text-align: left;
  }

  .move-options button:hover:not(:disabled) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .move-options button:disabled {
    opacity: 0.62;
  }

  .move-project-name {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .move-project-context {
    grid-column: 1;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .move-project-count {
    grid-column: 2;
    grid-row: 1 / span 2;
    align-self: center;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
  }

  .move-options p {
    margin: 0;
    padding: var(--space-4);
    color: var(--text-muted);
    font-size: var(--font-size-sm);
    text-align: center;
  }
</style>
