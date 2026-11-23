<script lang="ts">
  import type { Snippet } from 'svelte'
  import { Card } from '@kenn-io/kit-ui'
  let { children }: { children: Snippet } = $props()
</script>

<Card padding="none" class="preview"><div>{@render children()}</div></Card>

<style>
  :global(.kit-card.preview) {
    padding: 12px 16px;
    border: 0;
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }
</style>
