<script lang="ts">
  import type { CronWorkflow } from '../../lib/cron/types'
  let { workflow }: { workflow: CronWorkflow } = $props()
</script>

<div class="detail">
  <h3>{workflow.name}</h3>
  <p>{workflow.definition.about ?? 'No description'}</p>
  <h4>Input</h4>
  <pre>{workflow.definition.input ?? '—'}</pre>
  <h4>Options</h4>
  <pre>{JSON.stringify(workflow.definition.options ?? {}, null, 2)}</pre>
  <h4>Steps</h4>
  <ol>
    {#each workflow.definition.steps as step (step.key)}<li>
        <strong>{step.key}</strong> · {step.kind}
        <pre>{step.command ?? step.prompt ?? '—'}</pre>
        <p>Runs after: {step.after?.join(', ') || '—'} · Retries: {step.retries ?? 0}</p>
        <pre>{JSON.stringify(step.options ?? {}, null, 2)}</pre>
      </li>{/each}
  </ol>
</div>

<style>
  .detail {
    padding: var(--space-4);
    overflow-wrap: anywhere;
  }
  pre {
    font-size: var(--font-size-xs);
    white-space: pre-wrap;
  }
  li {
    margin-bottom: var(--space-4);
  }
</style>
