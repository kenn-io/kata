<script lang="ts">
  import { Button, Markdown } from '@kenn-io/kit-ui'

  import IssueChecklist from './components/IssueChecklist.svelte'
  import IssueComments from './components/IssueComments.svelte'
  import IssueLinks from './components/IssueLinks.svelte'
  import IssueProperties from './components/IssueProperties.svelte'
  import type { KataIssueDetailProps, KataIssueHostAction } from './types.js'

  let { detail, actions = [], onOpenIssue }: KataIssueDetailProps = $props()

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
    <IssueProperties issue={detail.issue} />
    {#if actions.length > 0}
      <div class="host-actions">
        {#each actions as action (action.id)}
          <Button
            size="sm"
            disabled={Boolean(action.disabled || action.busy)}
            label={action.busy ? `${action.label}…` : action.label}
            onclick={() => invoke(action)}
          />
        {/each}
      </div>
    {/if}
  </header>

  <section class="body-section" aria-label="Description">
    {#if detail.issue.body}
      <Markdown source={detail.issue.body} />
    {:else}
      <p class="empty">No description.</p>
    {/if}
  </section>

  <IssueChecklist items={detail.issue.checklist} />
  <IssueLinks
    parent={detail.parent}
    children={detail.children}
    links={detail.links}
    {onOpenIssue}
  />

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
    container: kata-issue-detail / inline-size;
    display: grid;
    gap: var(--space-7, 24px);
    min-width: 0;
    color: var(--text-primary, #202124);
  }

  /* Properties follow the title in reading order; the host actions sit
     beside the title on wide panes. */
  .detail-header {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    grid-template-areas:
      'copy actions'
      'properties properties';
    align-items: start;
    gap: var(--space-4, 8px) var(--space-6, 16px);
  }

  .heading-copy {
    grid-area: copy;
    min-width: 0;
  }

  .detail-header > :global(.properties) {
    grid-area: properties;
  }

  .host-actions {
    grid-area: actions;
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
    margin: var(--space-1, 2px) 0 0;
    font-size: var(--font-size-xl, 1.125rem);
    font-weight: var(--font-weight-semibold, 600);
    line-height: 1.3;
    overflow-wrap: anywhere;
  }

  .host-actions,
  .claim-state {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-3, 6px);
  }

  .claim-state > span {
    border: 1px solid var(--border-muted, #e2e4e8);
    border-radius: 999px;
    padding: 1px 8px;
    color: var(--text-secondary, #555b6e);
    font-size: var(--font-size-xs, 0.75rem);
  }

  .body-section {
    min-width: 0;
  }

  .empty {
    margin: 0;
  }

  /* Stack on the pane's width, not the viewport: the detail pane can be
     narrow inside a wide window. */
  @container kata-issue-detail (max-width: 560px) {
    .detail-header {
      grid-template-columns: minmax(0, 1fr);
      grid-template-areas:
        'copy'
        'properties'
        'actions';
    }
  }
</style>
