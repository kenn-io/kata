<script lang="ts">
  import { SegmentedControl, SettingsSection, Tabs, type TabItem } from '@kenn-io/kit-ui'
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

  let tabs = $derived<TabItem[]>([
    { id: 'appearance', label: 'Appearance' },
    { id: 'search', label: 'Search by meaning' },
    ...(credentialsAvailable ? [{ id: 'credentials', label: 'Credentials' }] : []),
    ...(accessAvailable ? [{ id: 'access', label: 'Teams and visibility' }] : []),
  ])
</script>

<div class="settings-view">
  <Tabs
    {tabs}
    value={active}
    ariaLabel="Settings"
    onchange={(id) => onSelect(id as SettingsSectionID)}
    tabAttributes={(item, selected) => ({
      onclick: selected ? () => onSelect(item.id as SettingsSectionID) : undefined,
    })}
  >
    {#snippet children(id)}
      <div class="settings-content">
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
      </div>
    {/snippet}
  </Tabs>
</div>

<style>
  .settings-view {
    flex: 1 1 auto;
    min-width: 0;
    min-height: 0;
    overflow: hidden;
    background: var(--bg-surface);
  }

  .settings-view > :global(.kit-tabs) {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-width: 0;
  }

  .settings-view :global(.kit-tabs__list) {
    flex: none;
    width: min(100%, calc(720px + 2 * var(--layout-inset)));
    box-sizing: border-box;
    margin: 0 auto;
    padding: 0 var(--layout-inset);
  }

  .settings-view :global(.kit-tabs__panel) {
    flex: 1 1 auto;
    min-height: 0;
    overflow-y: auto;
    scrollbar-gutter: stable both-edges;
  }

  .settings-content {
    display: flex;
    flex-direction: column;
    gap: var(--space-7);
    max-width: calc(720px + 2 * var(--layout-inset));
    box-sizing: border-box;
    margin: 0 auto;
    padding: 0 var(--layout-inset) var(--space-8);
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

  @media (max-width: 760px) {
    .settings-view :global(.kit-tabs__panel) {
      scrollbar-gutter: stable;
    }
  }
</style>
