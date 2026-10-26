<script lang="ts">
  import { Button } from '@kenn-io/kit-ui'
  import type { CronJob, CronRun, CronProject } from '../../lib/cron/types'
  import CronRuns from './CronRuns.svelte'
  let {
    job,
    recentRuns,
    project,
    onOpenRun,
    canMutate = false,
    pending = false,
    onEdit = undefined,
    onToggle = undefined,
    onDelete = undefined,
  }: {
    job: CronJob
    recentRuns: CronRun[]
    project: CronProject
    onOpenRun: (uid: string) => void
    canMutate?: boolean
    pending?: boolean
    onEdit?: (() => void) | undefined
    onToggle?: (() => void) | undefined
    onDelete?: (() => void) | undefined
  } = $props()
  let confirming = $state(false)
</script>

<div class="detail">
  <h3>{job.name}</h3>
  <p>{job.definition.enabled ? 'Enabled' : 'Paused'} · {job.definition.kind}</p>
  <p>
    Last run: {job.last_run?.status ?? 'Never ran'}
  </p>
  {#if job.last_run}<button type="button" onclick={() => onOpenRun(job.last_run!.uid)}
      >Open last run</button
    >{/if}
  {#if onEdit || onToggle || onDelete}
    <div class="actions">
      <Button label="Edit job" disabled={!canMutate || pending} onclick={onEdit} />
      <Button
        label={job.definition.enabled ? 'Pause' : 'Unpause'}
        disabled={!canMutate || pending}
        onclick={onToggle}
      />
      <Button
        label="Delete job"
        disabled={!canMutate || pending}
        onclick={() => {
          confirming = true
        }}
      />
    </div>
    {#if confirming}
      <section aria-label="Delete confirmation">
        <p>Delete {job.name}? This stops future scheduling.</p>
        <Button
          label="Confirm delete"
          disabled={!canMutate || pending}
          onclick={() => {
            confirming = false
            onDelete?.()
          }}
        /><Button
          label="Keep job"
          disabled={pending}
          onclick={() => {
            confirming = false
          }}
        />
      </section>
    {/if}
  {/if}
  <h4>Definition</h4>
  <pre>{JSON.stringify(job.definition, null, 2)}</pre>
  <h4>Recent runs</h4>
  <CronRuns
    rows={recentRuns.map((run) => ({ project, run, jobName: job.name }))}
    onselect={(_project, run) => onOpenRun(run.uid)}
  />
</div>

<style>
  .actions {
    display: flex;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .detail {
    padding: var(--space-4);
    overflow-wrap: anywhere;
  }
  pre {
    font-size: var(--font-size-xs);
    white-space: pre-wrap;
  }
  button {
    background: transparent;
    border: 0;
    color: var(--accent-blue);
    font: inherit;
    cursor: pointer;
  }
</style>
