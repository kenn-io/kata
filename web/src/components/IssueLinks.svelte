<script lang="ts">
  import { Button, Disclosure, Typeahead, type TypeaheadOption } from '@kenn-io/kit-ui'

  import {
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
    draftResetGeneration?: number | undefined
    draftFenceGeneration?: number | undefined
    onEditIssue: (uid: string, patch: KataTaskEditPatch) => boolean | Promise<boolean>
    onSelectIssue: (target: IssueNavigationTarget) => void | Promise<void>
  }

  interface PendingDraftReset {
    uid: string
    generation: number
    value: string
    revision: number
  }

  let {
    issue,
    issueCatalog,
    searchReferences = async () => [],
    linkFilters,
    onLinkFiltersChange,
    actionsDisabled = false,
    draftResetGeneration = 0,
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
  let relatedDraft = $state('')
  let relatedDraftRevision = 0
  let lastDraftResetGeneration = $state<number | null>(null)
  let lastDraftFenceGeneration = $state<number | null>(null)
  let pendingRelatedReset = $state<PendingDraftReset | null>(null)

  const visibleLinks = $derived(
    issue.links.filter((link) =>
      kataLinkMatchesFilters(link, issue.issue.uid, peerResolution(link), linkFilters),
    ),
  )

  $effect(() => {
    const nextGeneration = draftResetGeneration
    if (lastDraftResetGeneration === null) {
      lastDraftResetGeneration = nextGeneration
      return
    }
    if (nextGeneration === lastDraftResetGeneration) return
    lastDraftResetGeneration = nextGeneration
    const uid = issue.issue.uid
    if (pendingRelatedReset?.uid === uid && pendingRelatedReset.generation !== nextGeneration) {
      if (
        relatedDraftRevision === pendingRelatedReset.revision &&
        relatedDraft === pendingRelatedReset.value
      ) {
        relatedDraft = ''
      }
    }
    pendingRelatedReset = null
  })

  $effect(() => {
    const nextGeneration = draftFenceGeneration
    if (lastDraftFenceGeneration === null) {
      lastDraftFenceGeneration = nextGeneration
      return
    }
    if (nextGeneration === lastDraftFenceGeneration) return
    lastDraftFenceGeneration = nextGeneration
    relatedDraft = ''
    pendingRelatedReset = null
    adding = false
    options = []
    searchGeneration += 1
  })

  function updateRelatedDraft(value: string): void {
    relatedDraft = value
    relatedDraftRevision += 1
  }

  function linkPeerUIDFor(link: KataTaskLink, selectedUID: string | undefined): string {
    return link.from.uid === selectedUID ? link.to.uid : link.from.uid
  }

  function linkPeerUID(link: KataTaskLink): string {
    return linkPeerUIDFor(link, issue.issue.uid)
  }

  function linkPeer(link: KataTaskLink): KataTaskLinkPeer {
    return link.from.uid === issue.issue.uid ? link.to : link.from
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

  async function submitRelatedLink(): Promise<boolean> {
    if (actionsDisabled) return false
    const draft = relatedDraft
    const ref = draft.trim()
    if (ref === '') return false
    const mutationUID = issue.issue.uid
    const resetGeneration = draftResetGeneration
    const draftRevision = relatedDraftRevision
    const ok = await onEditIssue(mutationUID, { links_delta: { add_related: [ref] } })
    if (ok === true) adding = false
    if (ok !== true || issue.issue.uid !== mutationUID) return false
    if (draftResetGeneration !== resetGeneration) {
      if (relatedDraftRevision === draftRevision && relatedDraft === draft) relatedDraft = ''
    } else {
      pendingRelatedReset = {
        uid: mutationUID,
        generation: resetGeneration,
        value: draft,
        revision: draftRevision,
      }
    }
    return true
  }
</script>

{#snippet linkRow(link: KataTaskLink)}
  {@const resolution = peerResolution(link)}
  {@const peer = resolution.kind === 'resolved' ? resolution.peer : undefined}
  <button
    type="button"
    class="link-row"
    aria-label={`${linkLabel(link)} ${linkPeerLabel(link)} ${peer?.title ?? ''} ${peer?.status ?? ''}`}
    onclick={() => void onSelectIssue({ uid: linkPeerUID(link) })}
  >
    <span class="link-status" class:closed={peer?.status === 'closed'} aria-hidden="true"></span>
    <span class="link-peer">{linkPeerLabel(link)}</span>
    {#if peer?.title}<span class="link-title">{peer.title}</span>{/if}
  </button>
{/snippet}
<section class="task-links" aria-label="Links">
  <div class="section-header link-section-header">
    <h3>Links <span>{issue.links.length}</span></h3>
    {#if issue.links.length === 0}<span class="link-empty">No links.</span>{/if}
    <div class="link-header-actions">
      {#if issue.links.length > 8}<LinkFilterMenu
          filters={linkFilters}
          onChange={onLinkFiltersChange}
        />{/if}
      <Button
        size="sm"
        label="Add"
        disabled={actionsDisabled}
        onclick={() => {
          adding = !adding
        }}
      />
    </div>
  </div>
  {#if adding}
    <div class="link-form">
      <span>Related</span>
      <Typeahead
        {options}
        fallbackLabel="Find task"
        placeholder="Search tasks"
        remote
        allowCustom
        onquery={(query) => void search(query)}
        onselect={async (value) => {
          updateRelatedDraft(value)
          return await submitRelatedLink()
        }}
      />
    </div>
  {/if}
  {#if issue.links.length === 0}{:else if visibleLinks.length === 0}<p class="link-empty">
      No links match these filters.
    </p>
  {:else}
    {#each ['blocked_by', 'blocks', 'parent', 'child', 'related'] as group (group)}
      {@const grouped = visibleLinks.filter(
        (link) => relationForKataLink(link, issue.issue.uid) === group,
      )}
      {@const open = grouped.filter((link) => !peerClosed(link))}
      {@const closed = grouped.filter(peerClosed)}
      {#if grouped.length}
        <div>
          <strong>{relationLabels[group as KataLinkRelation]} {grouped.length}</strong>
          {#each open as link (link.id)}{@render linkRow(link)}{/each}
          {#if closed.length}<Disclosure variant="plain" title={`${closed.length} closed`}>
              {#each closed as link (link.id)}{@render linkRow(link)}{/each}
            </Disclosure>{/if}
        </div>
      {/if}
    {/each}
  {/if}
</section>

<style>
  .task-links {
    display: grid;
    gap: 8px;
    margin: 0 0 24px;
  }
  .section-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
  }
  .section-header h3 {
    margin: 0;
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-semibold);
  }
  .section-header h3 span,
  .link-empty {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .link-header-actions,
  .link-form {
    display: flex;
    gap: var(--space-3);
    align-items: center;
  }
  .link-empty {
    margin: 0;
    margin-right: auto;
  }
  .link-row {
    width: 100%;
    min-height: 32px;
    border: 0;
    border-radius: 6px;
    background: transparent;
    color: var(--text-primary);
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 4px 6px;
    font: inherit;
    font-size: var(--font-size-sm);
    text-align: left;
    cursor: pointer;
  }
  .link-row:hover {
    background: var(--bg-surface-hover);
  }
  .link-status {
    width: 13px;
    height: 13px;
    flex: none;
    border: 1.5px solid var(--text-muted);
    border-radius: 50%;
  }
  .link-status.closed {
    background: var(--text-muted);
  }
  .link-peer {
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
  }
  .link-title {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
