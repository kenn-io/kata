<script lang="ts">
  import type { KataRoute } from '../lib/router'

  interface Props {
    route: Extract<KataRoute, { kind: 'route-error' }>
    onSearch: (reference: string) => void
  }

  let { route, onSearch }: Props = $props()
</script>

<section aria-labelledby="route-error-heading">
  <h2 id="route-error-heading">This Kata route is not valid</h2>
  <p>The address was left unchanged so it can be corrected or searched.</p>
  {#if route.searchRef}
    <button type="button" onclick={() => onSearch(route.searchRef ?? '')}>
      Search for {route.searchRef}
    </button>
  {/if}
</section>
