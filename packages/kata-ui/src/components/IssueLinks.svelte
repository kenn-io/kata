<script lang="ts">
  import type { KataIssueDetailModel } from '../types.js'

  interface Props {
    parent?: KataIssueDetailModel['parent']
    children: KataIssueDetailModel['children']
    links: KataIssueDetailModel['links']
  }

  let { parent, children, links }: Props = $props()
</script>

{#if parent || children.length > 0 || links.length > 0}
  <section class="detail-section" aria-labelledby="kata-links-heading">
    <h3 id="kata-links-heading">Links</h3>
    <ul>
      {#if parent}
        <li>
          <span>parent</span><strong>{parent.reference}</strong>{parent.title}
        </li>
      {/if}
      {#each children as child (child.uid)}
        <li>
          <span>child</span><strong>{child.reference}</strong>{child.title}
        </li>
      {/each}
      {#each links as link (link.id)}
        <li>
          <span>{link.relation}</span><strong>{link.peerReference}</strong>
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

  li > strong {
    font-family: var(--font-mono, ui-monospace, monospace);
    font-size: var(--font-size-sm, 0.875rem);
  }
</style>
