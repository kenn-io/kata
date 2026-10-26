<script lang="ts">
  import type { ComponentProps } from 'svelte'
  import CronPage from './CronPage.svelte'
  import CronEditorHost, { type CronEditorHandle } from './CronEditorHost.svelte'

  let {
    onLoadMore,
    onOpenIssue,
    ...props
  }: ComponentProps<typeof CronEditorHost> & {
    onLoadMore: (projectUID: string) => void
    onOpenIssue: (projectUID: string, uid: string) => void
  } = $props()
  let editor: CronEditorHandle | undefined = $state()
</script>

<CronPage
  route={props.route}
  state={props.state}
  onNavigate={props.onNavigate}
  onRefresh={props.onRefresh}
  canMutate={props.canMutate ?? false}
  mutationPending={props.mutationPending ?? false}
  {onLoadMore}
  {onOpenIssue}
  {editor}
/>
<CronEditorHost bind:this={editor} {...props} />
