<script lang="ts">
  import { useStore, useSvelteFlow, type FitViewOptions } from '@xyflow/svelte'

  let { options, signature }: { options: FitViewOptions; signature: string } = $props()
  const store = useStore()
  const { fitView } = useSvelteFlow()

  $effect(() => {
    if (!signature || !store.width || !store.height) return
    const frame = requestAnimationFrame(() => void fitView(options))
    return () => cancelAnimationFrame(frame)
  })
</script>
