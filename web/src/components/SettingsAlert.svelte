<script lang="ts">
  import CircleAlertIcon from '@lucide/svelte/icons/circle-alert'
  import TriangleAlertIcon from '@lucide/svelte/icons/triangle-alert'
  import type { Snippet } from 'svelte'

  // One line in a settings section saying what failed, tinted by how bad it is.
  interface Props {
    tone?: 'danger' | 'warning'
    children: Snippet
  }

  let { tone = 'danger', children }: Props = $props()
</script>

<p class="settings-alert" data-tone={tone} role="alert">
  <span class="icon" aria-hidden="true">
    {#if tone === 'danger'}<CircleAlertIcon size={14} />{:else}<TriangleAlertIcon size={14} />{/if}
  </span>
  <span class="text">{@render children()}</span>
</p>

<style>
  .settings-alert {
    display: flex;
    align-items: flex-start;
    gap: var(--space-4);
    margin: 0;
    padding: var(--space-4) var(--space-5);
    border-radius: var(--radius-md);
    background: color-mix(in srgb, var(--accent-red) 12%, var(--bg-surface));
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    line-height: 1.45;
    overflow-wrap: anywhere;
  }

  .settings-alert[data-tone='warning'] {
    background: color-mix(in srgb, var(--accent-amber) 12%, var(--bg-surface));
  }

  .icon {
    display: inline-flex;
    flex: none;
    align-items: center;
    height: 1lh;
    color: var(--accent-red);
  }

  .settings-alert[data-tone='warning'] .icon {
    color: var(--accent-amber);
  }

  .text {
    min-width: 0;
  }
</style>
