<script lang="ts">
  import type { CronProject, CronWorkflow } from '../../lib/cron/types'
  let {
    rows,
    onselect,
  }: {
    rows: { project: CronProject; workflow: CronWorkflow }[]
    onselect: (project: CronProject, workflow: CronWorkflow) => void
  } = $props()
</script>

<ul>
  {#each rows as { project, workflow } (`${project.uid}:${workflow.uid}`)}<li>
      <button type="button" onclick={() => onselect(project, workflow)}>{workflow.name}</button
      ><span class="project-chip">{project.name}</span>
      <p>
        {workflow.definition.about ?? 'No description'} · {workflow.definition.steps.length} steps
      </p>
    </li>{/each}
</ul>

<style>
  ul {
    list-style: none;
    padding: 0;
  }
  li {
    padding: var(--space-3);
    border-bottom: 1px solid var(--border-muted);
  }
  button {
    font: inherit;
    border: 0;
    background: transparent;
    color: var(--text-primary);
    cursor: pointer;
    text-decoration: underline;
  }
  .project-chip {
    margin-left: var(--space-3);
    font-size: var(--font-size-xs);
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    padding: 2px 6px;
  }
  p {
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
  }
</style>
