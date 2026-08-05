<script lang="ts">
  import CalendarClock from '@lucide/svelte/icons/calendar-clock'
  import CheckCircle2 from '@lucide/svelte/icons/check-circle-2'
  import Trash2 from '@lucide/svelte/icons/trash-2'
  import type { KataRecurrence } from '../lib/kata/types'
  import { formatRRule } from '../lib/recurrence/rrule'

  interface Props {
    recurrences: KataRecurrence[]
    canCreate: boolean
    onCreate: () => void
    onEdit: (rec: KataRecurrence) => void
    onDelete: (rec: KataRecurrence) => void
  }

  let { recurrences, canCreate, onCreate, onEdit, onDelete }: Props = $props()

  function status(rec: KataRecurrence): 'deleted' | 'done' | 'active' {
    if (rec.deleted_at) return 'deleted'
    if (!rec.next_occurrence_key) return 'done'
    return 'active'
  }
</script>

<section class="recurrence-panel" aria-label="Recurrences">
  <header class="head">
    <h3>Recurring</h3>
    <button
      type="button"
      class="add"
      disabled={!canCreate}
      onclick={onCreate}
      aria-label="+ New recurrence">+ New</button
    >
  </header>
  {#if recurrences.length === 0}
    <p class="empty">No recurring tasks</p>
  {:else}
    <div class="recurrence-list">
      {#each recurrences as rec (rec.uid)}
        <div class="recurrence-row">
          <button
            type="button"
            class="row-main"
            aria-label={rec.template_title}
            onclick={() => onEdit(rec)}
          >
            <div class="recurrence-icon" aria-hidden="true">
              {#if status(rec) === 'deleted'}<Trash2 size={15} />
              {:else if status(rec) === 'done'}<CheckCircle2 size={15} />
              {:else}<CalendarClock size={15} />{/if}
            </div>
            <div class="recurrence-main">
              <div class="recurrence-title">{rec.template_title}</div>
              <div class="recurrence-meta">
                <span>{formatRRule(rec.rrule)}</span>
                <span>Start {rec.dtstart}</span>
                <span>{rec.timezone}</span>
              </div>
              <div class="recurrence-meta">
                {#if status(rec) === 'deleted'}<span>Deleted</span>
                {:else if rec.next_occurrence_key}<span>Next {rec.next_occurrence_key}</span>
                {:else}<span>Done</span>{/if}
                {#if rec.last_materialized_uid}<span>Last {rec.last_materialized_uid}</span>{/if}
                {#if rec.template_labels.length > 0}<span>{rec.template_labels.join(' · ')}</span
                  >{/if}
              </div>
            </div>
          </button>
          <button
            type="button"
            class="row-delete"
            aria-label="Delete recurrence"
            onclick={() => onDelete(rec)}><Trash2 size={14} /></button
          >
        </div>
      {/each}
    </div>
  {/if}
</section>

<style>
  .recurrence-panel {
    display: grid;
    gap: var(--space-4);
  }
  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
  }
  h3 {
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
    text-transform: uppercase;
    letter-spacing: 0;
  }
  .add {
    padding: 2px 8px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-primary);
    font-size: var(--font-size-xs);
  }
  .empty {
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  .recurrence-list {
    display: grid;
    gap: 8px;
  }
  .recurrence-row {
    display: grid;
    grid-template-columns: 1fr auto;
    gap: 4px;
    align-items: start;
  }
  .row-main {
    min-width: 0;
    display: grid;
    grid-template-columns: auto minmax(0, 1fr);
    gap: 8px;
    text-align: left;
    border: none;
    background: none;
    color: inherit;
    cursor: pointer;
    padding: 4px;
    border-radius: var(--radius-sm);
  }
  .row-main:hover {
    background: var(--bg-surface-hover);
  }
  .row-delete {
    align-self: start;
    width: 22px;
    height: 22px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    color: var(--text-muted);
    border: none;
    background: none;
    cursor: pointer;
    border-radius: var(--radius-sm);
  }
  .row-delete:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }
  .recurrence-icon {
    width: 22px;
    height: 22px;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    color: var(--accent-blue);
  }
  .recurrence-main {
    min-width: 0;
    display: grid;
    gap: 4px;
  }
  .recurrence-title {
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    font-weight: 600;
  }
  .recurrence-meta {
    min-width: 0;
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
</style>
