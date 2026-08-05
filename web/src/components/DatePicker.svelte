<script lang="ts">
  import CalendarIcon from '@lucide/svelte/icons/calendar'
  import XIcon from '@lucide/svelte/icons/x'
  import { Button, Calendar, IconButton, todayStr } from '@kenn-io/kit-ui'

  interface Props {
    value: string
    onchange: (value: string) => void
    ariaLabel?: string
    placeholder?: string
    disabled?: boolean
    clearable?: boolean
    clearLabel?: string
    onEscape?: () => void
    class?: string
  }

  let {
    value,
    onchange,
    ariaLabel = 'Choose date',
    placeholder = 'Pick date',
    disabled = false,
    clearable = false,
    clearLabel,
    onEscape,
    class: className = '',
  }: Props = $props()

  let open = $state(false)
  let rootEl = $state<HTMLDivElement>()
  let buttonEl = $state<HTMLButtonElement>()
  let calendarMonth = $derived(validISODate(value) ? value : todayStr())

  const popoverID = `date-picker-${Math.random().toString(36).slice(2)}`
  const displayValue = $derived(value ? formatDate(value) : placeholder)
  const selectedDate = $derived(validISODate(value) ? value : '')

  $effect(() => {
    if (!open) return

    function handleMousedown(event: MouseEvent): void {
      const target = event.target as Node
      if (rootEl?.contains(target)) return
      open = false
    }

    function handleKeydown(event: KeyboardEvent): void {
      if (event.key === 'Escape') {
        open = false
        buttonEl?.focus()
      }
    }

    document.addEventListener('mousedown', handleMousedown)
    document.addEventListener('keydown', handleKeydown)
    return () => {
      document.removeEventListener('mousedown', handleMousedown)
      document.removeEventListener('keydown', handleKeydown)
    }
  })

  function validISODate(input: string): boolean {
    const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(input)
    if (!match) return false
    const year = Number(match[1])
    const month = Number(match[2])
    const day = Number(match[3])
    const date = new Date(Date.UTC(year, month - 1, day))
    return (
      date.getUTCFullYear() === year &&
      date.getUTCMonth() === month - 1 &&
      date.getUTCDate() === day
    )
  }

  function formatDate(input: string): string {
    if (!validISODate(input)) return input
    const [year, month, day] = input.split('-').map(Number)
    const date = new Date(year!, month! - 1, day!)
    return date.toLocaleDateString(undefined, {
      month: 'short',
      day: 'numeric',
      year: new Date().getFullYear() === date.getFullYear() ? undefined : 'numeric',
    })
  }

  function pick(date: string): void {
    onchange(date)
    open = false
    buttonEl?.focus()
  }

  function clearDate(): void {
    onchange('')
    open = false
    buttonEl?.focus()
  }

  function handleDatePickerKeydown(event: KeyboardEvent): void {
    if (event.key !== 'Escape') return
    if (!open && !onEscape) return
    event.preventDefault()
    event.stopPropagation()
    open = false
    buttonEl?.focus()
    onEscape?.()
  }
</script>

<div class={['date-picker', className]} bind:this={rootEl}>
  <span
    class="date-picker-trigger-host"
    {@attach (element) => {
      buttonEl = element.querySelector<HTMLButtonElement>('button') ?? undefined
      if (!buttonEl) return
      buttonEl.setAttribute('aria-haspopup', 'dialog')
      buttonEl.setAttribute('aria-controls', popoverID)
      buttonEl.onkeydown = handleDatePickerKeydown
    }}
  >
    <Button
      class="date-picker-trigger"
      size="sm"
      ariaLabel={`${ariaLabel}: ${displayValue}`}
      ariaExpanded={open}
      onclick={() => {
        if (!disabled) open = !open
      }}
      {disabled}
    >
      <CalendarIcon size="13" strokeWidth="1.9" aria-hidden="true" />
      <span class:placeholder={!value}>{displayValue}</span>
    </Button>
  </span>

  {#if clearable && value}
    <span
      class="date-picker-clear-host"
      {@attach (element) => {
        const clearButton = element.querySelector<HTMLButtonElement>('button')
        if (clearButton) clearButton.onkeydown = handleDatePickerKeydown
      }}
    >
      <IconButton
        class="date-picker-clear"
        size="sm"
        tone="danger"
        ariaLabel={clearLabel ?? `Clear ${ariaLabel.toLowerCase()}`}
        onclick={clearDate}
        {disabled}
      >
        <XIcon size="12" strokeWidth="2" aria-hidden="true" />
      </IconButton>
    </span>
  {/if}

  {#if open}
    <div
      id={popoverID}
      class="date-picker-popover kit-popover-card"
      role="dialog"
      aria-label={ariaLabel}
      tabindex="-1"
      onkeydown={handleDatePickerKeydown}
    >
      <Calendar
        bind:month={calendarMonth}
        selected={selectedDate ? { from: selectedDate, to: selectedDate } : null}
        onpick={pick}
      />
    </div>
  {/if}
</div>

<style>
  .date-picker {
    position: relative;
    display: inline-flex;
    align-items: center;
    gap: 2px;
    min-width: 136px;
  }

  .date-picker-trigger-host {
    display: inline-flex;
    flex: 1 1 auto;
    min-width: 0;
  }

  :global(.date-picker-trigger.kit-button) {
    justify-content: flex-start;
    width: 100%;
    height: 26px;
    min-height: 26px;
    padding: 0 8px;
    border-color: var(--border-muted);
    border-radius: var(--radius-sm);
    font-size: var(--font-size-xs);
    font-weight: 600;
  }

  :global(.date-picker-trigger.kit-button:hover:not(:disabled)),
  :global(.date-picker-trigger.kit-button[aria-expanded='true']) {
    border-color: var(--border-default);
    color: var(--text-primary);
  }

  :global(.date-picker-trigger.kit-button > span) {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    text-align: left;
  }

  .placeholder {
    color: var(--text-muted);
  }

  .date-picker-clear-host {
    display: inline-flex;
    flex: 0 0 auto;
  }

  :global(.date-picker-clear.kit-icon-button) {
    width: 22px;
    height: 26px;
    border: 1px solid var(--border-muted);
    background: var(--bg-inset);
  }

  .date-picker-popover {
    position: absolute;
    z-index: 94;
    top: calc(100% + 3px);
    left: 0;
    width: max-content;
    padding: var(--space-3);
  }
</style>
