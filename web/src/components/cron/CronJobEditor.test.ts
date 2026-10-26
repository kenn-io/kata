import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, expect, test, vi } from 'vitest'
import CronJobEditor from './CronJobEditor.svelte'
import { parseCronDocument, rawCronValue } from '../../lib/cron/document'
import { buildCronJobDraft, serializeCronJobDraft } from '../../lib/cron/jobDraft'
afterEach(cleanup)
const props = () => ({
  draft: buildCronJobDraft(undefined, 1),
  workflows: [],
  projectUID: 'p1',
  readonly: false,
  pending: false,
  onchange: vi.fn(),
  onsubmit: vi.fn(),
  oncancel: vi.fn(),
  resolveIssue: vi.fn<(projectID: number, reference: string) => Promise<string>>(
    async () => '01ARZ3NDEKTSV4RRFFQ69G5FAV',
  ),
})
test('invalid field text remains visible and prevents submission', async () => {
  const p = props()
  render(CronJobEditor, p)
  await fireEvent.input(screen.getByRole('textbox', { name: 'Timeout' }), {
    target: { value: '9007199254740993' },
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
  expect(p.onsubmit).not.toHaveBeenCalled()
  expect((screen.getByRole('textbox', { name: 'Timeout' }) as HTMLInputElement).value).toBe(
    '9007199254740993',
  )
  expect(screen.getAllByRole('alert').length).toBeGreaterThan(0)
})
test.each([
  { readonly: true, pending: false },
  { readonly: false, pending: true },
])('authority/pending disable form %o', (state) => {
  render(CronJobEditor, { ...props(), ...state })
  expect((screen.getByRole('button', { name: 'Save job' }) as HTMLButtonElement).disabled).toBe(
    true,
  )
  expect((screen.getByRole('textbox', { name: 'Name' }) as HTMLInputElement).disabled).toBe(true)
})
test('references resolve once to canonical UIDs before submit', async () => {
  const p = props()
  p.draft.values = {
    ...p.draft.values,
    name: 'Example',
    prompt: 'Do work',
    issue_kind: 'existing',
    issue: 'abc4',
  }
  render(CronJobEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
  await vi.waitFor(() => expect(p.onsubmit).toHaveBeenCalledTimes(1))
  expect(p.resolveIssue).toHaveBeenCalledWith(1, 'abc4')
  expect(p.onsubmit.mock.calls[0]?.[0].values.issue).toBe('01ARZ3NDEKTSV4RRFFQ69G5FAV')
  await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
  await vi.waitFor(() => expect(p.onsubmit).toHaveBeenCalledTimes(2))
  expect(p.resolveIssue).toHaveBeenCalledTimes(1)
})
test('authority loss during issue resolution prevents submit and retains draft', async () => {
  const p = props()
  p.draft.values = {
    ...p.draft.values,
    name: 'Example',
    prompt: 'Do work',
    issue_kind: 'existing',
    issue: 'abc4',
  }
  let resolve!: (uid: string) => void
  p.resolveIssue = vi.fn(
    () =>
      new Promise<string>((r) => {
        resolve = r
      }),
  )
  const view = render(CronJobEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
  await view.rerender({ ...p, readonly: true })
  resolve('01ARZ3NDEKTSV4RRFFQ69G5FAV')
  await vi.waitFor(() =>
    expect((screen.getByRole('button', { name: 'Save job' }) as HTMLButtonElement).disabled).toBe(
      true,
    ),
  )
  expect(p.onsubmit).not.toHaveBeenCalled()
})
test('a successfully resolved reference stays canonical when another lookup fails', async () => {
  const p = props(),
    uid = '01ARZ3NDEKTSV4RRFFQ69G5FAV'
  p.draft.values = {
    ...p.draft.values,
    name: 'Example',
    prompt: 'Do work',
    issue_kind: 'existing',
    issue: 'abc4',
    trigger: 'issue-scheduled',
    trigger_issue: 'def4',
  }
  p.resolveIssue = vi
    .fn()
    .mockResolvedValueOnce(uid)
    .mockRejectedValueOnce(new Error('Lookup unavailable'))
    .mockResolvedValue(uid)
  render(CronJobEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
  await screen.findByText('Lookup unavailable')
  expect((screen.getByRole('textbox', { name: 'Existing issue' }) as HTMLInputElement).value).toBe(
    uid,
  )
  await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
  await vi.waitFor(() => expect(p.onsubmit).toHaveBeenCalledTimes(1))
  expect(p.resolveIssue.mock.calls.map((args) => args[1])).toEqual(['abc4', 'def4', 'def4'])
})
test('closing the drawer during reference lookup cannot save after it unmounts', async () => {
  const p = props()
  p.draft.values = {
    ...p.draft.values,
    name: 'Example',
    prompt: 'Do work',
    issue_kind: 'existing',
    issue: 'abc4',
  }
  let resolve!: (uid: string) => void
  p.resolveIssue = vi.fn(
    () =>
      new Promise<string>((r) => {
        resolve = r
      }),
  )
  const view = render(CronJobEditor, p)
  await fireEvent.click(screen.getByRole('button', { name: 'Save job' }))
  view.unmount()
  resolve('01ARZ3NDEKTSV4RRFFQ69G5FAV')
  await new Promise((r) => setTimeout(r, 0))
  expect(p.onsubmit).not.toHaveBeenCalled()
})

test.each([false, true])(
  'round trip through recurring restores the rendered ordinary overlap (persisted %s)',
  async (persisted) => {
    const p = props()
    if (persisted)
      p.draft = buildCronJobDraft(
        {
          uid: 'job1',
          name: 'Example',
          definitionEventUID: 'event1',
          document: parseCronDocument(
            '{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"cron","cron":"0 9 * * *"},"action":{"kind":"execute","prompt":"Do work"},"issue":{"kind":"per-run","title":"Result"},"overlap":"forbid","catchup":"all"}',
          ),
        },
        1,
      )
    render(CronJobEditor, p)
    await fireEvent.change(screen.getByRole('combobox', { name: 'Action' }), {
      target: { value: 'recurring' },
    })
    await fireEvent.change(screen.getByRole('combobox', { name: 'Action' }), {
      target: { value: 'prompt' },
    })
    const visible = (screen.getByRole('combobox', { name: 'Overlap' }) as HTMLSelectElement).value
    expect(visible).toBe('forbid')
    const changed = p.onchange.mock.calls.at(-1)![0]
    expect(rawCronValue(serializeCronJobDraft(changed), ['overlap'])).toBe(JSON.stringify(visible))
  },
)
test('conflict comparison omits internal mode-roundtrip bookkeeping', async () => {
  const p = props(),
    record = {
      uid: 'job1',
      name: 'Example',
      definitionEventUID: 'new-event',
      document: parseCronDocument(
        '{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"cron","cron":"0 9 * * *"},"action":{"kind":"execute","prompt":"Do work"},"issue":{"kind":"per-run","title":"Result"},"overlap":"forbid","catchup":"all"}',
      ),
    }
  p.draft = buildCronJobDraft(record, 1)
  const view = render(CronJobEditor, p)
  await fireEvent.change(screen.getByRole('combobox', { name: 'Action' }), {
    target: { value: 'recurring' },
  })
  await fireEvent.change(screen.getByRole('combobox', { name: 'Action' }), {
    target: { value: 'prompt' },
  })
  await view.rerender({ ...p, draft: p.onchange.mock.calls.at(-1)![0], conflict: record })
  expect(screen.getByRole('region', { name: 'Definition conflict' }).textContent).not.toContain(
    'overlap:ordinary',
  )
})
