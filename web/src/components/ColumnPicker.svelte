<script lang="ts">
  import { Checkbox, SegmentedControl } from '@kenn-io/kit-ui'
  import type { KataOptionalTaskColumn, KataTaskColumnVisibility } from '../lib/kata/columns'
  import DisplayMenu from './DisplayMenu.svelte'

  interface Props {
    visibility: KataTaskColumnVisibility
    columns: readonly { id: KataOptionalTaskColumn; label: string }[]
    splitDirection: 'horizontal' | 'vertical'
    onSplitDirectionChange: (direction: 'horizontal' | 'vertical') => void
    onchange: (visibility: KataTaskColumnVisibility) => void
  }

  let { visibility, columns, splitDirection, onSplitDirectionChange, onchange }: Props = $props()
</script>

<DisplayMenu>
  <div class="display-menu__section">
    <div class="display-menu__title">Task opens</div>
    <SegmentedControl
      ariaLabel="Task opens"
      options={[
        { value: 'horizontal', label: 'Beside list' },
        { value: 'vertical', label: 'Below list' },
      ]}
      value={splitDirection}
      onchange={(value) => onSplitDirectionChange(value as 'horizontal' | 'vertical')}
      block
    />
  </div>
  <div class="display-menu__section">
    <div class="display-menu__title">Columns, shown when space allows</div>
    {#each columns as column (column.id)}
      <Checkbox
        checked={visibility[column.id]}
        label={column.label}
        onchange={(checked) => onchange({ ...visibility, [column.id]: checked })}
      />
    {/each}
  </div>
</DisplayMenu>
