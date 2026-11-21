<script lang="ts">
  import type { Snippet } from 'svelte'
  import ListChecks from '@lucide/svelte/icons/list-checks'
  import TriangleAlert from '@lucide/svelte/icons/triangle-alert'

  // A full-page state before the workspace can show: the mark, one heading,
  // one line, then its action or form. A failure shows the danger mark.
  interface Props {
    title: string
    tone?: 'neutral' | 'danger' | undefined
    description?: string | undefined
    alert?: boolean | undefined
    children?: Snippet | undefined
    detail?: Snippet | undefined
  }

  let {
    title,
    tone = 'neutral',
    description = undefined,
    alert = false,
    children = undefined,
    detail = undefined,
  }: Props = $props()
  const headingID = $props.id()
</script>

<section class="status-page" role={alert ? 'alert' : undefined} aria-labelledby={headingID}>
  <span class="status-mark" data-tone={tone} aria-hidden="true">
    {#if tone === 'danger'}<TriangleAlert size={20} />{:else}<ListChecks size={20} />{/if}
  </span>
  <h2 id={headingID}>{title}</h2>
  {#if description}<p>{description}</p>{/if}
  {#if detail}<div class="status-page-detail">{@render detail()}</div>{/if}
  {#if children}<div class="status-page-actions">{@render children()}</div>{/if}
</section>

<style>
  .status-page {
    display: grid;
    justify-items: center;
    gap: var(--space-5);
    width: min(calc(100vw - 32px), 360px);
    margin: auto;
    text-align: center;
  }

  .status-mark {
    display: inline-grid;
    place-items: center;
    width: 40px;
    height: 40px;
    border-radius: var(--radius-md);
    background: color-mix(in srgb, var(--accent-blue) 14%, var(--bg-surface));
    color: var(--accent-blue);
  }

  .status-mark[data-tone='danger'] {
    background: color-mix(in srgb, var(--accent-red) 12%, var(--bg-surface));
    color: var(--accent-red);
  }

  h2 {
    margin: 0;
    font-size: var(--font-size-xl);
    line-height: 1.25;
  }

  p {
    margin: 0;
    color: var(--text-secondary);
  }

  .status-page-detail,
  .status-page-actions {
    display: grid;
    justify-items: center;
    gap: var(--space-5);
    width: 100%;
  }
</style>
