<script lang="ts">
  import { Button, Chip } from '@kenn-io/kit-ui'

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

  interface IssueNavigationTarget {
    uid: string
  }

  interface Props {
    issue: KataTaskDetail
    issueCatalog: readonly KataTaskSummary[]
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
    linkFilters,
    onLinkFiltersChange,
    actionsDisabled = false,
    draftResetGeneration = 0,
    draftFenceGeneration = 0,
    onEditIssue,
    onSelectIssue,
  }: Props = $props()

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
  const showStateChips = $derived(linkFilters.statuses.open && linkFilters.statuses.closed)

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

  const relationLabels: Record<KataLinkRelation, string> = {
    parent: 'parent',
    child: 'child',
    blocks: 'blocks',
    blocked_by: 'blocked_by',
    related: 'related',
  }

  function linkLabel(link: KataTaskLink): string {
    return relationLabels[relationForKataLink(link, issue.issue.uid)]
  }

  async function submitRelatedLink(): Promise<void> {
    if (actionsDisabled) return
    const draft = relatedDraft
    const ref = draft.trim()
    if (ref === '') return
    const mutationUID = issue.issue.uid
    const resetGeneration = draftResetGeneration
    const draftRevision = relatedDraftRevision
    const ok = await onEditIssue(mutationUID, { links_delta: { add_related: [ref] } })
    if (!ok || issue.issue.uid !== mutationUID) return
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
  }

  function handleRelatedKeydown(event: KeyboardEvent): void {
    if (event.key === 'Enter') {
      event.preventDefault()
      void submitRelatedLink()
    }
  }
</script>

<section class="task-links" aria-label="Links">
  <div class="section-header link-section-header">
    <h3>Links</h3>
    <div class="link-header-actions">
      <span>
        {visibleLinks.length === issue.links.length
          ? issue.links.length
          : `${visibleLinks.length} / ${issue.links.length}`}
      </span>
      <LinkFilterMenu filters={linkFilters} onChange={onLinkFiltersChange} />
    </div>
  </div>
  {#if issue.links.length === 0}
    <p class="link-empty">No links.</p>
  {:else if visibleLinks.length === 0}
    <p class="link-empty">No links match these filters.</p>
  {:else}
    <div class="link-list">
      {#each visibleLinks as link (link.id)}
        {@const resolution = peerResolution(link)}
        {@const peer = resolution.kind === 'resolved' ? resolution.peer : undefined}
        <button
          type="button"
          class={[
            'link-row',
            (showStateChips || resolution.kind === 'failed') && 'link-row--with-state',
          ]}
          aria-label={`${linkLabel(link)} ${linkPeerLabel(link)} ${peer?.title ?? ''}${showStateChips && peer ? ` ${peer.status}` : ''}${resolution.kind === 'failed' ? ' state unavailable' : ''}`.trim()}
          title={peer ? undefined : 'Task state unavailable; open to load details'}
          onclick={() => {
            void onSelectIssue({ uid: linkPeerUID(link) })
          }}
        >
          <span class="link-kind">{linkLabel(link)}</span>
          <span class="link-peer">{linkPeerLabel(link)}</span>
          {#if peer?.title}<span class="link-title">{peer.title}</span>{/if}
          {#if showStateChips && peer}
            <Chip size="xs" tone={peer.status === 'open' ? 'success' : 'muted'}>{peer.status}</Chip>
          {:else if resolution.kind === 'failed'}
            <Chip size="xs" tone="muted" title="Task state unavailable">unknown</Chip>
          {/if}
        </button>
      {/each}
    </div>
  {/if}
  <form
    class="link-form"
    onsubmit={(event) => {
      event.preventDefault()
      void submitRelatedLink()
    }}
  >
    <label>
      <span>Related issue</span>
      <input
        aria-label="Related issue"
        placeholder="Short id"
        bind:value={() => relatedDraft, updateRelatedDraft}
        onkeydown={handleRelatedKeydown}
        disabled={actionsDisabled}
      />
    </label>
    <Button
      type="submit"
      surface="outline"
      size="sm"
      label="Link"
      disabled={actionsDisabled || relatedDraft.trim() === ''}
    />
  </form>
</section>

<style>
  .task-links {
    display: grid;
    gap: 8px;
    margin: 0 0 18px;
  }

  .section-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    margin-bottom: 8px;
  }

  .section-header h3 {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 650;
    text-transform: uppercase;
  }

  .link-header-actions {
    display: inline-flex;
    align-items: center;
    gap: var(--space-3);
  }

  .link-header-actions > span,
  .link-empty {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .link-empty {
    margin: 0;
  }

  .link-list {
    display: grid;
    gap: var(--space-1);
  }

  .link-row {
    width: 100%;
    min-height: 32px;
    border: 0;
    border-radius: 6px;
    background: transparent;
    color: var(--text-primary);
    display: grid;
    grid-template-columns: max-content max-content minmax(0, 1fr);
    align-items: center;
    gap: 8px;
    padding: 4px 6px;
    font: inherit;
    font-size: var(--font-size-sm);
    text-align: left;
    cursor: pointer;
  }

  .link-row--with-state {
    grid-template-columns: max-content max-content minmax(0, 1fr) max-content;
  }

  .link-row:hover:not(:disabled) {
    background: var(--bg-hover);
  }

  .link-kind {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 650;
  }

  .link-peer {
    color: var(--text-primary);
    font-family: var(--font-mono);
    font-size: var(--font-size-xs);
  }

  .link-title {
    min-width: 0;
    color: var(--text-secondary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .link-form {
    display: flex;
    align-items: flex-end;
    gap: 6px;
  }

  .link-form label {
    min-width: 0;
    flex: 1;
    display: grid;
    gap: var(--space-1);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 650;
  }

  .link-form input {
    width: 100%;
    min-height: 28px;
    border: 1px solid var(--border-default);
    border-radius: 6px;
    background: var(--bg-primary);
    color: var(--text-primary);
    font: inherit;
    font-size: var(--font-size-sm);
    font-weight: 500;
    padding: 4px 8px;
  }
</style>
