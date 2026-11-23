<script lang="ts">
  /* eslint-disable svelte/prefer-svelte-reactivity */
  import { Button, SearchInput } from '@kenn-io/kit-ui'
  import Modal from './Modal.svelte'
  import DialogPreview from './shared/DialogPreview.svelte'
  import { tick } from 'svelte'
  import type { KataProjectSummary, KataTaskDetail } from '../lib/kata/types'
  import { markColor } from '../lib/marks'

  interface Props {
    issue: KataTaskDetail
    projects: KataProjectSummary[]
    movePending?: boolean | undefined
    allowMove?: boolean | undefined
    error?: string | undefined
    onMoveIssue: (toProjectUID: string) => boolean | Promise<boolean>
  }

  let {
    issue,
    projects,
    movePending = false,
    allowMove = true,
    error = undefined,
    onMoveIssue,
  }: Props = $props()

  let menuOpen = $state(false)
  let trigger: HTMLElement | null = null
  let moveSearchInput = $state() as HTMLInputElement
  let moveQuery = $state('')
  let refused = $state(false)
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
    refused = false
    await tick()
    moveSearchInput?.focus()
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
      else refused = true
    } finally {
      if (pendingMoves.get(sourceIssueUID)?.operation === operation) {
        const nextPendingMoves = new Map(pendingMoves)
        nextPendingMoves.delete(sourceIssueUID)
        pendingMoves = nextPendingMoves
      }
    }
  }
</script>

<Modal open={menuOpen} title="Move task" onClose={() => closeMenu(false, true)} width={560}>
  <div class="move-picker">
    <div>
      <strong>{issue.issue.title}</strong>
      <div class="task-id">{issue.issue.qualified_id}</div>
    </div>
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
          <span class="move-project-name"
            ><span class="project-mark" style:background={markColor(project.name)}
            ></span>{project.name}</span
          >
          {#if destinationContext(project)}
            <span class="move-project-context">{destinationContext(project)}</span>
          {/if}
          <span class="move-project-count">{project.open_count} open</span>
        </button>
      {:else}
        <p role="status">No matching projects</p>
      {/each}
    </div>
    {#if refused && error}<p class="move-error" role="alert">{error}</p>{/if}
    <DialogPreview>
      <div>The task gets a new ID.</div>
      {#if issue.issue.child_counts?.total}
        <div>
          Its {issue.issue.child_counts.total} child {issue.issue.child_counts.total === 1
            ? 'task moves'
            : 'tasks move'} with it.
        </div>
      {/if}
      <div>Links, comments, and history come along.</div>
    </DialogPreview>
  </div>
  {#snippet footer()}
    <Button
      size="sm"
      label="Cancel"
      disabled={movePending || activeMoveOperation !== null}
      onclick={() => closeMenu(false, true)}
    />
  {/snippet}
</Modal>

<style>
  .move-picker {
    display: grid;
    gap: var(--space-4);
  }
  .move-error {
    color: var(--accent-red);
    margin: 0;
  }
  .task-id {
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
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
  .project-mark {
    display: inline-block;
    width: 8px;
    height: 8px;
    margin-right: 8px;
    border-radius: 50%;
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
