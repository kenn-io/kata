<script lang="ts">
  /* eslint-disable svelte/prefer-svelte-reactivity */
  import { autoReposition, floatingPopoverStyle, SearchInput } from '@kenn-io/kit-ui'
  import { tick } from 'svelte'
  import type { KataProjectSummary, KataTaskDetail } from '../lib/kata/types'

  interface Props {
    issue: KataTaskDetail
    projects: KataProjectSummary[]
    movePending?: boolean | undefined
    allowMove?: boolean | undefined
    onMoveIssue: (toProjectUID: string) => boolean | Promise<boolean>
  }

  let { issue, projects, movePending = false, allowMove = true, onMoveIssue }: Props = $props()

  let menuOpen = $state(false)
  let menuRoot: HTMLDivElement | null = $state(null)
  let trigger: HTMLElement | null = null
  let panel = $state<HTMLDivElement>()
  let panelStyle = $state('')
  let moveSearchInput = $state() as HTMLInputElement
  let moveQuery = $state('')
  let pendingMoves = $state.raw(
    new Map<string, { operation: number; project: KataProjectSummary }>(),
  )
  let interactionGeneration = 0
  let moveOperationGeneration = 0
  let trackedUID = $state<string | null>(null)

  const activeMove = $derived(pendingMoves.get(issue.issue.uid) ?? null)
  const activeMoveOperation = $derived(activeMove?.operation ?? null)
  const movingProjectUID = $derived(activeMove?.project.uid ?? null)
  const pendingProject = $derived(activeMove?.project ?? null)
  const eligibleProjects = $derived.by(() => {
    if (!allowMove) return []
    return projects
      .filter(
        (project) => project.uid !== issue.issue.project_uid && project.metadata.role !== 'inbox',
      )
      .toSorted(
        (a, b) =>
          a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }) ||
          a.uid.localeCompare(b.uid),
      )
  })
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

  $effect(() => {
    if (issue.issue.uid === trackedUID) return
    trackedUID = issue.issue.uid
    interactionGeneration += 1
    menuOpen = false
    moveQuery = ''
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

  $effect(() => {
    if (!menuOpen) return
    return autoReposition(() => [panel], position)
  })

  function position(): void {
    if (!trigger || !panel) return
    panelStyle = floatingPopoverStyle({
      trigger: trigger.getBoundingClientRect(),
      viewportWidth: window.innerWidth,
      viewportHeight: window.innerHeight,
      popoverWidth: panel.offsetWidth,
      popoverHeight: panel.offsetHeight,
      align: 'start',
    })
  }

  function projectArea(project: KataProjectSummary): string {
    return typeof project.metadata.area === 'string' ? project.metadata.area.trim() : ''
  }

  function destinationContext(project: KataProjectSummary): string | null {
    return duplicateContexts.get(project.uid) ?? null
  }

  function triggerButton(): HTMLButtonElement | null {
    return trigger instanceof HTMLButtonElement ? trigger : null
  }

  function closeMenu(force = false, restoreFocus = false): void {
    if ((movePending || activeMoveOperation !== null) && !force) return
    interactionGeneration += 1
    menuOpen = false
    moveQuery = ''
    if (restoreFocus) queueMicrotask(() => triggerButton()?.focus())
  }

  export async function open(): Promise<void> {
    if (!allowMove || movePending || activeMoveOperation !== null) return
    trigger = document.activeElement instanceof HTMLElement ? document.activeElement : null
    menuOpen = true
    moveQuery = ''
    await tick()
    position()
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
</script>

<div class="overflow-host" bind:this={menuRoot} role="presentation">
  {#if menuOpen}
    <div
      class="move-picker kit-popover-card"
      bind:this={panel}
      style={panelStyle}
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

<style>
  .overflow-host {
    position: relative;
    display: inline-flex;
  }

  .move-picker {
    position: fixed;
    z-index: 35;
    margin: 0;
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
    /* Reserve the scrollbar's width so counts do not shift when a search
       changes whether the options overflow. */
    scrollbar-gutter: stable;
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
    opacity: var(--opacity-disabled);
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
