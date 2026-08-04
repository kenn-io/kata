<script lang="ts">
  import { SplitResizeHandle, type SplitResizeEvent } from '@kenn-io/kit-ui'
  import type { Snippet } from 'svelte'

  type Orientation = 'vertical' | 'horizontal'
  type SnippetFunction = () => ReturnType<Snippet>

  interface Props {
    orientation: Orientation
    primarySize: number
    minPrimary?: number
    minSecondary?: number
    responsiveBreakpoint?: number
    ariaLabel: string
    onResize: (size: number) => void
    primary: SnippetFunction
    secondary: SnippetFunction
  }

  let {
    orientation,
    primarySize,
    minPrimary = 200,
    minSecondary = 200,
    responsiveBreakpoint = 0,
    ariaLabel,
    onResize,
    primary,
    secondary,
  }: Props = $props()

  let container: HTMLDivElement | null = $state(null)
  let containerWidth = $state(0)
  let containerHeight = $state(0)
  let resizeStartSize = 0
  let resizeTotalSize = 0

  const effectiveOrientation = $derived(
    orientation === 'horizontal' &&
      responsiveBreakpoint > 0 &&
      containerWidth < responsiveBreakpoint
      ? 'vertical'
      : orientation,
  )
  const totalSize = $derived(effectiveOrientation === 'vertical' ? containerHeight : containerWidth)

  function axisSize(rect: DOMRect): number {
    return effectiveOrientation === 'vertical' ? rect.height : rect.width
  }

  function measure(rect: DOMRect): void {
    containerWidth = rect.width
    containerHeight = rect.height
  }

  function clampSize(size: number, total: number): number {
    if (total <= 0) return Math.max(minPrimary, size)
    const maxPrimary = Math.max(minPrimary, total - minSecondary)
    return Math.max(minPrimary, Math.min(maxPrimary, size))
  }

  $effect(() => {
    if (!container) return
    measure(container.getBoundingClientRect())
    if (typeof ResizeObserver === 'undefined') return
    const observer = new ResizeObserver((entries) => {
      const entry = entries[0]
      if (!entry) return
      measure(entry.target.getBoundingClientRect())
    })
    observer.observe(container)
    return () => observer.disconnect()
  })

  $effect(() => {
    if (totalSize <= 0) return
    const clamped = clampSize(primarySize, totalSize)
    if (clamped !== primarySize) onResize(clamped)
  })

  function startResize(): void {
    if (!container) return
    resizeStartSize = primarySize
    resizeTotalSize = axisSize(container.getBoundingClientRect())
  }

  function handleResize(event: SplitResizeEvent): void {
    const multiplier = event.event instanceof KeyboardEvent && event.event.shiftKey ? 4 : 1
    onResize(clampSize(resizeStartSize + event.delta * multiplier, resizeTotalSize))
  }

  const appliedSize = $derived(totalSize > 0 ? clampSize(primarySize, totalSize) : primarySize)
  const valueMax = $derived(
    totalSize > 0 ? Math.max(minPrimary, totalSize - minSecondary) : minPrimary,
  )
</script>

<div
  class={['kata-sash', `kata-sash--${effectiveOrientation}`]}
  data-orientation={effectiveOrientation}
  bind:this={container}
>
  <div class="pane pane-primary" style:flex-basis={`${Math.round(appliedSize)}px`}>
    {@render primary()}
  </div>
  <SplitResizeHandle
    class="sash-handle"
    orientation={effectiveOrientation}
    {ariaLabel}
    ariaValueMin={minPrimary}
    ariaValueMax={Math.round(valueMax)}
    ariaValueNow={Math.round(appliedSize)}
    keyboardStep={16}
    onResizeStart={startResize}
    onResize={handleResize}
  />
  <div class="pane pane-secondary">
    {@render secondary()}
  </div>
</div>

<style>
  .kata-sash {
    min-width: 0;
    min-height: 0;
    display: flex;
    flex: 1 1 auto;
    overflow: hidden;
  }

  .kata-sash--vertical {
    flex-direction: column;
  }

  .kata-sash--horizontal {
    flex-direction: row;
  }

  .pane {
    min-width: 0;
    min-height: 0;
    display: flex;
    overflow: hidden;
  }

  .pane-primary {
    flex: 0 0 auto;
  }

  .pane-secondary {
    flex: 1 1 auto;
  }

  :global(.sash-handle) {
    flex: 0 0 auto;
    background: var(--border-default);
  }
</style>
