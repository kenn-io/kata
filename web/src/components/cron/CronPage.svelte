<script lang="ts">
  import { DetailDrawer, SegmentedControl, Button } from '@kenn-io/kit-ui'
  import type { CronState } from '../../lib/cron/controller'
  import { mergedRuns } from '../../lib/cron/controller'
  import type {
    CronRouteState,
    CronSelection,
    CronRun,
    CronJob,
    CronWorkflow,
  } from '../../lib/cron/types'
  import { jobTags } from '../../lib/cron/format'
  import CronFilters from './CronFilters.svelte'
  import CronRuns from './CronRuns.svelte'
  import CronJobs from './CronJobs.svelte'
  import CronWorkflows from './CronWorkflows.svelte'
  import CronRunDrawer from './CronRunDrawer.svelte'
  import CronJobDrawer from './CronJobDrawer.svelte'
  import CronWorkflowDrawer from './CronWorkflowDrawer.svelte'
  let {
    route,
    state,
    onNavigate,
    onLoadMore,
    onRefresh,
    onOpenIssue,
  }: {
    route: CronRouteState
    state: Readonly<CronState>
    onNavigate: (route: CronRouteState) => void
    onLoadMore: (projectUID: string) => void
    onRefresh: () => void
    onOpenIssue: (projectUID: string, uid: string) => void
  } = $props()
  const projects = $derived(
    state.projects.filter(
      (p) => !p.error && (!route.projectUIDs.length || route.projectUIDs.includes(p.project.uid)),
    ),
  )
  const groups = $derived(
    route.groupByProject
      ? projects.map((p) => ({ key: p.project.uid, name: p.project.name, projects: [p] }))
      : [{ key: 'all', name: '', projects }],
  )
  const jobNames = $derived(
    new Map(
      state.projects.flatMap((p) =>
        p.jobs.map((job) => [`${p.project.uid}:${job.uid}`, job.name] as const),
      ),
    ),
  )
  const selectionProject = $derived(
    state.projects.find((p) => p.project.uid === state.selected?.projectUID)?.project,
  )
  function select(selection: CronSelection | undefined) {
    onNavigate({ ...route, selection })
  }
  function change(next: CronRouteState) {
    onNavigate({
      ...next,
      statuses:
        next.tab === 'jobs'
          ? next.statuses
          : next.statuses.filter((status) => status !== 'never_ran'),
      selection: undefined,
    })
  }
</script>

<section class="cron-page" aria-label="Cron">
  <header>
    <h2>Cron</h2>
    <Button onclick={onRefresh} disabled={state.loading} label="Refresh" />
  </header>
  <SegmentedControl
    ariaLabel="Cron views"
    options={[
      { value: 'jobs', label: 'Jobs' },
      { value: 'runs', label: 'Runs' },
      { value: 'workflows', label: 'Workflows' },
    ]}
    value={route.tab}
    onchange={(value) => change({ ...route, tab: value as CronRouteState['tab'] })}
  />
  <CronFilters {route} projects={state.projects} onchange={change} />
  {#if state.loading}<p role="status">Loading cron…</p>{/if}
  {#each state.projects.filter((p) => p.error) as project (project.project.uid)}<p role="status">
      {project.project.name}: {project.error}. Other readable projects remain available.
    </p>{/each}
  {#if !state.loading && !projects.some((p) => p.hasRecords)}<p>
      No cron records in these projects.
    </p>{/if}
  {#each groups as group (group.key)}
    <section aria-label={group.name || route.tab}>
      {#if group.name}<h3>{group.name}</h3>{/if}
      {#if route.tab === 'runs'}<CronRuns
          rows={mergedRuns(group.projects).map((row) => ({
            ...row,
            jobName: jobNames.get(`${row.project.uid}:${row.run.job_uid}`),
          }))}
          onselect={(project, run) =>
            select({ kind: 'run', projectUID: project.uid, uid: run.uid })}
        />
      {:else if route.tab === 'jobs'}<CronJobs
          rows={group.projects.flatMap((p) =>
            p.jobs
              .filter(
                (job) =>
                  !route.tags.length ||
                  route.tags.every((tag) => jobTags(job.definition.options).includes(tag)),
              )
              .map((job) => ({ project: p.project, job })),
          )}
          onselect={(project, job) =>
            select({ kind: 'job', projectUID: project.uid, uid: job.uid })}
        />
      {:else}<CronWorkflows
          rows={group.projects.flatMap((p) =>
            p.workflows.map((workflow) => ({ project: p.project, workflow })),
          )}
          onselect={(project, workflow) =>
            select({ kind: 'workflow', projectUID: project.uid, uid: workflow.uid })}
        />{/if}
    </section>
  {/each}
  {#if route.tab === 'runs'}
    {#each projects.filter((p) => p.historyError) as project (project.project.uid)}<p role="status">
        {project.project.name}: {project.historyError}
      </p>{/each}
    {#if projects.some((p) => p.nextBeforeUID)}<p>Loaded history · more runs available</p>{/if}
    {#each projects.filter((p) => p.nextBeforeUID) as project (project.project.uid)}<Button
        disabled={state.loading || (project.loadingMore ?? false)}
        onclick={() => onLoadMore(project.project.uid)}
        label={`Load more · ${project.project.name}`}
      />{/each}
  {/if}
  {#if state.selected}
    <DetailDrawer
      title={state.selected.kind === 'run'
        ? 'Run detail'
        : state.selected.kind === 'job'
          ? 'Job detail'
          : 'Workflow detail'}
      onclose={() => select(undefined)}
    >
      {#if state.detailLoading}<p role="status">Loading detail…</p>{:else if state.detailError}<p
          role="status"
        >
          {state.detailError}
        </p>{:else if state.detail && selectionProject}
        {#if state.selected.kind === 'run'}<CronRunDrawer
            run={state.detail as CronRun}
            peek={state.peek}
            onOpenIssue={(uid) => onOpenIssue(selectionProject!.uid, uid)}
            onOpenJob={(uid) => select({ kind: 'job', projectUID: selectionProject!.uid, uid })}
          />
        {:else if state.selected.kind === 'job'}<CronJobDrawer
            job={state.detail as CronJob}
            recentRuns={state.recentRuns}
            project={selectionProject}
            onOpenRun={(uid) => select({ kind: 'run', projectUID: selectionProject!.uid, uid })}
          />
        {:else}<CronWorkflowDrawer workflow={state.detail as CronWorkflow} />{/if}
      {/if}
    </DetailDrawer>
  {/if}
</section>

<style>
  .cron-page {
    flex: 1;
    padding: var(--space-5);
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: 0;
    overflow: auto;
    height: 100%;
  }
  header {
    display: flex;
    justify-content: space-between;
    align-items: center;
  }
  h2,
  h3 {
    margin: 0;
  }
  p {
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
  }
  @media (max-width: 640px) {
    .cron-page {
      padding: var(--space-3);
    }
  }
</style>
