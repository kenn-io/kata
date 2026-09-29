<script lang="ts">
  import { Button } from '@kenn-io/kit-ui'

  import type { KataIssueDetailModel } from '../types.js'

  interface Props {
    parent?: KataIssueDetailModel['parent']
    children: KataIssueDetailModel['children']
    links: KataIssueDetailModel['links']
    onOpenIssue?: ((uid: string) => void) | undefined
  }

  let { parent, children, links, onOpenIssue }: Props = $props()

  const relationLabels: Record<string, string> = {
    parent: 'Parent',
    child: 'Child',
    blocks: 'Blocks',
    blocked_by: 'Blocked by',
    related: 'Related',
  }

  function relationLabel(relation: string): string {
    return relationLabels[relation] ?? relation
  }
</script>

{#snippet row(relation: string, reference: string, uid: string, title: string, status: string)}
  <li>
    <span class="relation">{relationLabel(relation)}</span>
    {#if onOpenIssue}
      <Button
        size="sm"
        class="reference-link"
        label={reference}
        ariaLabel={`Open ${relationLabel(relation).toLowerCase()} ${reference}`}
        onclick={() => onOpenIssue?.(uid)}
      />
    {:else}
      <span class="reference">{reference}</span>
    {/if}
    <span class="peer">
      {#if title}<span class="title">{title}</span>{/if}
      {#if status}<span class="status" class:closed={status === 'closed'}>{status}</span>{/if}
    </span>
  </li>
{/snippet}

{#if parent || children.length > 0 || links.length > 0}
  <section class="detail-section" aria-labelledby="kata-links-heading">
    <h3 id="kata-links-heading">Links</h3>
    <ul>
      {#if parent}
        {@render row('parent', parent.reference, parent.uid, parent.title, parent.status)}
      {/if}
      {#each children as child (child.uid)}
        {@render row('child', child.reference, child.uid, child.title, child.status)}
      {/each}
      {#each links as link (link.id)}
        {@render row(link.relation, link.peerReference, link.peerUID, '', link.peerStatus)}
      {/each}
    </ul>
  </section>
{/if}

<style>
  .detail-section {
    min-width: 0;
  }

  h3 {
    margin: 0 0 var(--space-4, 8px);
    color: var(--text-primary, #202124);
    font-size: var(--font-size-md, 0.8125rem);
    font-weight: var(--font-weight-semibold, 600);
  }

  ul {
    display: grid;
    grid-template-columns: max-content max-content minmax(0, 1fr);
    column-gap: var(--space-5, 12px);
    margin: 0;
    padding: 0;
    list-style: none;
  }

  li {
    display: grid;
    grid-column: 1 / -1;
    grid-template-columns: subgrid;
    align-items: center;
    min-height: 28px;
    border-top: 1px solid var(--border-muted, #e2e4e8);
  }

  li:last-child {
    border-bottom: 1px solid var(--border-muted, #e2e4e8);
  }

  .relation {
    color: var(--text-muted, #656a73);
    font-size: var(--font-size-sm, 0.75rem);
  }

  .reference,
  li > :global(.reference-link.kit-button) {
    font-family: var(--font-mono, ui-monospace, monospace);
    font-size: var(--font-size-xs, 0.6875rem);
  }

  li > :global(.reference-link.kit-button) {
    justify-self: start;
    min-height: 0;
    padding: 0;
    border: 0;
    background: none;
    color: var(--accent-blue, #2563eb);
    font-weight: var(--font-weight-medium, 500);
  }

  li > :global(.reference-link.kit-button:hover:not(:disabled)) {
    background: none;
    color: var(--accent-blue, #2563eb);
    text-decoration: underline;
  }

  .peer {
    display: flex;
    align-items: baseline;
    gap: var(--space-4, 8px);
    min-width: 0;
  }

  .title {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .status {
    flex: 0 0 auto;
    color: var(--text-muted, #656a73);
    font-size: var(--font-size-xs, 0.6875rem);
    text-transform: capitalize;
  }
</style>
