<script lang="ts">
  import {
    IssueDetail as SharedIssueDetail,
    projectIssueDetail,
    type KataIssueHostAction,
  } from '@kenn-io/kata-ui'
  import type { ComponentProps } from 'svelte'

  import IssueEditor from './IssueEditor.svelte'
  import IssueHistory from './IssueHistory.svelte'
  import RecurrencePanel from './RecurrencePanel.svelte'

  let props: ComponentProps<typeof IssueEditor> = $props()
  let editing = $state(false)

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

<section class="editor-mode" aria-label="Kata issue editor" hidden={!editing}>
  <div class="editor-toolbar">
    <button type="button" onclick={() => (editing = false)}>Done editing</button>
  </div>
  <IssueEditor {...props} />
</section>
{#if !editing}
  <div class="shared-detail">
    <SharedIssueDetail {detail} {actions} />
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
    flex: 1 1 auto;
    min-width: 0;
    min-height: 0;
    overflow: auto;
    background: var(--bg-primary);
    padding: 18px 22px;
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
    border-bottom: 1px solid var(--border-default);
    padding: 8px 22px;
    background: var(--bg-primary);
  }

  .editor-toolbar button {
    border: 1px solid var(--border-default);
    border-radius: 6px;
    padding: 5px 9px;
    background: var(--surface-interactive);
    color: var(--text-primary);
    font: inherit;
    cursor: pointer;
  }
</style>
