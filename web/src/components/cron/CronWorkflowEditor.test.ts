import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte'
import { afterEach, expect, test, vi } from 'vitest'
import CronWorkflowEditor from './CronWorkflowEditor.svelte'
import { buildWorkflowDraft, serializeWorkflowDraft } from '../../lib/cron/workflowDraft'
import { parseCronDocument, rawCronValue } from '../../lib/cron/document'
afterEach(cleanup)
const source =
  '{"version":1,"about":"About","input":"Input","options":{"big":9007199254740993},"steps":[{"key":"first","kind":"command","command":"echo ok","options":{"big":9007199254740993}},{"key":"second","kind":"prompt","prompt":"Continue","after":["first"]}]}'
const props = () => ({
  draft: buildWorkflowDraft(
    {
      uid: 'workflow1',
      name: 'Example',
      definitionEventUID: 'event1',
      document: parseCronDocument(source),
    },
    1,
  ),
  onchange: vi.fn(),
  onsubmit: vi.fn(),
  oncancel: vi.fn(),
})
test('all workflow and step fields edit losslessly and submit the current draft', async () => {
  const p = props()
  render(CronWorkflowEditor, p)
  for (const [name, value] of [
    ['Name', 'New name'],
    ['About', 'New about'],
    ['Input', 'New input'],
    ['Workflow options', '{"big":9007199254740995}'],
  ] as const)
    await fireEvent.input(screen.getByRole('textbox', { name }), { target: { value } })
  const row = within(screen.getByRole('group', { name: 'Step first' }))
  await fireEvent.input(row.getByRole('textbox', { name: 'Command' }), {
    target: { value: 'echo changed' },
  })
  await fireEvent.input(row.getByRole('textbox', { name: 'Retries' }), { target: { value: '100' } })
  await fireEvent.input(row.getByRole('textbox', { name: 'Step options' }), {
    target: { value: '{"exp":1e400}' },
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Save workflow' }))
  expect(p.onsubmit).toHaveBeenCalledTimes(1)
  const changed = p.onsubmit.mock.calls[0]![0],
    output = serializeWorkflowDraft(changed)
  expect(changed.values.name).toBe('New name')
  expect(rawCronValue(output, ['about'])).toBe('"New about"')
  expect(rawCronValue(output, ['input'])).toBe('"New input"')
  expect(rawCronValue(output, ['options', 'big'])).toBe('9007199254740995')
  expect(rawCronValue(output, ['steps', 0, 'options', 'exp'])).toBe('1e400')
  expect(rawCronValue(output, ['steps', 0, 'retries'])).toBe('100')
  expect(rawCronValue(output, ['steps', 0, 'command'])).toBe('"echo changed"')
})
test('key rename keeps the focused control and updates dependent rows', async () => {
  render(CronWorkflowEditor, props())
  const input = within(screen.getByRole('group', { name: 'Step first' })).getByRole('textbox', {
    name: 'Key',
  }) as HTMLInputElement
  input.focus()
  await fireEvent.input(input, { target: { value: 'renamed' } })
  expect(document.activeElement).toBe(input)
  expect(
    within(screen.getByRole('group', { name: 'Step renamed' })).getByRole('textbox', {
      name: 'Key',
    }),
  ).toBe(input)
  expect(
    (
      within(screen.getByRole('group', { name: 'Step second' })).getByRole('textbox', {
        name: 'Runs after',
      }) as HTMLInputElement
    ).value,
  ).toBe('renamed')
})
test('invalid rename stays visible, reports error, and blocks save without partially rewriting dependents', async () => {
  const p = props()
  render(CronWorkflowEditor, p)
  const row = within(screen.getByRole('group', { name: 'Step first' }))
  await fireEvent.input(row.getByRole('textbox', { name: 'Key' }), { target: { value: 'second' } })
  expect((row.getByRole('textbox', { name: 'Key' }) as HTMLInputElement).value).toBe('second')
  expect(screen.getByText(/keys must be unique/)).toBeTruthy()
  await fireEvent.click(screen.getByRole('button', { name: 'Save workflow' }))
  expect(p.onsubmit).not.toHaveBeenCalled()
  expect(
    (
      within(screen.getByRole('group', { name: 'Step second' })).getByRole('textbox', {
        name: 'Runs after',
      }) as HTMLInputElement
    ).value,
  ).toBe('first')
})
test('dependent deletion lists direct references and requires explicit confirmation', async () => {
  const p = props()
  render(CronWorkflowEditor, p)
  await fireEvent.click(
    within(screen.getByRole('group', { name: 'Step first' })).getByRole('button', {
      name: 'Delete step',
    }),
  )
  const confirm = screen.getByRole('region', { name: 'Confirm step deletion' })
  expect(confirm.textContent).toContain('second')
  expect(screen.getByRole('group', { name: 'Step first' })).toBeTruthy()
  await fireEvent.click(within(confirm).getByRole('button', { name: 'Keep step' }))
  expect(screen.getByRole('group', { name: 'Step first' })).toBeTruthy()
  await fireEvent.click(
    within(screen.getByRole('group', { name: 'Step first' })).getByRole('button', {
      name: 'Delete step',
    }),
  )
  await fireEvent.click(screen.getByRole('button', { name: 'Delete step and remove references' }))
  expect(screen.queryByRole('group', { name: 'Step first' })).toBeNull()
  expect((screen.getByRole('textbox', { name: 'Runs after' }) as HTMLInputElement).value).toBe('')
})
test('live row and graph errors flag later dependencies and prevent save', async () => {
  const p = props()
  render(CronWorkflowEditor, p)
  await fireEvent.input(
    within(screen.getByRole('group', { name: 'Step first' })).getByRole('textbox', {
      name: 'Runs after',
    }),
    { target: { value: 'second, missing' } },
  )
  expect(
    within(screen.getByRole('group', { name: 'Step first' })).getByText(/must name an earlier/),
  ).toBeTruthy()
  expect(
    within(screen.getByRole('region', { name: 'Workflow dependency graph' })).getByText(
      /Missing dependency/,
    ),
  ).toBeTruthy()
  await fireEvent.click(screen.getByRole('button', { name: 'Save workflow' }))
  expect(p.onsubmit).not.toHaveBeenCalled()
})
test('typing a dependency separator retains the text so another dependency can be entered', async () => {
  render(CronWorkflowEditor, props())
  const input = within(screen.getByRole('group', { name: 'Step second' })).getByRole('textbox', {
    name: 'Runs after',
  }) as HTMLInputElement
  await fireEvent.input(input, { target: { value: 'first, ' } })
  expect(input.value).toBe('first, ')
  await fireEvent.input(input, { target: { value: 'first, missing' } })
  expect(input.value).toBe('first, missing')
})
test('invalid key buffer remains visible through unrelated edits until explicitly corrected', async () => {
  const p = props()
  render(CronWorkflowEditor, p)
  const row = within(screen.getByRole('group', { name: 'Step first' })),
    key = row.getByRole('textbox', { name: 'Key' }) as HTMLInputElement
  await fireEvent.input(key, { target: { value: 'second' } })
  await fireEvent.input(row.getByRole('textbox', { name: 'Command' }), {
    target: { value: 'echo changed' },
  })
  expect(key.value).toBe('second')
  expect(screen.getByText(/keys must be unique/)).toBeTruthy()
  await fireEvent.input(key, { target: { value: 'corrected' } })
  expect(screen.queryByText(/keys must be unique/)).toBeNull()
  await fireEvent.click(screen.getByRole('button', { name: 'Save workflow' }))
  expect(p.onsubmit).toHaveBeenCalledTimes(1)
})
test('kind changes clear the dormant field, and adding both step kinds exposes their fields', async () => {
  render(CronWorkflowEditor, props())
  const first = within(screen.getByRole('group', { name: 'Step first' }))
  await fireEvent.change(first.getByRole('combobox', { name: 'Kind' }), {
    target: { value: 'prompt' },
  })
  expect(first.queryByRole('textbox', { name: 'Command' })).toBeNull()
  expect(first.getByRole('textbox', { name: 'Prompt' })).toBeTruthy()
  await fireEvent.click(screen.getByRole('button', { name: 'Add command step' }))
  await fireEvent.click(screen.getByRole('button', { name: 'Add prompt step' }))
  expect(screen.getAllByRole('group').length).toBe(4)
})
test.each([{ readonly: true }, { pending: true }])(
  'authority and pending disable every mutation %o',
  (state) => {
    render(CronWorkflowEditor, { ...props(), ...state })
    expect((screen.getByRole('textbox', { name: 'Name' }) as HTMLInputElement).disabled).toBe(true)
    expect(
      (screen.getByRole('button', { name: 'Save workflow' }) as HTMLButtonElement).disabled,
    ).toBe(true)
    expect(
      (screen.getByRole('button', { name: 'Add command step' }) as HTMLButtonElement).disabled,
    ).toBe(true)
  },
)
