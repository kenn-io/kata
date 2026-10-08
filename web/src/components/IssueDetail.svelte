<script lang="ts">
  import {
    IssueDetail as SharedIssueDetail,
    projectIssueDetail,
    type KataIssueHostAction,
  } from '@kenn-io/kata-ui'
  import { Button } from '@kenn-io/kit-ui'
  import { formatTimestamp } from '@kenn-io/kit-ui/utils/time'
  import type { ComponentProps } from 'svelte'

  import Comments from './Comments.svelte'
  import type { KataCommentReplyIntent } from '../lib/kata/types'
  import IssueEditor from './IssueEditor.svelte'
  import IssueHistory from './IssueHistory.svelte'
  import RecurrencePanel from './RecurrencePanel.svelte'

  let props: ComponentProps<typeof IssueEditor> = $props()
  let editing = $state(false)
  let replyIntent = $state<KataCommentReplyIntent | undefined>()
  let replySourceUID = $state('')

  const detail = $derived(projectIssueDetail(props.issue))
  const visibleRecurrences = $derived.by(() => {
    const recurrences = props.selectedRecurrences ?? []
    const attachedID = props.issue.issue.recurrence_id
    if (attachedID === undefined) return recurrences
    const attached = recurrences.find((recurrence) => recurrence.id === attachedID)
    return attached ? [attached] : []
  })
  const actions = $derived.by(() => {
    const actionsFenced = (props.actionsDisabled ?? false) || (props.authorityBlocked ?? false)
    const next: KataIssueHostAction[] = [
      {
        id: 'edit',
        label: 'Edit issue',
        disabled: actionsFenced,
        invoke: () => {
          editing = true
        },
      },
    ]

    if (props.workspaceAction?.onClick) {
      next.push({
        id: 'workspace',
        label: props.workspaceAction.label,
        disabled: actionsFenced || (props.workspaceAction.disabled ?? false),
        busy: props.workspaceAction.busy ?? false,
        invoke: props.workspaceAction.onClick,
      })
    }
    if (props.onOpenGraph) {
      next.push({
        id: 'graph',
        label: 'Open reachable graph',
        disabled: actionsFenced,
        invoke: () => props.onOpenGraph?.(props.issue.issue),
      })
    }

    return next
  })
</script>

{#if props.returnComment && props.onSelectIssue}
  <Button
    size="sm"
    label="Return to original comment"
    onclick={() =>
      props.onSelectIssue?.({
        uid: props.returnComment!.issueUID,
        commentUID: props.returnComment!.commentUID,
      })}
  />
{/if}
<section class="editor-mode" aria-label="Kata issue editor" hidden={!editing}>
  <div class="editor-toolbar">
    <Button size="sm" label="Done editing" onclick={() => (editing = false)} />
  </div>
  <IssueEditor {...props} navigationActive={editing} />
</section>
{#if !editing}
  <div class="shared-detail">
    <SharedIssueDetail
      {detail}
      {actions}
      onOpenIssue={(uid) => void props.onSelectIssue?.({ uid })}
      onOpenComment={(uid, commentUID, returnComment) =>
        void props.onSelectIssue?.({ uid, commentUID, returnComment })}
      onReplyComment={(replyTo, kind) => {
        replySourceUID = props.issue.issue.uid
        replyIntent = { replyTo, kind, force: false }
      }}
      commentActionsDisabled={Boolean(props.actionsDisabled || props.authorityBlocked)}
      selectedCommentUID={props.selectedCommentUID}
    />
    {#if replyIntent && replySourceUID === props.issue.issue.uid}
      <Comments
        issue={props.issue}
        searchReferences={props.searchReferences ?? (async () => [])}
        actionsDisabled={Boolean(props.actionsDisabled || props.authorityBlocked)}
        draftResetGeneration={props.draftResetGeneration}
        draftFenceGeneration={props.draftFenceGeneration}
        onAddComment={props.onAddComment ?? (async () => false)}
        initialReply={replyIntent}
        showList={false}
        commentError={props.commentError}
        onCancelReply={() => {
          replyIntent = undefined
        }}
      />
    {/if}
    {#if props.issue.issue.assignment_expires_on}
      <dl class="assignment-timing" aria-label="Assignment timing">
        <div>
          <dt>Assignment expires</dt>
          <dd>
            <time datetime={props.issue.issue.assignment_expires_on}>
              {formatTimestamp(props.issue.issue.assignment_expires_on)}
            </time>
          </dd>
        </div>
      </dl>
    {/if}
    {#if visibleRecurrences.length > 0}
      <RecurrencePanel recurrences={visibleRecurrences} readOnly />
    {/if}
    <IssueHistory events={props.events ?? []} />
  </div>
{/if}

<style>
  .editor-mode[hidden] {
    display: none;
  }

  .shared-detail {
    display: grid;
    align-content: start;
    gap: var(--space-7);
    flex: 1 1 auto;
    min-width: 0;
    min-height: 0;
    overflow: auto;
    background: var(--bg-surface);
    padding: var(--space-6) var(--space-7) var(--space-8);
  }

  /* Keep prose at a readable measure on wide panes. */
  .shared-detail > :global(*) {
    max-width: 880px;
  }

  .assignment-timing > div {
    display: flex;
    gap: var(--space-4);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .assignment-timing dt {
    font-weight: 600;
  }

  .assignment-timing dd {
    margin: 0;
  }

  .editor-mode {
    display: flex;
    flex: 1 1 auto;
    min-width: 0;
    min-height: 0;
    flex-direction: column;
  }

  .editor-toolbar {
    display: flex;
    justify-content: flex-end;
    border-bottom: 1px solid var(--border-muted);
    padding: var(--space-3) var(--space-7);
    background: var(--bg-surface);
  }
</style>
