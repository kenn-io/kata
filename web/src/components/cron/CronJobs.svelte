<script lang="ts">
  import { StatusDot, Table } from '@kenn-io/kit-ui'
  import { statusDot, jobTags } from '../../lib/cron/format'
  import type { CronProject, CronJob } from '../../lib/cron/types'
  let {
    rows,
    onselect,
  }: {
    rows: { project: CronProject; job: CronJob }[]
    onselect: (project: CronProject, job: CronJob) => void
  } = $props()
</script>

<div class="table-scroll">
  <Table bare unsorted
    ><thead
      ><tr><th>Last run</th><th>Job</th><th>Project</th><th>Schedule</th><th>Tags</th></tr></thead
    ><tbody>
      {#each rows as { project, job } (`${project.uid}:${job.uid}`)}<tr
          ><td
            ><StatusDot
              status={statusDot(job.last_run?.status ?? 'unknown')}
              label={job.last_run?.status ?? 'Never ran'}
            /><span class="status">{job.last_run?.status ?? 'Never ran'}</span></td
          ><td
            ><button type="button" onclick={() => onselect(project, job)}>{job.name}</button
            >{#if !job.definition.enabled}
              <span class="paused">Paused</span>{/if}</td
          ><td><span class="project-chip">{project.name}</span></td><td
            >{job.definition.trigger.cron ??
              job.definition.trigger.at ??
              (job.definition.trigger.interval_seconds
                ? `Every ${job.definition.trigger.interval_seconds}s`
                : job.definition.trigger.kind)}</td
          ><td>{jobTags(job.definition.options).join(', ') || '—'}</td></tr
        >{/each}
    </tbody></Table
  >
</div>

<style>
  .table-scroll {
    overflow-x: auto;
  }
  .table-scroll :global(table) {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--font-size-sm);
  }
  th,
  td {
    text-align: left;
    padding: var(--space-3);
    border-bottom: 1px solid var(--border-muted);
  }
  th {
    color: var(--text-muted);
  }
  button {
    font: inherit;
    border: 0;
    background: transparent;
    color: var(--text-primary);
    cursor: pointer;
    text-decoration: underline;
    text-underline-offset: 3px;
  }
  .project-chip {
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    padding: 2px 6px;
  }
  td {
    white-space: nowrap;
  }
  .status,
  .paused {
    margin-left: var(--space-2);
  }
  .paused {
    color: var(--text-muted);
  }
</style>
