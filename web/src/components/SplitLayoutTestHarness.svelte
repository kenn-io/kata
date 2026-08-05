<script lang="ts">
  import SplitLayout from './SplitLayout.svelte'

  interface Props {
    orientation?: 'horizontal' | 'vertical'
    primarySize?: number
    minPrimary?: number
    minSecondary?: number
    responsiveBreakpoint?: number
    onResize?: (size: number) => void
  }

  let {
    orientation = 'horizontal',
    primarySize = 300,
    minPrimary = 200,
    minSecondary = 200,
    responsiveBreakpoint = 0,
    onResize = () => {},
  }: Props = $props()
</script>

<SplitLayout
  {orientation}
  {primarySize}
  {minPrimary}
  {minSecondary}
  {responsiveBreakpoint}
  ariaLabel="Resize Kata panes"
  {onResize}
>
  {#snippet primary()}Primary{/snippet}
  {#snippet secondary()}Secondary{/snippet}
</SplitLayout>
