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
</script>

{#snippet reference(relation: string, label: string, uid: string)}
  {#if onOpenIssue}
    <Button
      size="sm"
      surface="soft"
      class="reference-link"
      label={label}
      ariaLabel={`Open ${relation} ${label}`}
      onclick={() => onOpenIssue?.(uid)}
    />
  {:else}
    <strong>{label}</strong>
  {/if}
{/snippet}

{#if parent || children.length > 0 || links.length > 0}
  <section class="detail-section" aria-labelledby="kata-links-heading">
    <h3 id="kata-links-heading">Links</h3>
    <ul>
      {#if parent}
        <li>
          <span>parent</span>{@render reference('parent', parent.reference, parent.uid)}{parent.title}
        </li>
      {/if}
      {#each children as child (child.uid)}
        <li>
          <span>child</span>{@render reference('child', child.reference, child.uid)}{child.title}
        </li>
      {/each}
      {#each links as link (link.id)}
        <li>
          <span>{link.relation}</span>{@render reference(
            link.relation,
            link.peerReference,
            link.peerUID,
          )}
          {#if link.peerStatus}<em>{link.peerStatus}</em>{/if}
        </li>
      {/each}
    </ul>
  </section>
{/if}

<style>
  .detail-section {
    min-width: 0;
    border-top: 1px solid var(--border-muted, #e2e4e8);
    padding-top: 16px;
  }

  h3 {
    margin: 0 0 9px;
    color: var(--text-muted, #656a73);
    font-size: var(--font-size-xs, 0.75rem);
    font-weight: 650;
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }

  ul {
    display: grid;
    gap: 8px;
    margin: 0;
    padding: 0;
    list-style: none;
  }

  li {
    display: flex;
    align-items: baseline;
    gap: 8px;
  }

  li > span,
  li > em {
    color: var(--text-muted, #656a73);
    font-size: var(--font-size-xs, 0.75rem);
    font-style: normal;
  }

  li > strong,
  li > :global(.reference-link) {
    font-family: var(--font-mono, ui-monospace, monospace);
    font-size: var(--font-size-sm, 0.875rem);
  }

  li > :global(.reference-link) {
    color: var(--accent-blue, #2563eb);
    font-weight: 650;
  }
</style>
