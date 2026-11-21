<script lang="ts">
  import { Button } from '@kenn-io/kit-ui'
  import type { KataRoute } from '../lib/router'
  import StatusPage from './shared/StatusPage.svelte'

  interface Props {
    route: Extract<KataRoute, { kind: 'route-error' }>
    onSearch: (reference: string) => void
    onOpenInbox: () => void
  }

  let { route, onSearch, onOpenInbox }: Props = $props()
</script>

<StatusPage title="Page not found" description={route.searchRef ? undefined : 'Check the address.'}>
  {#if route.searchRef}
    <Button
      tone="info"
      surface="solid"
      label={`Search for ${route.searchRef}`}
      onclick={() => onSearch(route.searchRef ?? '')}
    />
  {/if}
  <Button tone="info" surface="solid" label="Open Inbox" onclick={onOpenInbox} />
</StatusPage>
