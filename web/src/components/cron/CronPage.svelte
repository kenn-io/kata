<script lang="ts">
  import { DetailDrawer, SegmentedControl, Button } from '@kenn-io/kit-ui'
  import type { CronProjectState, CronState } from '../../lib/cron/controller'
  import { mergedRuns } from '../../lib/cron/controller'
  import type {
    CronRouteState,
    CronSelection,
    CronRun,
    CronJob,
    CronWorkflow,
  } from '../../lib/cron/types'
  import type { CronEditorHandle } from './CronEditorHost.svelte'
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
    state: cronState,
    onNavigate,
    onLoadMore,
    onRefresh,
    onOpenIssue,
    canMutate = false,
    mutationPending = false,
    editor = undefined,
  }: {
    route: CronRouteState
    state: Readonly<CronState>
    onNavigate: (route: CronRouteState) => void
    onLoadMore: (projectUID: string) => void
    onRefresh: () => void
    onOpenIssue: (projectUID: string, uid: string) => void
    canMutate?: boolean
    mutationPending?: boolean
    editor?: CronEditorHandle | undefined
  } = $props()
  const projects = $derived(
    cronState.projects.filter(
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
      cronState.projects.flatMap((p) =>
        p.jobs.map((job) => [`${p.project.uid}:${job.uid}`, job.name] as const),
      ),
    ),
  )
  const selectionProject = $derived(
    cronState.projects.find((p) => p.project.uid === cronState.selected?.projectUID)?.project,
  )
  // A linked issue may have moved since the run; open it where it lives now.
  const issueProject = $derived(
    cronState.projects.find((p) => p.project.id === cronState.peek.issue?.issue.project_id)
      ?.project ?? selectionProject,
  )
  function tabError(project: CronProjectState) {
    return route.tab === 'runs'
      ? project.historyError
      : route.tab === 'jobs'
        ? project.jobsError
        : project.workflowsError
  }
  const writable = $derived(canMutate && !!editor && !mutationPending && !editor.isPending())
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
    <div class="page-actions">
      <Button onclick={onRefresh} disabled={cronState.loading} label="Refresh" />
      {#if editor && canMutate}<Button
          label="New job"
          disabled={!writable || !projects.length || editor.isOpen()}
          onclick={() => editor?.create('job')}
        />{/if}
      {#if route.tab === 'workflows' && editor && canMutate}<Button
          label="New workflow"
          disabled={!writable || !projects.length || editor.isOpen()}
          onclick={() => editor?.create('workflow')}
        />{/if}
    </div>
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
  <CronFilters {route} projects={cronState.projects} onchange={change} />
  {#if cronState.loading}<p role="status">Loading cron…</p>{/if}
  {#each cronState.projects.filter((p) => p.error) as project (project.project.uid)}<p
      role="status"
    >
      {project.project.name}: {project.error}. Other readable projects remain available.
    </p>{/each}
  {#if !cronState.loading && !projects.some((p) => p.hasRecords)}<p>
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
  {#each projects.filter((p) => tabError(p)) as project (project.project.uid)}<p role="status">
      {project.project.name}: {tabError(project)}
    </p>{/each}
  {#if route.tab === 'runs'}
    {#if projects.some((p) => p.nextBeforeUID)}<p>Loaded history · more runs available</p>{/if}
    {#each projects.filter((p) => p.nextBeforeUID) as project (project.project.uid)}<Button
        disabled={cronState.loading || (project.loadingMore ?? false)}
        onclick={() => onLoadMore(project.project.uid)}
        label={`Load more · ${project.project.name}`}
      />{/each}
  {/if}
  {#if cronState.selected && !editor?.isOpen()}
    <DetailDrawer
      title={cronState.selected.kind === 'run'
        ? 'Run detail'
        : cronState.selected.kind === 'job'
          ? 'Job detail'
          : 'Workflow detail'}
      onclose={() => select(undefined)}
    >
      {#if editor?.error()}<p role="alert">{editor?.error()}</p>{/if}
      {#if cronState.detailLoading}<p role="status">
          Loading detail…
        </p>{:else if cronState.detailError}<p role="status">
          {cronState.detailError}
        </p>{:else if cronState.detail && selectionProject}
        {#if cronState.selected.kind === 'run'}<CronRunDrawer
            run={cronState.detail as CronRun}
            peek={cronState.peek}
            relatedSessionURL={cronState.relatedSessionURL}
            onOpenIssue={(uid) => onOpenIssue(issueProject!.uid, uid)}
            onOpenJob={(uid) => select({ kind: 'job', projectUID: selectionProject!.uid, uid })}
          />
        {:else if cronState.selected.kind === 'job'}<CronJobDrawer
            job={cronState.detail as CronJob}
            canMutate={writable}
            pending={mutationPending || !!editor?.isPending()}
            onEdit={editor ? () => void editor?.openJob('edit') : undefined}
            onToggle={editor ? () => void editor?.openJob('toggle') : undefined}
            onDelete={editor ? () => void editor?.openJob('delete') : undefined}
            recentRuns={cronState.recentRuns}
            project={selectionProject}
            onOpenRun={(uid) => select({ kind: 'run', projectUID: selectionProject!.uid, uid })}
          />
        {:else}<CronWorkflowDrawer
            workflow={cronState.detail as CronWorkflow}
            canMutate={writable}
            pending={mutationPending || !!editor?.isPending()}
            onEdit={editor ? () => void editor?.openWorkflow() : undefined}
          />{/if}
      {/if}
    </DetailDrawer>
  {/if}
</section>

<style>
  .page-actions {
    display: flex;
    gap: var(--space-2);
  }
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
    .page-actions {
      display: flex;
      gap: var(--space-2);
    }
    .cron-page {
      padding: var(--space-3);
    }
  }
</style>
