<script lang="ts">
  import { MiniMap, useStore, useSvelteFlow, type MiniMapProps } from '@xyflow/svelte'
  import { graphOverflowsView } from '../lib/graph/viewport'

  let props: MiniMapProps = $props()
  const store = $derived(useStore())
  const { getNodesBounds } = useSvelteFlow()
  let overflows = $derived(
    graphOverflowsView(
      getNodesBounds(store.nodes.filter((node) => !node.hidden)),
      store.viewport,
      store.width,
      store.height,
    ),
  )
</script>

{#if overflows}<MiniMap {...props} />{/if}
