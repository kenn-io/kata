<script lang="ts">
  import { Checkbox, SelectDropdown } from '@kenn-io/kit-ui'
  import type {
    KataCreateRecurrenceInput,
    KataPatchRecurrenceInput,
    KataRecurrence,
    KataRecurrenceTemplateUpdateInput,
  } from '../lib/kata/types'
  import DatePicker from './DatePicker.svelte'
  import {
    WEEKDAYS,
    WEEKDAY_LABEL,
    WEEKDAY_LONG,
    MONTH_LONG,
    serializeRRule,
    formatRRule,
    parseRRule,
    type Weekday,
    type Ordinal,
    type CommonRule,
    type ParseResult,
  } from '../lib/recurrence/rrule'

  type Mode =
    | { kind: 'create'; projectID: number }
    | { kind: 'edit'; recurrence: KataRecurrence; etag: string }

  interface Props {
    mode: Mode
    actor: string
    onCreate: (projectID: number, input: KataCreateRecurrenceInput) => Promise<void>
    onPatch: (id: number, input: KataPatchRecurrenceInput, etag: string) => Promise<void>
    onSaved: () => void
  }

  let { mode, actor, onCreate, onPatch, onSaved }: Props = $props()

  // --- Schedule state (Common mode) -------------------------------
  // Compute defaults from the initial dtstart once. Subsequent state
  // initializers consume the plain constant rather than reading the
  // reactive `dtstart` (which would trigger `state_referenced_locally`).
  const INITIAL_DTSTART = initialDtstart()
  let dtstart: string = $state(INITIAL_DTSTART)
  let timezone: string = $state('System default')
  let freq = $state<CommonRule['freq']>('WEEKLY')
  let interval: number = $state(1)
  let byDay = $state<Weekday[]>([weekdayFor(INITIAL_DTSTART)])
  let dayInMonthMode = $state<'dayOfMonth' | 'nthWeekday'>('dayOfMonth')
  let dayInMonthDay: number = $state(1)
  let dayInMonthLastDay: boolean = $state(false)
  let dayInMonthOrdinal = $state<Ordinal>(1)
  let dayInMonthWeekday = $state<Weekday>(weekdayFor(INITIAL_DTSTART))
  let yearMonth: number = $state(monthOf(INITIAL_DTSTART))
  let yearlyDayMode = $state<'none' | 'dayOfMonth' | 'nthWeekday'>('none')
  let endMode = $state<'never' | 'until' | 'count'>('never')
  let endDate: string = $state(INITIAL_DTSTART)
  let endCount: number = $state(10)

  const frequencyOptions = [
    { value: 'DAILY', label: 'Daily' },
    { value: 'WEEKLY', label: 'Weekly' },
    { value: 'MONTHLY', label: 'Monthly' },
    { value: 'YEARLY', label: 'Yearly' },
  ]
  const ordinalOptions = [
    { value: '1', label: '1st' },
    { value: '2', label: '2nd' },
    { value: '3', label: '3rd' },
    { value: '4', label: '4th' },
    { value: '5', label: '5th' },
    { value: '-1', label: 'Last' },
  ]
  const timezoneOptions = [
    { value: 'System default', label: 'System default' },
    { value: 'UTC', label: 'UTC' },
    { value: 'America/Chicago', label: 'America/Chicago' },
    { value: 'America/Los_Angeles', label: 'America/Los_Angeles' },
    { value: 'America/New_York', label: 'America/New_York' },
    { value: 'Europe/London', label: 'Europe/London' },
    { value: 'Europe/Berlin', label: 'Europe/Berlin' },
    { value: 'Asia/Tokyo', label: 'Asia/Tokyo' },
  ]
  const priorityOptions = [
    { value: '', label: '(none)' },
    { value: '0', label: 'P0' },
    { value: '1', label: 'P1' },
    { value: '2', label: 'P2' },
    { value: '3', label: 'P3' },
    { value: '4', label: 'P4' },
  ]
  const weekdayOptions = WEEKDAYS.map((day) => ({
    value: day,
    label: weekdayLongLabel(day),
  }))
  const monthOptions = MONTH_LONG.map((label, index) => ({
    value: String(index + 1),
    label,
  }))

  // --- Template state -------------------------------------------
  let title: string = $state('')
  let body: string = $state('')
  let owner: string = $state('')
  let priority: number | null = $state(null)
  let labelsText: string = $state('') // comma-separated
  let metadataText: string = $state('') // JSON or empty

  // --- Mode toggle (Common ⇄ Advanced) ---------------------------
  // `mode_` deliberately ends with an underscore to avoid shadowing
  // the `mode` prop (create/edit dialog mode). Declared here so the
  // edit-mode prefill block (below) can set it before any $derived
  // reads it.
  let mode_ = $state<'common' | 'advanced'>('common')
  let advancedText: string = $state('')

  // --- Submit state ----------------------------------------------
  let submitError: string | null = $state(null)
  let conflictBanner: string | null = $state(null)

  // --- Baseline tracking (edit mode) ----------------------------
  // The prop `mode` is owned by the dialog and we cannot mutate it.
  // After a 412 conflict we need to remember the server's new
  // Recurrence (for diffing) and etag (for the next PATCH) — track
  // those locally so they survive the reload. Initial values are
  // captured via plain functions so the $state initializers don't
  // read the prop directly (avoids `state_referenced_locally`).
  let baselineRecurrence: KataRecurrence | null = $state(initialBaselineRecurrence())
  let baselineEtag: string | null = $state(initialBaselineEtag())

  // --- Edit-mode prefill ----------------------------------------
  // Runs once at component instantiation. Mutates the $state vars
  // declared above so the form reflects the incoming Recurrence.
  // Must run AFTER all `let X = $state(...)` declarations and BEFORE
  // the first `$derived` that reads them. We read `mode` (a prop)
  // directly here rather than `baselineRecurrence` (a $state) to
  // avoid Svelte's `state_referenced_locally` warning.
  applyEditPrefill()
  function applyEditPrefill(): void {
    const initialMode = mode
    if (initialMode.kind !== 'edit') return
    applyRecurrenceToForm(initialMode.recurrence)
  }

  // applyRecurrenceToForm fully resets every form field from the
  // given Recurrence. Used for the initial prefill AND the 412
  // reload, so Common-mode schedule fields always reflect the
  // server's view (not the user's stale edits).
  function applyRecurrenceToForm(rec: KataRecurrence): void {
    title = rec.template_title
    body = rec.template_body ?? ''
    owner = rec.template_owner ?? ''
    priority = rec.template_priority ?? null
    labelsText = (rec.template_labels ?? []).join(', ')
    metadataText =
      rec.template_metadata && Object.keys(rec.template_metadata).length > 0
        ? JSON.stringify(rec.template_metadata, null, 2)
        : ''
    dtstart = rec.dtstart
    timezone = rec.timezone
    const parsed = parseRRule(rec.rrule)
    if (parsed.kind !== 'common') {
      advancedText = rec.rrule
      mode_ = 'advanced'
      return
    }
    applyCommonRuleToForm(parsed.rrule, rec.rrule)
  }

  // applyCommonRuleToForm canonically resets every schedule field
  // from a CommonRule. Used by both the initial prefill and the
  // Advanced → Common toggle so toggling never leaves stale state
  // for fields the new rule omits (e.g. an unset byDay would
  // otherwise keep the previous selection). Always switches mode_
  // to "common" and updates advancedText so a subsequent toggle
  // back to Advanced sees the canonical serialization.
  function applyCommonRuleToForm(c: CommonRule, rawRrule: string | undefined = undefined): void {
    freq = c.freq
    interval = c.interval
    byDay = c.byDay ?? [weekdayFor(dtstart)]
    dayInMonthMode = c.dayInMonth?.kind ?? 'dayOfMonth'
    dayInMonthLastDay = c.dayInMonth?.kind === 'dayOfMonth' && c.dayInMonth.day === -1
    dayInMonthDay =
      c.dayInMonth?.kind === 'dayOfMonth' && c.dayInMonth.day > 0
        ? c.dayInMonth.day
        : dayOfMonth(dtstart)
    dayInMonthOrdinal = c.dayInMonth?.kind === 'nthWeekday' ? c.dayInMonth.ordinal : 1
    dayInMonthWeekday =
      c.dayInMonth?.kind === 'nthWeekday' ? c.dayInMonth.weekday : weekdayFor(dtstart)
    yearMonth = c.byMonth ?? monthOf(dtstart)
    yearlyDayMode =
      c.freq === 'YEARLY'
        ? c.dayInMonth === undefined
          ? 'none'
          : c.dayInMonth.kind === 'dayOfMonth'
            ? 'dayOfMonth'
            : 'nthWeekday'
        : 'none'
    endMode = c.end?.kind ?? 'never'
    endDate = c.end?.kind === 'until' ? c.end.date : dtstart
    endCount = c.end?.kind === 'count' ? c.end.count : 10
    advancedText = rawRrule ?? serializeRRule(c)
    mode_ = 'common'
  }

  // --- Always-visible label for interval -------------------------
  let intervalLabel = $derived(
    freq === 'DAILY'
      ? 'day(s)'
      : freq === 'WEEKLY'
        ? 'week(s)'
        : freq === 'MONTHLY'
          ? 'month(s)'
          : 'year(s)',
  )

  let currentCommonRule = $derived<CommonRule>(buildCommonRuleFromForm())

  let advancedParse = $derived<ParseResult>(
    mode_ === 'advanced' ? parseRRule(advancedText) : { kind: 'common', rrule: currentCommonRule },
  )

  // activeRrule is the string we'd submit right now. Common mode emits
  // canonical; Advanced mode emits the raw textarea contents. The
  // invalid-guard in canSave/trySave prevents submitting an invalid
  // Advanced rule.
  let activeRrule = $derived(
    mode_ === 'advanced' ? advancedText : serializeRRule(currentCommonRule),
  )

  // The compact summary mirrors whatever rule we'd submit right now —
  // Common form in Common mode, raw textarea in Advanced mode. We hide
  // the summary entirely (empty string) in two cases:
  //   - Advanced-mode rule fails to parse — the rrule-feedback block
  //     already shows the specific parser error.
  //   - WEEKLY with zero weekdays selected — the saved rule would
  //     fall back to dtstart's weekday per RFC 5545, but the form
  //     shows no chips selected; "Weekly" alone is misleading and
  //     Save is already disabled.
  let currentSummary = $derived(
    mode_ === 'advanced' && advancedParse.kind === 'invalid'
      ? ''
      : mode_ === 'common' && freq === 'WEEKLY' && byDay.length === 0
        ? ''
        : formatRRule(activeRrule),
  )

  function buildCommonRuleFromForm(): CommonRule {
    const out: CommonRule = { freq, interval }
    if (freq === 'WEEKLY' && byDay.length > 0) out.byDay = byDay
    if (freq === 'MONTHLY') {
      if (dayInMonthMode === 'dayOfMonth') {
        out.dayInMonth = { kind: 'dayOfMonth', day: dayInMonthLastDay ? -1 : dayInMonthDay }
      } else {
        out.dayInMonth = {
          kind: 'nthWeekday',
          ordinal: dayInMonthOrdinal,
          weekday: dayInMonthWeekday,
        }
      }
    }
    if (freq === 'YEARLY') {
      out.byMonth = yearMonth
      if (yearlyDayMode === 'dayOfMonth') {
        // Yearly day-of-month does NOT consult dayInMonthLastDay —
        // that checkbox belongs to the Monthly controls. The Yearly UI
        // models the "last day" case implicitly via dayInMonthDay; if
        // we ever add an explicit Yearly last-day toggle, plumb it as
        // its own state var, not by reusing the Monthly one.
        out.dayInMonth = { kind: 'dayOfMonth', day: dayInMonthDay }
      } else if (yearlyDayMode === 'nthWeekday') {
        out.dayInMonth = {
          kind: 'nthWeekday',
          ordinal: dayInMonthOrdinal,
          weekday: dayInMonthWeekday,
        }
      }
    }
    if (endMode === 'until') out.end = { kind: 'until', date: endDate }
    else if (endMode === 'count') out.end = { kind: 'count', count: endCount }
    else out.end = { kind: 'never' }
    return out
  }

  // --- Exposed instance API (used by RecurrenceEditorDialog) -----
  export function canSave(): boolean {
    if (title.trim().length === 0) return false
    if (mode_ === 'advanced' && parseRRule(advancedText).kind === 'invalid') return false
    if (mode_ === 'common' && !isCommonFormValid()) return false
    if (mode_ === 'common' && freq === 'WEEKLY' && byDay.length === 0) return false
    if (mode.kind === 'edit') return hasEditChanges()
    return true
  }
  export async function trySave(): Promise<void> {
    submitError = null
    if (mode_ === 'advanced' && parseRRule(advancedText).kind === 'invalid') return
    if (mode_ === 'common' && !isCommonFormValid()) return
    let template
    try {
      template = templatePayload()
    } catch (e) {
      submitError = e instanceof Error ? e.message : String(e)
      return
    }

    if (mode.kind === 'create') {
      try {
        await onCreate(mode.projectID, {
          actor,
          rrule: activeRrule,
          dtstart,
          timezone: resolvedTimezone(),
          template,
        })
        onSaved()
      } catch (e) {
        submitError = extractValidationMessage(e)
      }
      return
    }

    if (baselineRecurrence === null || baselineEtag === null) return
    const rec = baselineRecurrence
    const patch: KataPatchRecurrenceInput = { actor }

    const templateDiff = templateChanges(rec, template)
    if (templateDiff) patch.template = templateDiff

    const newTz = resolvedTimezone()
    if (newTz !== rec.timezone) patch.timezone = newTz
    if (dtstart !== rec.dtstart) patch.dtstart = dtstart

    if (mode_ === 'advanced') {
      if (advancedText !== rec.rrule) patch.rrule = advancedText
    } else {
      const originalParse = parseRRule(rec.rrule)
      const newCommon = currentCommonRule
      if (originalParse.kind === 'common') {
        if (!structurallyEqualCommon(originalParse.rrule, newCommon, dtstart)) {
          patch.rrule = serializeRRule(newCommon)
        }
      } else {
        patch.rrule = serializeRRule(newCommon)
      }
    }

    if (Object.keys(patch).length === 1) return

    try {
      await onPatch(rec.id, patch, baselineEtag)
      onSaved()
    } catch (e) {
      if (isConflict(e)) {
        conflictBanner = 'Server version loaded; your unsaved edits were not applied.'
        const server = (e as { response?: { recurrence?: KataRecurrence; etag?: string } }).response
        if (server?.recurrence) {
          baselineRecurrence = server.recurrence
          baselineEtag = server.etag ?? baselineEtag
          applyRecurrenceToForm(server.recurrence)
        }
      } else {
        submitError = extractValidationMessage(e)
      }
    }
  }

  function templateChanges(
    rec: KataRecurrence,
    candidate: ReturnType<typeof templatePayload>,
  ): KataRecurrenceTemplateUpdateInput | null {
    const diff: KataRecurrenceTemplateUpdateInput = {}
    if (candidate.title !== rec.template_title) diff.title = candidate.title
    if ((candidate.body ?? '') !== (rec.template_body ?? '')) diff.body = candidate.body ?? ''
    if ((candidate.owner ?? '') !== (rec.template_owner ?? '')) {
      if (candidate.owner === undefined) diff.clearOwner = true
      else diff.owner = candidate.owner
    }
    if ((candidate.priority ?? null) !== (rec.template_priority ?? null)) {
      if (candidate.priority === undefined) diff.clearPriority = true
      else diff.priority = candidate.priority
    }
    const candidateLabels = candidate.labels ?? []
    const recLabels = rec.template_labels ?? []
    if (JSON.stringify(candidateLabels) !== JSON.stringify(recLabels)) diff.labels = candidateLabels
    const candidateMeta = JSON.stringify(candidate.metadata ?? {})
    const recMeta = JSON.stringify(rec.template_metadata ?? {})
    if (candidateMeta !== recMeta) diff.metadata = candidate.metadata ?? {}
    return Object.keys(diff).length > 0 ? diff : null
  }

  function isCommonFormValid(): boolean {
    return parseRRule(serializeRRule(currentCommonRule)).kind !== 'invalid'
  }

  // structurallyEqualCommon compares two CommonRules for semantic
  // equality. Both sides are normalized first because the parser omits
  // `end: {kind:"never"}` while the form builder always sets it. It
  // also fills the daemon's implicit DTSTART defaults for WEEKLY
  // BYDAY, MONTHLY day-of-month, and YEARLY BYMONTH so edit-mode saves
  // do not turn omitted RRULE parts into explicit schedule changes.
  function structurallyEqualCommon(a: CommonRule, b: CommonRule, baseDtstart: string): boolean {
    return (
      JSON.stringify(normalizeCommon(a, baseDtstart)) ===
      JSON.stringify(normalizeCommon(b, baseDtstart))
    )
  }

  function normalizeCommon(r: CommonRule, baseDtstart: string): CommonRule {
    const out: CommonRule = { freq: r.freq, interval: r.interval }
    if (r.freq === 'YEARLY') out.byMonth = r.byMonth ?? monthOf(baseDtstart)
    else if (r.byMonth !== undefined) out.byMonth = r.byMonth
    if (r.freq === 'WEEKLY')
      out.byDay = r.byDay && r.byDay.length > 0 ? r.byDay : [weekdayFor(baseDtstart)]
    else if (r.byDay && r.byDay.length > 0) out.byDay = r.byDay
    if (r.freq === 'MONTHLY') {
      out.dayInMonth = r.dayInMonth ?? { kind: 'dayOfMonth', day: dayOfMonth(baseDtstart) }
    } else if (r.dayInMonth !== undefined) {
      out.dayInMonth = r.dayInMonth
    }
    if (r.end && r.end.kind !== 'never') out.end = r.end
    return out
  }

  function isConflict(e: unknown): boolean {
    return !!e && typeof e === 'object' && (e as { status?: number }).status === 412
  }

  function hasEditChanges(): boolean {
    if (baselineRecurrence === null) return false
    const rec = baselineRecurrence
    let template
    try {
      template = templatePayload()
    } catch {
      return false
    }
    if (templateChanges(rec, template)) return true
    if (resolvedTimezone() !== rec.timezone) return true
    if (dtstart !== rec.dtstart) return true
    if (mode_ === 'advanced') return advancedText !== rec.rrule
    const originalParse = parseRRule(rec.rrule)
    if (originalParse.kind !== 'common') return true
    return !structurallyEqualCommon(originalParse.rrule, currentCommonRule, dtstart)
  }

  function extractValidationMessage(e: unknown): string {
    if (e && typeof e === 'object') {
      const anyE = e as { code?: string; message?: string; status?: number }
      if (anyE.code === 'validation' && typeof anyE.message === 'string') {
        return anyE.message
      }
    }
    return e instanceof Error ? e.message : String(e)
  }

  // --- Mode toggle helpers ---------------------------------------
  function toCommonOrStay() {
    const r = parseRRule(advancedText)
    if (r.kind !== 'common') return // advanced/invalid → stay in advanced
    // Delegate to the canonical reset helper so omitted optional
    // fields (e.g. parsing FREQ=WEEKLY with no BYDAY) don't leave
    // stale values from the previous form state.
    applyCommonRuleToForm(r.rrule, advancedText)
  }

  function toAdvanced() {
    advancedText = serializeRRule(currentCommonRule)
    mode_ = 'advanced'
  }

  function toggleWeekday(d: Weekday) {
    // Filter through WEEKDAYS (canonical order) so the result is
    // sorted regardless of toggle sequence. Without this, toggling MO
    // back on after WE/FR would produce [WE,FR,MO] — semantically
    // equal but JSON.stringify-different from [MO,WE,FR], producing a
    // spurious `rrule` field in the edit-mode PATCH diff.
    const next = byDay.includes(d) ? byDay.filter((x) => x !== d) : [...byDay, d]
    byDay = WEEKDAYS.filter((w) => next.includes(w))
  }

  // --- Helpers ---------------------------------------------------
  function resolvedTimezone(): string {
    if (timezone === 'System default') {
      try {
        return Intl.DateTimeFormat().resolvedOptions().timeZone
      } catch {
        return 'UTC'
      }
    }
    return timezone
  }

  function templatePayload() {
    let metadata: Record<string, unknown> | undefined
    if (metadataText.trim() !== '') {
      try {
        metadata = JSON.parse(metadataText)
      } catch {
        throw new Error('Metadata must be valid JSON')
      }
    }
    const labels = labelsText
      .split(',')
      .map((s) => s.trim())
      .filter((s) => s !== '')
    return {
      title: title.trim(),
      body: body === '' ? undefined : body,
      owner: owner.trim() || undefined,
      priority: priority ?? undefined,
      labels: labels.length > 0 ? labels : undefined,
      metadata,
    }
  }

  function initialBaselineRecurrence(): KataRecurrence | null {
    return mode.kind === 'edit' ? mode.recurrence : null
  }
  function initialBaselineEtag(): string | null {
    return mode.kind === 'edit' ? mode.etag : null
  }
  function weekdayLabel(d: string): string {
    return WEEKDAY_LABEL[d as Weekday] ?? d
  }
  function weekdayLongLabel(d: string): string {
    return WEEKDAY_LONG[d as Weekday] ?? d
  }
  function initialDtstart(): string {
    const today = new Date()
    const y = today.getFullYear()
    const m = String(today.getMonth() + 1).padStart(2, '0')
    const d = String(today.getDate()).padStart(2, '0')
    return `${y}-${m}-${d}`
  }
  function weekdayFor(iso: string): Weekday {
    if (!/^\d{4}-\d{2}-\d{2}$/.test(iso)) return 'MO' // fallback for malformed input
    const [y, m, d] = iso.split('-').map(Number)
    // Use a local-time Date so the weekday matches the user's calendar.
    const dt = new Date(y!, (m ?? 1) - 1, d ?? 1)
    return WEEKDAYS[(dt.getDay() + 6) % 7]! // JS: 0=Sun, RRULE: MO first
  }
  function monthOf(iso: string): number {
    return Number(iso.split('-')[1] ?? 1)
  }
  function dayOfMonth(iso: string): number {
    return Number(iso.split('-')[2] ?? 1)
  }
</script>

<div class="editor">
  {#if conflictBanner}
    <div class="conflict" role="alert">{conflictBanner}</div>
  {/if}

  {#if currentSummary}
    <div class="summary" data-testid="recurrence-summary">{currentSummary}</div>
  {/if}

  {#if submitError}
    <div class="submit-error" role="alert">{submitError}</div>
  {/if}

  <fieldset class="schedule">
    <legend>Schedule</legend>

    <div class="mode-toggle" role="group" aria-label="Editor mode">
      <button
        type="button"
        class="toggle"
        aria-pressed={mode_ === 'common'}
        onclick={() => mode_ === 'advanced' && toCommonOrStay()}>Common</button
      >
      <button
        type="button"
        class="toggle"
        aria-pressed={mode_ === 'advanced'}
        onclick={() => mode_ === 'common' && toAdvanced()}>Advanced</button
      >
    </div>

    {#if mode_ === 'common'}
      <label class="row">
        <span>Frequency</span>
        <SelectDropdown
          title="Frequency"
          value={freq}
          options={frequencyOptions}
          onchange={(value) => {
            freq = value as CommonRule['freq']
          }}
        />
      </label>

      <label class="row">
        <span>Every</span>
        <input
          type="number"
          min="1"
          bind:value={interval}
          aria-label={`Every ${interval} ${intervalLabel}`}
        />
        <span class="suffix">{intervalLabel}</span>
      </label>

      {#if freq === 'WEEKLY'}
        <div class="row chips" role="group" aria-label="Weekdays">
          {#each WEEKDAYS as d (d)}
            <button
              type="button"
              class="chip"
              aria-pressed={byDay.includes(d)}
              onclick={() => toggleWeekday(d)}>{weekdayLabel(d)}</button
            >
          {/each}
        </div>
      {/if}

      {#if freq === 'MONTHLY'}
        <div class="row monthly">
          <label>
            <input
              type="radio"
              name="monthlyMode"
              value="dayOfMonth"
              checked={dayInMonthMode === 'dayOfMonth'}
              onchange={() => (dayInMonthMode = 'dayOfMonth')}
            />
            On day
            <input
              type="number"
              min="1"
              max="31"
              bind:value={dayInMonthDay}
              disabled={dayInMonthMode !== 'dayOfMonth' || dayInMonthLastDay}
            />
            of the month
          </label>
          <Checkbox
            class="last-day"
            bind:checked={dayInMonthLastDay}
            disabled={dayInMonthMode !== 'dayOfMonth'}
            label="Last day of month"
          />
          <label>
            <input
              type="radio"
              name="monthlyMode"
              value="nthWeekday"
              checked={dayInMonthMode === 'nthWeekday'}
              onchange={() => (dayInMonthMode = 'nthWeekday')}
            />
            On the
            <SelectDropdown
              title="Monthly ordinal"
              value={String(dayInMonthOrdinal)}
              options={ordinalOptions}
              disabled={dayInMonthMode !== 'nthWeekday'}
              onchange={(value) => {
                dayInMonthOrdinal = Number(value) as Ordinal
              }}
            />
            <SelectDropdown
              title="Monthly weekday"
              value={dayInMonthWeekday}
              options={weekdayOptions}
              disabled={dayInMonthMode !== 'nthWeekday'}
              onchange={(value) => {
                dayInMonthWeekday = value as Weekday
              }}
            />
          </label>
        </div>
      {/if}

      {#if freq === 'YEARLY'}
        <label class="row">
          <span>Month</span>
          <SelectDropdown
            title="Month"
            value={String(yearMonth)}
            options={monthOptions}
            onchange={(value) => {
              yearMonth = Number(value)
            }}
          />
        </label>
        <div class="row yearly-day">
          <label>
            <input
              type="radio"
              name="yearlyDayMode"
              value="none"
              checked={yearlyDayMode === 'none'}
              onchange={() => (yearlyDayMode = 'none')}
            />
            Anniversary of start date
          </label>
          <label>
            <input
              type="radio"
              name="yearlyDayMode"
              value="dayOfMonth"
              checked={yearlyDayMode === 'dayOfMonth'}
              onchange={() => (yearlyDayMode = 'dayOfMonth')}
            />
            On day
            <input
              type="number"
              min="1"
              max="31"
              bind:value={dayInMonthDay}
              disabled={yearlyDayMode !== 'dayOfMonth'}
            />
          </label>
          <label>
            <input
              type="radio"
              name="yearlyDayMode"
              value="nthWeekday"
              checked={yearlyDayMode === 'nthWeekday'}
              onchange={() => (yearlyDayMode = 'nthWeekday')}
            />
            On the
            <SelectDropdown
              title="Yearly ordinal"
              value={String(dayInMonthOrdinal)}
              options={ordinalOptions}
              disabled={yearlyDayMode !== 'nthWeekday'}
              onchange={(value) => {
                dayInMonthOrdinal = Number(value) as Ordinal
              }}
            />
            <SelectDropdown
              title="Yearly weekday"
              value={dayInMonthWeekday}
              options={weekdayOptions}
              disabled={yearlyDayMode !== 'nthWeekday'}
              onchange={(value) => {
                dayInMonthWeekday = value as Weekday
              }}
            />
          </label>
        </div>
      {/if}

      <div class="row end" role="group" aria-label="End condition">
        <label>
          <input
            type="radio"
            name="end"
            value="never"
            checked={endMode === 'never'}
            onchange={() => (endMode = 'never')}
          />
          Never
        </label>
        <label>
          <input
            type="radio"
            name="end"
            value="until"
            checked={endMode === 'until'}
            onchange={() => (endMode = 'until')}
          />
          On date
          <DatePicker
            ariaLabel="End date"
            value={endDate}
            disabled={endMode !== 'until'}
            onchange={(value) => {
              endDate = value
            }}
          />
        </label>
        <label>
          <input
            type="radio"
            name="end"
            value="count"
            checked={endMode === 'count'}
            onchange={() => (endMode = 'count')}
          />
          After
          <input type="number" min="1" bind:value={endCount} disabled={endMode !== 'count'} />
          occurrences
        </label>
      </div>
    {/if}

    {#if mode_ === 'advanced'}
      <label class="row advanced">
        <span>RRULE</span>
        <textarea bind:value={advancedText} rows="3" aria-invalid={advancedParse.kind === 'invalid'}
        ></textarea>
      </label>
      <div class="row feedback" data-testid="rrule-feedback">
        {#if advancedParse.kind === 'invalid'}
          <span class="error">Invalid: {advancedParse.error}</span>
        {:else if advancedParse.kind === 'advanced'}
          <span class="hint"
            >Advanced{advancedParse.reason ? ` — ${advancedParse.reason}` : ''}</span
          >
        {:else}
          <span class="ok">Common: {formatRRule(advancedText)}</span>
          <button type="button" class="link" onclick={toCommonOrStay}>Use Common mode</button>
        {/if}
      </div>
    {/if}

    <label class="row">
      <span>Start date</span>
      <DatePicker
        ariaLabel="Start date"
        value={dtstart}
        onchange={(value) => {
          dtstart = value
        }}
      />
    </label>

    <label class="row">
      <span>Timezone</span>
      <SelectDropdown
        title="Timezone"
        value={timezone}
        options={timezoneOptions}
        onchange={(value) => {
          timezone = value
        }}
      />
    </label>
  </fieldset>

  <fieldset class="template">
    <legend>Template</legend>
    <label class="row">
      <span>Title</span>
      <input type="text" bind:value={title} required />
    </label>
    <label class="row">
      <span>Body</span>
      <textarea bind:value={body} rows="3"></textarea>
    </label>
    <label class="row">
      <span>Owner</span>
      <input type="text" bind:value={owner} placeholder="username or empty" />
    </label>
    <label class="row">
      <span>Priority</span>
      <SelectDropdown
        title="Priority"
        value={priority === null ? '' : String(priority)}
        options={priorityOptions}
        onchange={(value) => {
          priority = value === '' ? null : Number(value)
        }}
      />
    </label>
    <label class="row">
      <span>Labels</span>
      <input type="text" bind:value={labelsText} placeholder="comma-separated" />
    </label>
    <label class="row">
      <span>Metadata</span>
      <textarea bind:value={metadataText} rows="3" placeholder="JSON object (optional)"></textarea>
    </label>
  </fieldset>
</div>

<style>
  .editor {
    display: grid;
    gap: 16px;
  }
  .schedule {
    display: grid;
    gap: var(--space-4);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    padding: 12px;
  }
  legend {
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
    padding: 0 6px;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .row > span:first-child {
    min-width: 90px;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
  .chips {
    gap: 4px;
  }
  .chip {
    padding: 4px 8px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    background: var(--bg-surface);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }
  .chip[aria-pressed='true'] {
    background: var(--accent-primary);
    color: white;
    border-color: transparent;
  }
  .monthly,
  .yearly-day,
  .end {
    flex-direction: column;
    align-items: flex-start;
    gap: 6px;
  }
  .summary {
    padding: 8px 10px;
    border-radius: var(--radius-sm);
    background: var(--bg-surface-elevated, var(--bg-surface));
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }
  .submit-error {
    padding: 8px 10px;
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--accent-red) 12%, transparent);
    color: var(--accent-danger, #c4302b);
    font-size: var(--font-size-sm);
  }
  .conflict {
    padding: 8px 10px;
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--accent-red) 12%, transparent);
    color: var(--accent-danger, #c4302b);
    font-size: var(--font-size-sm);
    font-weight: 500;
  }
  .template {
    display: grid;
    gap: var(--space-4);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    padding: 12px;
  }
  .mode-toggle {
    display: inline-flex;
    gap: 0;
    align-self: flex-start;
  }
  .toggle {
    padding: 4px 10px;
    border: 1px solid var(--border-default);
    background: var(--bg-surface);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
    cursor: pointer;
  }
  .toggle:first-child {
    border-top-left-radius: var(--radius-sm);
    border-bottom-left-radius: var(--radius-sm);
  }
  .toggle:last-child {
    border-top-right-radius: var(--radius-sm);
    border-bottom-right-radius: var(--radius-sm);
    border-left: none;
  }
  .toggle[aria-pressed='true'] {
    background: var(--accent-primary);
    color: white;
    border-color: transparent;
  }
  .advanced textarea {
    flex: 1;
    min-width: 0;
    font-family: var(--font-mono, ui-monospace, monospace);
    font-size: var(--font-size-sm);
  }
  .feedback {
    font-size: var(--font-size-sm);
  }
  .feedback .error {
    color: var(--accent-danger, #c33);
  }
  .feedback .hint {
    color: var(--text-secondary);
  }
  .feedback .ok {
    color: var(--text-secondary);
  }
  .feedback .link {
    background: none;
    border: none;
    color: var(--accent-primary);
    cursor: pointer;
    padding: 0;
    font: inherit;
    text-decoration: underline;
  }
</style>
