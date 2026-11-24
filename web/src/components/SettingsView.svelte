<script lang="ts">
  import {
    SettingsLayout,
    SegmentedControl,
    SettingsSection,
    type SettingsCategory,
  } from '@kenn-io/kit-ui'
  import type { Snippet } from 'svelte'
  import type { SettingsSection as RouteSettingsSection } from '../lib/router'

  export type SettingsSectionID = RouteSettingsSection | 'credentials'

  interface Props {
    active: SettingsSectionID
    theme: 'light' | 'dark' | 'system'
    onThemeChange: (theme: 'light' | 'dark' | 'system') => void
    credentialsAvailable: boolean
    accessAvailable: boolean
    onSelect: (section: SettingsSectionID) => void
    search: Snippet
    credentials: Snippet
    access: Snippet
  }

  let {
    active,
    theme,
    onThemeChange,
    credentialsAvailable,
    accessAvailable,
    onSelect,
    search,
    credentials,
    access,
  }: Props = $props()

  let accessVisited = $state(false)
  $effect(() => {
    if (!accessAvailable) accessVisited = false
    else if (active === 'access') accessVisited = true
  })

  let categories = $derived<SettingsCategory[]>([
    { id: 'appearance', label: 'Appearance', group: 'This browser' },
    { id: 'search', label: 'Search by meaning', group: 'This server' },
    ...(credentialsAvailable
      ? [{ id: 'credentials', label: 'Credentials', group: 'This server' }]
      : []),
    ...(accessAvailable
      ? [{ id: 'access', label: 'Teams and visibility', group: 'This server' }]
      : []),
  ])
</script>

<div class="settings-view">
  <SettingsLayout
    {categories}
    bind:active={() => active, (next) => onSelect(next as SettingsSectionID)}
  >
    {#snippet panel(id)}
      {#if id === 'appearance'}
        <SettingsSection title="Appearance">
          <div class="settings-row">
            <span>Theme</span>
            <SegmentedControl
              ariaLabel="Theme"
              value={theme}
              options={[
                { value: 'light', label: 'Light' },
                { value: 'dark', label: 'Dark' },
                { value: 'system', label: 'System' },
              ]}
              onchange={(value) => onThemeChange(value as 'light' | 'dark' | 'system')}
            />
          </div>
        </SettingsSection>
      {:else if id === 'search'}
        {@render search()}
      {:else if id === 'credentials'}
        {@render credentials()}
      {/if}
      {#if accessVisited && accessAvailable}
        <div hidden={id !== 'access'}>{@render access()}</div>
      {/if}
    {/snippet}
  </SettingsLayout>
</div>

<style>
  .settings-view {
    display: flex;
    flex: 1 1 auto;
    min-width: 0;
    min-height: 0;
    overflow: hidden;
    background: var(--bg-surface);
    /* Every select in a settings section takes this one width. */
    --settings-select-w: var(--layout-settings-select-w);
    --settings-row-h: 28px;
  }

  .settings-view > :global(*) {
    flex: 1 1 auto;
    min-width: 0;
  }

  .settings-view :global(.settings-title-action) {
    position: absolute;
    top: 0;
    right: 0;
    display: flex;
    align-items: center;
    height: var(--layout-header-h);
  }

  .settings-view :global(.settings-title-action .kit-button) {
    gap: var(--space-2);
  }

  /* A settings row: a 160px label column with its control right after it. */
  .settings-row {
    display: grid;
    grid-template-columns: minmax(min-content, var(--layout-settings-label-w)) auto;
    justify-content: start;
    align-items: center;
    min-height: 40px;
  }

  /* Section list: a 240px column whose heading sits in the 48px header band.
     Rows sit 8px from the column's edges; their own 8px padding puts the
     labels on the 16px inset, under the heading. */
  .settings-view :global(.kit-settings__sidebar) {
    gap: 0;
    box-sizing: border-box;
    width: 240px;
    padding: 0 var(--space-4) var(--space-4);
  }

  .settings-view :global(.kit-settings__title) {
    display: flex;
    flex: none;
    align-items: center;
    height: var(--layout-header-h);
    padding: 0 var(--space-4);
  }

  .settings-view :global(.kit-settings__nav) {
    gap: var(--space-1);
  }

  .settings-view :global(.kit-settings__group-title) {
    display: flex;
    align-items: center;
    height: var(--settings-row-h);
    padding: 0 var(--space-4);
    font-size: var(--font-size-xs);
  }

  .settings-view :global(.kit-settings__group-title:not(:first-child)) {
    margin-top: var(--space-4);
  }

  .settings-view :global(.kit-settings__nav-item) {
    align-items: center;
    height: var(--settings-row-h);
    padding: 0 var(--space-4);
    font-size: var(--font-size-md);
    line-height: 16px;
  }

  /* The current section reads like the sidebar's current place: the hover
     fill with primary text, never blue. */
  .settings-view :global(.kit-settings__nav-item--active),
  .settings-view :global(.kit-settings__nav-item--active:hover) {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
    font-weight: 400;
  }

  /* Content column: at most 720px of content, centered in the pane with its
     header band. The gutter is reserved on both sides so the scrollbar
     doesn't pull the column off center. The first section's title band
     starts at the top, level with the section list's heading. */
  .settings-view :global(.kit-settings__scroll) {
    scrollbar-gutter: stable both-edges;
  }

  .settings-view :global(.kit-settings__panel) {
    gap: var(--space-7);
    max-width: calc(720px + 2 * var(--layout-inset));
    margin: 0 auto;
    padding: 0 var(--layout-inset) var(--space-8);
  }

  .settings-view :global(.kit-settings-section) {
    position: relative;
    border: 0;
    border-radius: 0;
    background: transparent;
  }

  .settings-view :global(.kit-settings-section__header) {
    display: flex;
    flex-direction: column;
    justify-content: center;
    box-sizing: border-box;
    min-height: var(--layout-header-h);
    padding: 0;
  }

  .settings-view :global(.kit-settings-section__title) {
    font-size: var(--font-size-lg);
    line-height: 16px;
  }

  /* Selects read like the rest of the row: regular weight, body size. */
  .settings-view :global(.kit-select-dropdown__trigger) {
    font-size: var(--font-size-md);
    font-weight: 400;
  }

  .settings-view :global(.kit-settings-section__body) {
    gap: var(--space-4);
    padding: var(--space-4) 0 0;
  }

  /* Narrow: the slim bar already names the view, so the section list keeps
     only its rows, wrapping under the bar instead of scrolling sideways. */
  @media (max-width: 760px) {
    .settings-view :global(.kit-settings__sidebar) {
      width: 100%;
      padding: var(--space-4);
      overflow: visible;
    }

    .settings-view :global(.kit-settings__title),
    .settings-view :global(.kit-settings__group-title) {
      display: none;
    }

    .settings-view :global(.kit-settings__nav) {
      flex-wrap: wrap;
      gap: var(--space-1);
    }

    .settings-view :global(.kit-settings__scroll) {
      scrollbar-gutter: stable;
    }

    .settings-view :global(.kit-settings__panel) {
      max-width: none;
    }
  }
</style>
