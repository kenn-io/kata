<script lang="ts">
  import { StatusDot, Table } from '@kenn-io/kit-ui'
  import { statusDot } from '../../lib/cron/format'
  import type { CronProject, CronRun } from '../../lib/cron/types'
  let {
    rows,
    onselect,
  }: {
    rows: { project: CronProject; run: CronRun; jobName?: string | undefined }[]
    onselect: (project: CronProject, run: CronRun) => void
  } = $props()
</script>

<div class="table-scroll">
  <Table bare unsorted
    ><thead><tr><th>Status</th><th>Job</th><th>Project</th><th>Started</th></tr></thead><tbody>
      {#each rows as { project, run, jobName } (`${project.uid}:${run.uid}`)}<tr
          class={run.status === 'failed' ? 'failed' : ''}
          ><td
            ><StatusDot status={statusDot(run.status)} label={run.status} /><span class={run.status}
              >{run.status}</span
            ></td
          ><td
            ><button type="button" onclick={() => onselect(project, run)}
              >{jobName ?? run.job_uid ?? run.uid}</button
            ></td
          ><td><span class="project-chip">{project.name}</span></td><td
            ><time datetime={run.started_at ?? run.created_at}
              >{run.started_at ?? run.created_at}</time
            ></td
          ></tr
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
    font-weight: var(--font-weight-medium);
  }
  td {
    white-space: nowrap;
  }
  td:first-child span {
    margin-left: var(--space-2);
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
  .failed,
  .failed button {
    color: var(--accent-red);
  }
  .succeeded {
    color: var(--accent-green);
  }
  .unknown,
  .running {
    color: var(--accent-amber);
  }
  .project-chip {
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
    padding: 2px 6px;
    color: var(--text-secondary);
  }
</style>
