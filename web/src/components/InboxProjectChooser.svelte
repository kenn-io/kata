<script lang="ts">
  import Modal from './Modal.svelte'

  interface ProjectChoice {
    uid: string
    name: string
  }

  interface Props {
    open: boolean
    projects: ProjectChoice[]
    onClose: () => void
    onSelect: (projectUID: string) => Promise<void>
  }

  let { open, projects, onClose, onSelect }: Props = $props()
  let pending = $state<string | undefined>()

  async function choose(projectUID: string): Promise<void> {
    if (pending) return
    pending = projectUID
    try {
      await onSelect(projectUID)
      onClose()
    } finally {
      pending = undefined
    }
  }
</script>

<Modal {open} title="Choose Inbox project" {onClose} width={420}>
  <p class="chooser-copy">
    New tasks are captured in one Inbox project. Choose it once to continue.
  </p>
  <div class="project-list">
    {#each projects as project (project.uid)}
      <button
        type="button"
        class="project-choice"
        aria-label={`Use ${project.name} as Inbox`}
        disabled={pending !== undefined}
        onclick={() => void choose(project.uid)}
      >
        {project.name}
      </button>
    {/each}
  </div>
  {#snippet footer()}
    <button class="cancel" type="button" onclick={onClose} disabled={pending !== undefined}
      >Cancel</button
    >
  {/snippet}
</Modal>

<style>
  .chooser-copy {
    margin: 0 0 var(--space-4);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .project-list {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    max-height: min(360px, 50vh);
    overflow-y: auto;
  }

  .project-choice,
  .cancel {
    min-height: 32px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-primary);
    padding: 6px 10px;
    text-align: left;
  }

  .project-choice:hover:not(:disabled),
  .cancel:hover:not(:disabled) {
    background: var(--bg-surface-hover);
  }

  .cancel {
    background: var(--bg-inset);
  }
</style>
