import { describe, expect, it } from 'vitest'

import { findRawButtons } from './check-kit-ui-usage'

describe('Kata UI usage check', () => {
  it('rejects raw buttons and accepts the kit Button component', () => {
    expect(findRawButtons('<button type="button">Save</button>')).toEqual([
      {
        line: 1,
        message: 'raw <button> element; use Button from @kenn-io/kit-ui',
      },
    ])

    expect(
      findRawButtons(`
        <script>
          import { Button } from '@kenn-io/kit-ui'
        </script>
        <Button type="button" label="Save" />
      `),
    ).toEqual([])
  })
})
