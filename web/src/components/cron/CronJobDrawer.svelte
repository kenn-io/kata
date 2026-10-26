<script lang="ts">
  import type { CronJob, CronRun, CronProject } from '../../lib/cron/types'
  import CronRuns from './CronRuns.svelte'
  let {
    job,
    recentRuns,
    project,
    onOpenRun,
  }: {
    job: CronJob
    recentRuns: CronRun[]
    project: CronProject
    onOpenRun: (uid: string) => void
  } = $props()
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
  <h4>Definition</h4>
  <pre>{JSON.stringify(job.definition, null, 2)}</pre>
  <h4>Recent runs</h4>
  <CronRuns
    rows={recentRuns.map((run) => ({ project, run, jobName: job.name }))}
    onselect={(_project, run) => onOpenRun(run.uid)}
  />
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
  button {
    background: transparent;
    border: 0;
    color: var(--accent-blue);
    font: inherit;
    cursor: pointer;
  }
</style>
