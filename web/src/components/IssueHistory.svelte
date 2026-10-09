<script lang="ts">
  import type { KataTaskEvent } from '../lib/kata/types'
  import { formatTimestamp } from '@kenn-io/kit-ui/utils/time'

  import { describeKataEvent } from '../lib/history/format'

  interface Props {
    events: readonly KataTaskEvent[]
  }

  let { events }: Props = $props()

  const fullTimestamp = new Intl.DateTimeFormat(undefined, {
    dateStyle: 'full',
    timeStyle: 'long',
  })

  function eventTime(value: string): { short: string; full: string } | undefined {
    const parsed = new Date(value)
    if (Number.isNaN(parsed.getTime())) return undefined
    return { short: formatTimestamp(value), full: fullTimestamp.format(parsed) }
  }

  function closingSession(event: KataTaskEvent): { label: string; url?: string } | undefined {
    if (event.type !== 'issue.closed') return undefined
    const value = event.payload?.transcript
    if (!value || typeof value !== 'object' || Array.isArray(value)) return undefined
    const ref = value as Record<string, unknown>
    if (
      (ref.agent !== 'codex' && ref.agent !== 'claude') ||
      typeof ref.session_id !== 'string' ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(ref.session_id)
    ) {
      return undefined
    }

    const session: { label: string; url?: string } = {
      label: `Session: ${ref.agent} ${ref.session_id}`,
    }
    if (typeof ref.url === 'string') {
      try {
        const url = new URL(ref.url)
        if (
          (url.protocol === 'http:' || url.protocol === 'https:') &&
          !url.username &&
          !url.password &&
          !ref.url.includes('?') &&
          !ref.url.includes('#')
        ) {
          session.url = ref.url
        }
      } catch {
        // A valid identifier remains useful without a usable locator.
      }
    }
    return session
  }
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
        {@const time = eventTime(event.created_at)}
        {@const session = closingSession(event)}
        <li class="event-row" data-tone={descriptor.tone}>
          <span class="event-icon" aria-hidden="true">
            <EventIcon size={14} strokeWidth={1.8} />
          </span>
          <span class="event-label">{descriptor.label}</span>
          {#if time}
            <time class="event-time" datetime={event.created_at} title={time.full}
              >{time.short}</time
            >
          {/if}
          {#if session}
            <span class="event-session">
              {#if session.url}
                <a
                  href={session.url}
                  target="_blank"
                  rel="noopener noreferrer"
                  title="Open session in AgentsView">{session.label}</a
                >
              {:else}
                {session.label}
              {/if}
            </span>
          {/if}
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .events h3 {
    margin: 0 0 var(--space-4);
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: var(--font-weight-semibold);
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
    display: grid;
    grid-template-columns: auto minmax(0, 1fr) auto;
    align-items: baseline;
    gap: 8px;
    min-height: 24px;
  }

  .event-label {
    overflow-wrap: anywhere;
  }

  .event-session {
    grid-column: 2 / -1;
    overflow-wrap: anywhere;
    font-size: var(--font-size-xs);
  }

  .event-time {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }

  .event-icon {
    display: inline-flex;
    align-self: start;
    padding-top: 2px;
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
