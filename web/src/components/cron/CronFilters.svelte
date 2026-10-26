<script lang="ts">
  import { FilterDropdown, Checkbox, Chip } from '@kenn-io/kit-ui'
  import type { CronRouteState } from '../../lib/cron/types'
  import { runStatuses } from '../../lib/cron/types'
  import type { CronProjectState } from '../../lib/cron/controller'
  import { jobTags } from '../../lib/cron/format'
  let {
    route,
    projects,
    onchange,
  }: {
    route: CronRouteState
    projects: readonly CronProjectState[]
    onchange: (route: CronRouteState) => void
  } = $props()
  const readable = $derived(projects.filter((p) => !p.error && p.hasRecords))
  const tags = $derived(
    [
      ...new Set(projects.flatMap((p) => p.jobs.flatMap((j) => jobTags(j.definition.options)))),
    ].sort(),
  )
  const statuses = $derived(route.tab === 'jobs' ? [...runStatuses, 'never_ran'] : [...runStatuses])
  const toggle = (key: 'projectUIDs' | 'statuses' | 'tags', value: string) =>
    onchange({
      ...route,
      [key]: route[key].includes(value)
        ? route[key].filter((v) => v !== value)
        : [...route[key], value],
    })
</script>

<div class="filters" aria-label="Cron filters">
  <FilterDropdown
    label="Project"
    detail={String(readable.length)}
    badgeCount={route.projectUIDs.length}
    sections={[
      {
        items: readable.map((p) => ({
          id: p.project.uid,
          label: p.project.name,
          active: route.projectUIDs.includes(p.project.uid),
          count: 1,
          onSelect: () => toggle('projectUIDs', p.project.uid),
        })),
      },
    ]}
    onSelectAll={() => onchange({ ...route, projectUIDs: readable.map((p) => p.project.uid) })}
    onDeselectAll={() => onchange({ ...route, projectUIDs: [] })}
  />
  {#if route.tab !== 'workflows'}<FilterDropdown
      label="Status"
      badgeCount={route.statuses.length}
      sections={[
        {
          items: statuses.map((status) => ({
            id: status,
            label: status.replaceAll('_', ' '),
            active: route.statuses.includes(status),
            onSelect: () => toggle('statuses', status),
          })),
        },
      ]}
      onSelectAll={() => onchange({ ...route, statuses })}
      onDeselectAll={() => onchange({ ...route, statuses: [] })}
    />{/if}
  {#if route.tab === 'jobs'}<FilterDropdown
      label="Tags"
      badgeCount={route.tags.length}
      sections={[
        {
          items: tags.map((tag) => ({
            id: tag,
            label: tag,
            active: route.tags.includes(tag),
            onSelect: () => toggle('tags', tag),
          })),
        },
      ]}
      onSelectAll={() => onchange({ ...route, tags })}
      onDeselectAll={() => onchange({ ...route, tags: [] })}
    />{/if}
  <Checkbox
    checked={route.groupByProject}
    onchange={(checked) => onchange({ ...route, groupByProject: checked })}
    label="Group by project"
  />
</div>
<div class="chips">
  {#each ['projectUIDs', 'statuses', 'tags'] as const as key (key)}
    {#each route[key] as value (value)}<Chip
        interactive
        uppercase={false}
        onclick={() => toggle(key, value)}
        ariaLabel={`Clear ${value}`}
        >{key === 'projectUIDs'
          ? (projects.find((p) => p.project.uid === value)?.project.name ?? value)
          : value} ×</Chip
      >{/each}
  {/each}
</div>

<style>
  .filters,
  .chips {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-3);
  }
</style>
