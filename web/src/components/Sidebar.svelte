<script lang="ts">
  import AlarmClockIcon from '@lucide/svelte/icons/alarm-clock'
  import InboxIcon from '@lucide/svelte/icons/inbox'
  import KeyRoundIcon from '@lucide/svelte/icons/key-round'
  import PlusIcon from '@lucide/svelte/icons/plus'
  import StarIcon from '@lucide/svelte/icons/star'
  import UsersIcon from '@lucide/svelte/icons/users'
  import { ScrollBox, showFlash } from '@kenn-io/kit-ui'

  import GroupedSidebarSection from './GroupedSidebarSection.svelte'

  import type {
    KataTaskMutationResponse,
    KataTaskSearchFilters,
    KataTaskViewName,
  } from '../lib/kata/types'
  import type { KataAreaSummary, KataCurrentView } from '../lib/kata/authority'

  interface Props {
    areas: KataAreaSummary[]
    currentView: KataCurrentView
    searchFilters: KataTaskSearchFilters
    projectCreationDisabled: boolean
    draftFenceGeneration?: number | undefined
    credentialAuditAvailable?: boolean | undefined
    credentialAuditActive?: boolean | undefined
    onOpenView: (name: KataTaskViewName) => void | Promise<void>
    onOpenProject: (projectUID: string) => void | Promise<void>
    onCreateProject: (name: string) => Promise<KataTaskMutationResponse>
    onOpenCredentials?: (() => void | Promise<void>) | undefined
  }

  let {
    areas,
    currentView,
    searchFilters,
    projectCreationDisabled,
    draftFenceGeneration = 0,
    credentialAuditAvailable = false,
    credentialAuditActive = false,
    onOpenView,
    onOpenProject,
    onCreateProject,
    onOpenCredentials = () => {},
  }: Props = $props()

  const systemViews: Array<{
    name: KataTaskViewName
    label: string
    icon: typeof InboxIcon
  }> = [
    { name: 'inbox', label: 'Inbox', icon: InboxIcon },
    { name: 'today', label: 'Today', icon: StarIcon },
    { name: 'delegated', label: 'Delegated', icon: UsersIcon },
    { name: 'scheduled', label: 'Scheduled', icon: AlarmClockIcon },
  ]

  let creatingProject = $state(false)
  let createDraft = $state('')
  let createSaving = $state(false)
  let createInput: HTMLInputElement | null = $state(null)
  let collapsedAreas = $state<string[]>([])
  let lastDraftFenceGeneration = $state<number | null>(null)
  $effect(() => {
    const nextGeneration = draftFenceGeneration
    if (lastDraftFenceGeneration === null) {
      lastDraftFenceGeneration = nextGeneration
      return
    }
    if (nextGeneration === lastDraftFenceGeneration) return
    lastDraftFenceGeneration = nextGeneration
    cancelCreatingProject()
  })

  function toggleArea(name: string): void {
    collapsedAreas = collapsedAreas.includes(name)
      ? collapsedAreas.filter((area) => area !== name)
      : [...collapsedAreas, name]
  }

  // Counts come from the list the user is looking at, so a badge never
  // disagrees with the rows beneath it (Inbox holds future start dates back).
  function viewCount(name: KataTaskViewName): number | undefined {
    if (
      (name === 'inbox' || name === 'today') &&
      currentView.name === name &&
      searchFilters.scope.kind === 'all'
    ) {
      return currentView.groups.reduce((sum, group) => sum + group.issues.length, 0)
    }
    return undefined
  }

  // A system view stays highlighted while narrowed to a project; a project row
  // is highlighted only while browsing that project on its own.
  function isProjectActive(uid: string): boolean {
    return (
      currentView.name === 'all' &&
      searchFilters.scope.kind === 'project' &&
      searchFilters.scope.project_uid === uid
    )
  }

  function startCreatingProject(): void {
    if (projectCreationDisabled) return
    creatingProject = true
    createDraft = ''
    queueMicrotask(() => createInput?.focus())
  }

  function cancelCreatingProject(): void {
    creatingProject = false
    createDraft = ''
  }

  async function submitCreateProject(): Promise<void> {
    const name = createDraft.trim()
    if (!name || createSaving || projectCreationDisabled) return
    createSaving = true
    try {
      await onCreateProject(name)
      creatingProject = false
      createDraft = ''
    } catch (err) {
      showFlash(err instanceof Error ? err.message : 'Could not create project.', {
        tone: 'danger',
      })
    } finally {
      createSaving = false
    }
  }
</script>

<div class="kata-sidebar" aria-label="Kata navigation">
  <ScrollBox label="Kata navigation">
    <nav class="kata-nav" aria-label="System views">
      {#each systemViews as view (view.name)}
        {@const Icon = view.icon}
        {@const count = viewCount(view.name)}
        <button
          type="button"
          class:active={!credentialAuditActive && currentView.name === view.name}
          aria-label={count !== undefined ? `${view.label} ${count}` : view.label}
          onclick={() => {
            void onOpenView(view.name)
          }}
        >
          <span class="nav-icon"><Icon size={14} strokeWidth={1.75} /></span>
          <span class="nav-label">{view.label}</span>
          {#if count !== undefined}
            <span class="nav-count">{count}</span>
          {/if}
        </button>
      {/each}
      {#if credentialAuditAvailable}
        <button
          type="button"
          class:active={credentialAuditActive}
          aria-label="Credentials"
          onclick={() => void onOpenCredentials()}
        >
          <span class="nav-icon"><KeyRoundIcon size={14} strokeWidth={1.75} /></span>
          <span class="nav-label">Credentials</span>
        </button>
      {/if}
    </nav>

    {#each areas as area (area.name)}
      <GroupedSidebarSection
        label={area.name}
        count={area.projects.length}
        collapsed={collapsedAreas.includes(area.name)}
        onclick={() => toggleArea(area.name)}
      >
        {#each area.projects as project (project.uid)}
          <button
            type="button"
            class="project-select-button"
            class:active={isProjectActive(project.uid)}
            onclick={() => void onOpenProject(project.uid)}
          >
            <span class="project-name">{project.name}</span>
            <span class="project-count count">{project.open_count}</span>
          </button>
        {/each}
      </GroupedSidebarSection>
    {/each}

    <div class="project-create">
      {#if creatingProject}
        <form
          class="project-create-form"
          onsubmit={(event) => {
            event.preventDefault()
            void submitCreateProject()
          }}
        >
          <input
            bind:this={createInput}
            aria-label="New project name"
            placeholder="Project name"
            bind:value={createDraft}
            onkeydown={(event) => {
              if (event.key === 'Enter') {
                event.preventDefault()
                void submitCreateProject()
              } else if (event.key === 'Escape') {
                event.preventDefault()
                cancelCreatingProject()
              }
            }}
            disabled={createSaving || projectCreationDisabled}
          />
        </form>
      {:else}
        <button
          type="button"
          class="project-create-button"
          disabled={projectCreationDisabled}
          onclick={startCreatingProject}
        >
          <PlusIcon size={13} strokeWidth={1.9} />
          <span>New project</span>
        </button>
      {/if}
    </div>
  </ScrollBox>
</div>

<style>
  .kata-sidebar {
    --sidebar-list-border: var(--border-default);
    --sidebar-row-bg: transparent;
    --sidebar-row-hover-bg: var(--bg-surface-hover);
    --sidebar-row-padding: 6px 10px;

    display: flex;
    flex: 1 1 auto;
    flex-direction: column;
    min-width: 0;
    min-height: 0;
    border-right: 1px solid var(--border-default);
    background: var(--bg-inset);
  }

  .kata-nav {
    display: grid;
    gap: 4px;
    padding: 12px;
  }

  .kata-nav button,
  .project-select-button,
  .project-create-button {
    width: 100%;
    min-height: 30px;
    border: 0;
    border-radius: 6px;
    background: var(--sidebar-row-bg);
    color: var(--text-secondary);
    display: grid;
    grid-template-columns: 18px minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--space-3);
    padding: var(--sidebar-row-padding);
    text-align: left;
    font: inherit;
    font-size: var(--font-size-sm);
    cursor: pointer;
  }

  .kata-nav button:hover,
  .project-select-button:hover,
  .project-create-button:hover {
    background: var(--sidebar-row-hover-bg);
    color: var(--text-primary);
  }

  .kata-nav button.active,
  .project-select-button.active {
    background: var(--bg-row-selected);
    color: var(--text-primary);
  }

  .nav-icon {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    color: var(--text-muted);
  }

  .kata-nav button.active .nav-icon {
    color: var(--accent-blue);
  }

  .project-select-button.active .project-count,
  .kata-nav button.active .nav-count {
    color: var(--text-primary);
  }

  .kata-nav button.active .nav-label,
  .project-select-button.active .project-name {
    font-weight: 650;
  }

  .nav-label,
  .project-name {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .nav-count,
  .project-count {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
  }

  .project-create {
    padding: 12px;
  }

  .project-select-button {
    grid-template-columns: minmax(0, 1fr) auto;
  }

  .project-create-form input {
    width: 100%;
    min-height: 30px;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
    padding: 5px 8px;
  }

  .project-create-form input:focus {
    outline: none;
    border-color: var(--accent-blue);
    box-shadow: 0 0 0 2px color-mix(in srgb, var(--accent-blue) 18%, transparent);
  }
</style>
