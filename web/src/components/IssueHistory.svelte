<script lang="ts">
  import type { KataTaskEvent } from '../lib/kata/types'
  import { describeKataEvent } from '../lib/history/format'

  interface Props {
    events: readonly KataTaskEvent[]
  }

  let { events }: Props = $props()
</script>

<section class="events" aria-labelledby="kata-events-title">
  <h3 id="kata-events-title">Events</h3>
  {#if events.length === 0}
    <p>No events</p>
  {:else}
    <ul>
      {#each events as event (event.event_uid)}
        {@const descriptor = describeKataEvent(event)}
        {@const EventIcon = descriptor.icon}
        <li class="event-row" data-tone={descriptor.tone}>
          <span class="event-icon" aria-hidden="true">
            <EventIcon size={14} strokeWidth={1.8} />
          </span>
          <span>{descriptor.label}</span>
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .events h3 {
    margin: 0 0 8px;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 650;
    text-transform: uppercase;
  }

  .events p {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .events ul {
    margin: 0;
    padding: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    list-style: none;
  }

  .event-row {
    display: flex;
    align-items: center;
    gap: 8px;
    min-height: 24px;
  }

  .event-icon {
    flex: 0 0 auto;
    display: inline-flex;
    color: var(--text-muted);
  }

  .event-row[data-tone='positive'] .event-icon {
    color: var(--accent-green);
  }

  .event-row[data-tone='negative'] .event-icon {
    color: var(--accent-red);
  }

  .event-row[data-tone='warning'] .event-icon {
    color: var(--accent-amber);
  }
</style>
