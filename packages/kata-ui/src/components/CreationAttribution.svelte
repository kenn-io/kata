<script lang="ts">
  import type { KataCreationAttributionModel } from '../types.js'

  let { creation }: { creation?: KataCreationAttributionModel | undefined } = $props()
</script>

{#if creation}
  <div class="creation" aria-label="Creation attribution">
    <span>Creation: {creation.verification}</span>
    {#if creation.verification === 'verified' && creation.accountableActor}
      <span>Accountable: {creation.accountableActor}</span>
    {/if}
    {#if creation.sourceActor}
      <span>Source: {creation.sourceActor}{creation.teammate ? ` / ${creation.teammate}` : ''}</span
      >
    {/if}
  </div>
{/if}

<style>
  .creation {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2, 4px) var(--space-5, 12px);
    min-width: 0;
    color: var(--text-secondary, #555b6e);
    font-size: var(--font-size-sm, 0.75rem);
    overflow-wrap: anywhere;
  }
</style>
