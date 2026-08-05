<script lang="ts">
  import { Modal as KitModal } from '@kenn-io/kit-ui'
  import type { ComponentProps } from 'svelte'
  import type { Snippet } from 'svelte'

  type KitModalFooter = NonNullable<ComponentProps<typeof KitModal>['footer']>

  // Shared in-app dialog shell: adapts kit-ui Modal (backdrop, frame, header,
  // focus trap, scroll lock, Escape/overlay close, focus restore) to this
  // app's contract — an `open` prop, pixel widths, and a keyboard modal-stack
  // frame so global shortcuts stay suppressed while any dialog is open.
  // Feature components own only their body content and domain actions.
  interface Props {
    open: boolean
    title?: string | undefined
    ariaLabel?: string | undefined
    width?: number | undefined
    // Header close (X) button, off by default. Dialogs here provide an explicit
    // Cancel/close in their footer, so an X would duplicate it and add a stop to
    // the focus trap. Opt in with showClose only for content-only dialogs that
    // have no other dismiss control. kit Modal's `closable` prop gates ONLY the
    // header X; Escape (escapeCloses) and backdrop click (closeOnOverlayClick,
    // left at its default true) are independent of it, so every dialog stays
    // dismissable regardless of showClose.
    showClose?: boolean
    onClose: () => void
    children: Snippet
    footer?: Snippet | undefined
  }

  let {
    open,
    title = undefined,
    ariaLabel = undefined,
    width = 440,
    showClose = false,
    onClose,
    children,
    footer = undefined,
  }: Props = $props()

  let bodyEl: HTMLElement | null = $state(null)
  let footEl: HTMLElement | null = $state(null)

  $effect(() => {
    if (!open) return
    queueMicrotask(() => {
      // Preserve focus claimed by a mounted child control first. Shared inputs
      // implement autofocus in their mount attachment rather than by retaining
      // an HTML autofocus attribute, so replacing it here would move focus back
      // to the first field in the dialog.
      const active = document.activeElement
      if (active instanceof HTMLElement && (bodyEl?.contains(active) || footEl?.contains(active))) {
        return
      }

      // Otherwise prefer an explicit [data-autofocus] control, then a body
      // input/textarea, then the first enabled body button, then the first
      // enabled footer button — so confirmation dialogs land on their first
      // action (Cancel) instead of the panel.
      const explicit =
        bodyEl?.querySelector<HTMLElement>('[data-autofocus]') ??
        footEl?.querySelector<HTMLElement>('[data-autofocus]')
      const primary = bodyEl?.querySelector<HTMLElement>(
        "input:not([type='hidden']):not([disabled]), textarea:not([disabled])",
      )
      const action =
        bodyEl?.querySelector<HTMLElement>('button:not([disabled])') ??
        footEl?.querySelector<HTMLElement>('button:not([disabled])')
      ;(explicit ?? primary ?? action)?.focus()
    })
  })
</script>

{#snippet footerContent()}
  <div class="modal-scope" bind:this={footEl}>
    {@render footer?.()}
  </div>
{/snippet}

{#if open}
  <!-- Conditional spreads: kit Modal's optional props are declared without
       `| undefined`, so explicit undefined fails under
       exactOptionalPropertyTypes. Omit the props instead. -->
  <KitModal
    {...title !== undefined ? { title } : {}}
    {...ariaLabel !== undefined ? { ariaLabel } : {}}
    {...footer ? { footer: footerContent as KitModalFooter } : {}}
    width="100%"
    maxWidth={`min(${width}px, calc(100vw - 40px))`}
    closable={showClose}
    onclose={onClose}
  >
    <div class="modal-scope" bind:this={bodyEl}>
      {@render children()}
    </div>
  </KitModal>
{/if}

<style>
  /* Layout-transparent wrappers that exist only to scope the initial-focus
     query per dialog instance (stacked dialogs must not steal each other's
     focus target). */
  .modal-scope {
    display: contents;
  }
</style>
