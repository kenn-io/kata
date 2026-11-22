<script lang="ts">
  import { Disclosure, Typeahead, type TypeaheadOption } from '@kenn-io/kit-ui'
  import DetailSection from './DetailSection.svelte'
  import { markColor } from '../lib/marks'

  import {
    KATA_LINK_RELATIONS,
    kataLinkMatchesFilters,
    relationForKataLink,
    type KataLinkFilters,
    type KataLinkPeerResolution,
    type KataLinkRelation,
  } from '../lib/kata/linkFilters'
  import type {
    KataTaskDetail,
    KataTaskEditPatch,
    KataTaskLink,
    KataTaskLinkPeer,
    KataTaskSummary,
  } from '../lib/kata/types'
  import LinkFilterMenu from './LinkFilterMenu.svelte'
  import type { UIIssueReference } from '../lib/api/generated'

  interface IssueNavigationTarget {
    uid: string
  }

  interface Props {
    issue: KataTaskDetail
    issueCatalog: readonly KataTaskSummary[]
    searchReferences?: ((query: string) => Promise<UIIssueReference[]>) | undefined
    linkFilters: KataLinkFilters
    onLinkFiltersChange: (next: KataLinkFilters) => void
    actionsDisabled?: boolean | undefined
    draftFenceGeneration?: number | undefined
    onEditIssue: (uid: string, patch: KataTaskEditPatch) => boolean | Promise<boolean>
    onSelectIssue: (target: IssueNavigationTarget) => void | Promise<void>
  }

  let {
    issue,
    issueCatalog,
    searchReferences = async () => [],
    linkFilters,
    onLinkFiltersChange,
    actionsDisabled = false,
    draftFenceGeneration = 0,
    onEditIssue,
    onSelectIssue,
  }: Props = $props()

  let adding = $state(false)
  let options = $state<TypeaheadOption[]>([])
  let searchGeneration = 0
  async function search(query: string): Promise<void> {
    const generation = ++searchGeneration
    const results = await searchReferences(query)
    if (generation === searchGeneration)
      options = results
        .filter((task) => task.uid !== issue.issue.uid)
        .map((task) => ({ name: task.qualified_id, label: task.title, meta: task.qualified_id }))
  }
  let lastDraftFenceGeneration = $state<number | null>(null)

  const visibleLinks = $derived(
    issue.links.filter((link) =>
      kataLinkMatchesFilters(link, issue.issue.uid, peerResolution(link), linkFilters),
    ),
  )
  const hasAppliedLinkFilters = $derived(
    !linkFilters.statuses.open ||
      !linkFilters.statuses.closed ||
      KATA_LINK_RELATIONS.some((relation) => !linkFilters.relations[relation]),
  )

  $effect(() => {
    const nextGeneration = draftFenceGeneration
    if (lastDraftFenceGeneration === null) {
      lastDraftFenceGeneration = nextGeneration
      return
    }
    if (nextGeneration === lastDraftFenceGeneration) return
    lastDraftFenceGeneration = nextGeneration
    adding = false
    options = []
    searchGeneration += 1
  })

  function linkPeerUIDFor(link: KataTaskLink, selectedUID: string | undefined): string {
    return link.from.uid === selectedUID ? link.to.uid : link.from.uid
  }

  function linkPeerUID(link: KataTaskLink): string {
    return linkPeerUIDFor(link, issue.issue.uid)
  }

  function linkPeer(link: KataTaskLink): KataTaskLinkPeer {
    return link.from.uid === issue.issue.uid ? link.to : link.from
  }

  // A peer in another project names that project after its title.
  function linkPeerProject(link: KataTaskLink): string {
    const qualified = linkPeer(link).qualified_id
    const separator = qualified.lastIndexOf('#')
    const project = separator === -1 ? '' : qualified.slice(0, separator)
    return project === issue.issue.project_name ? '' : project
  }

  function linkPeerLabel(link: KataTaskLink): string {
    const peer = linkPeer(link)
    return peer.qualified_id.startsWith(`${issue.issue.project_name}#`)
      ? peer.short_id
      : peer.qualified_id
  }

  function peerResolution(link: KataTaskLink): KataLinkPeerResolution {
    const peer = issueCatalog.find((candidate) => candidate.uid === linkPeerUID(link))
    return { kind: 'resolved', peer: peer ?? linkPeer(link) }
  }

  function peerClosed(link: KataTaskLink): boolean {
    return (
      (issueCatalog.find((candidate) => candidate.uid === linkPeerUID(link)) ?? linkPeer(link))
        .status === 'closed'
    )
  }

  const relationLabels: Record<KataLinkRelation, string> = {
    parent: 'Parent',
    child: 'Child',
    blocks: 'Blocks',
    blocked_by: 'Blocked by',
    related: 'Related',
  }

  function linkLabel(link: KataTaskLink): string {
    return relationLabels[relationForKataLink(link, issue.issue.uid)]
  }

  async function submitRelatedLink(value: string): Promise<boolean> {
    if (actionsDisabled) return false
    const ref = value.trim()
    if (ref === '') return false
    const mutationUID = issue.issue.uid
    const ok = await onEditIssue(mutationUID, { links_delta: { add_related: [ref] } })
    if (ok === true) adding = false
    return ok === true && issue.issue.uid === mutationUID
  }
</script>

{#snippet linkRow(link: KataTaskLink)}
  {@const resolution = peerResolution(link)}
  {@const peer = resolution.kind === 'resolved' ? resolution.peer : undefined}
  <button
    type="button"
    class="link-row"
    class:link-row--closed={peer?.status === 'closed'}
    aria-label={`${linkLabel(link)} ${linkPeerLabel(link)} ${peer?.title ?? ''} ${peer?.status ?? ''}`}
    onclick={() => void onSelectIssue({ uid: linkPeerUID(link) })}
  >
    <span class="link-status" class:closed={peer?.status === 'closed'} aria-hidden="true"></span>
    <span class="link-peer">{linkPeer(link).short_id}</span>
    {#if peer?.title}<span class="link-title">{peer.title}</span>{/if}
    {#if linkPeerProject(link)}<span class="link-project"
        ><span class="project-mark" style:background={markColor(linkPeerProject(link))}
        ></span>{linkPeerProject(link)}</span
      >{/if}
  </button>
{/snippet}
<DetailSection
  title="Links"
  count={issue.links.length === 0
    ? undefined
    : visibleLinks.length === issue.links.length
      ? issue.links.length
      : `${visibleLinks.length} of ${issue.links.length}`}
  empty={adding
    ? undefined
    : issue.links.length === 0
      ? 'No links'
      : visibleLinks.length === 0
        ? 'No matching links'
        : undefined}
>
  {#snippet actions()}
    {#if issue.links.length > 1 || hasAppliedLinkFilters}<LinkFilterMenu
        filters={linkFilters}
        onChange={onLinkFiltersChange}
      />{/if}
    <button
      type="button"
      class="section-action"
      aria-expanded={adding}
      disabled={actionsDisabled}
      onclick={() => {
        adding = !adding
      }}>Add</button
    >
  {/snippet}
  {#each ['blocked_by', 'blocks', 'parent', 'child', 'related'] as group (group)}
    {@const grouped = visibleLinks.filter(
      (link) => relationForKataLink(link, issue.issue.uid) === group,
    )}
    {@const open = grouped.filter((link) => !peerClosed(link))}
    {@const closed = grouped.filter(peerClosed)}
    {#if grouped.length}
      <div class="link-group" role="group" aria-label={relationLabels[group as KataLinkRelation]}>
        <p class="link-group-heading">
          {relationLabels[group as KataLinkRelation]}
          <span class="link-group-count">{grouped.length}</span>
        </p>
        {#each open as link (link.id)}{@render linkRow(link)}{/each}
        {#if closed.length}<Disclosure variant="plain" title={`${closed.length} closed`}>
            {#each closed as link (link.id)}{@render linkRow(link)}{/each}
          </Disclosure>{/if}
      </div>
    {/if}
  {/each}
  {#if adding}
    <div class="link-form">
      <span class="link-relation">Related</span>
      <Typeahead
        {options}
        fallbackLabel="Find task"
        placeholder="Search tasks"
        remote
        allowCustom
        onquery={(query) => void search(query)}
        onselect={submitRelatedLink}
      />
    </div>
  {/if}
</DetailSection>

<style>
  .link-group + .link-group {
    margin-top: 8px;
  }
  .link-group-heading {
    display: flex;
    align-items: baseline;
    gap: 4px;
    margin: 0 0 2px;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-semibold);
  }
  .link-group-count {
    font-weight: var(--font-weight-normal);
  }
  .section-action {
    height: 24px;
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-secondary);
    font: inherit;
    font-size: var(--font-size-md);
    padding: 0 8px;
    cursor: pointer;
  }
  .section-action:hover:not(:disabled),
  .section-action[aria-expanded='true'] {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }
  /* A record row: its text starts at the pane inset and its highlight runs
     to the pane's edges. */
  .link-row {
    width: calc(100% + 32px);
    min-height: 32px;
    margin-inline: -16px;
    border: 0;
    background: transparent;
    color: var(--text-primary);
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 16px;
    font: inherit;
    font-size: var(--font-size-sm);
    text-align: left;
    cursor: pointer;
  }
  .link-row:hover {
    background: var(--bg-surface-hover);
  }
  .link-status {
    width: 14px;
    height: 14px;
    flex: none;
    border: 1.5px solid var(--text-muted);
    border-radius: 50%;
  }
  .link-status.closed {
    background: var(--text-muted);
  }
  /* The title is what the row is about; the ID is muted mono. */
  .link-peer {
    color: var(--text-secondary);
    font-family: var(--font-mono);
  }
  .link-title {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .link-project {
    display: inline-flex;
    align-items: center;
    flex: none;
    gap: 4px;
    color: var(--text-secondary);
  }
  .project-mark {
    width: 10px;
    height: 10px;
    border-radius: 3px;
  }
  .link-row--closed .link-title {
    color: var(--text-muted);
  }
  .link-form {
    display: flex;
    gap: 8px;
    align-items: center;
    margin-top: 8px;
  }
  .link-relation {
    display: inline-flex;
    align-items: center;
    height: 28px;
    padding: 0 12px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-semibold);
  }
  .link-form :global(.kit-typeahead) {
    flex: 1;
    min-width: 0;
  }
</style>
