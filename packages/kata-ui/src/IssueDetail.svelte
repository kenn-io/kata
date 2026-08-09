<script lang="ts">
  /* eslint-disable svelte/no-at-html-tags -- kit-ui sanitizes rendered markdown. */
  import { renderMarkdownSync } from '@kenn-io/kit-ui/utils/markdown'

  import IssueChecklist from './components/IssueChecklist.svelte'
  import IssueComments from './components/IssueComments.svelte'
  import IssueLinks from './components/IssueLinks.svelte'
  import IssueProperties from './components/IssueProperties.svelte'
  import type { KataIssueDetailProps, KataIssueHostAction } from './types.js'

  let { detail, actions = [] }: KataIssueDetailProps = $props()

  function invoke(action: KataIssueHostAction): void {
    if (action.disabled || action.busy) return
    void action.invoke()
  }
</script>

<section class="kata-issue-detail" aria-label="Kata issue detail">
  <header class="detail-header">
    <div class="heading-copy">
      <span class="reference">{detail.issue.reference}</span>
      <h2>{detail.issue.title}</h2>
    </div>
    {#if actions.length > 0}
      <div class="host-actions">
        {#each actions as action (action.id)}
          <button
            type="button"
            disabled={action.disabled || action.busy}
            aria-busy={action.busy ?? false}
            onclick={() => invoke(action)}
          >
            {action.busy ? `${action.label}…` : action.label}
          </button>
        {/each}
      </div>
    {/if}
  </header>

  <IssueProperties issue={detail.issue} />

  <section class="detail-section body-section" aria-label="Description">
    {#if detail.issue.body}
      <div class="markdown-body">
        {@html renderMarkdownSync(detail.issue.body)}
      </div>
    {:else}
      <p class="empty">No description.</p>
    {/if}
  </section>

  <IssueChecklist items={detail.issue.checklist} />
  <IssueLinks parent={detail.parent} children={detail.children} links={detail.links} />

  {#if detail.claim || detail.pendingClaims.length > 0}
    <section class="claim-state" aria-label="Claim state">
      {#if detail.claim}<span>Claimed by {detail.claim.holder}</span>{/if}
      {#each detail.pendingClaims as claim, index (`${claim.holder}-${index}`)}
        <span>Pending: {claim.holder}</span>
      {/each}
    </section>
  {/if}

  <IssueComments comments={detail.comments} />
</section>

<style>
  .kata-issue-detail {
    display: grid;
    gap: 18px;
    min-width: 0;
    color: var(--text-primary, #202124);
  }

  .detail-header {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 16px;
  }

  .heading-copy {
    min-width: 0;
  }

  .reference,
  .empty {
    color: var(--text-muted, #656a73);
  }

  .reference {
    font-family: var(--font-mono, ui-monospace, monospace);
    font-size: var(--font-size-xs, 0.75rem);
  }

  h2 {
    margin: 4px 0 0;
    font-size: var(--font-size-xl, 1.35rem);
    line-height: 1.25;
  }

  .host-actions,
  .claim-state {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
  }

  button {
    border: 1px solid var(--border-default, #d5d7dc);
    border-radius: 6px;
    padding: 6px 10px;
    background: var(--surface-interactive, #fff);
    color: inherit;
    font: inherit;
    cursor: pointer;
  }

  button:disabled {
    cursor: default;
    opacity: 0.55;
  }

  .claim-state > span {
    border: 1px solid var(--border-muted, #e2e4e8);
    border-radius: 999px;
    padding: 3px 8px;
    font-size: var(--font-size-xs, 0.75rem);
  }

  .detail-section {
    min-width: 0;
    border-top: 1px solid var(--border-muted, #e2e4e8);
    padding-top: 16px;
  }

  .body-section {
    border-top: 0;
    padding-top: 0;
  }

  .markdown-body :global(:first-child),
  .empty {
    margin-top: 0;
  }

  .markdown-body :global(:last-child),
  .empty {
    margin-bottom: 0;
  }

  @media (max-width: 560px) {
    .detail-header {
      display: grid;
    }
  }
</style>
