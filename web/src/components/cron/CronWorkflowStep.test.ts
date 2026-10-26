import { cleanup, fireEvent, render, screen } from '@testing-library/svelte'
import { afterEach, expect, test, vi } from 'vitest'
import CronWorkflowStep from './CronWorkflowStep.svelte'
import { addWorkflowStep, buildWorkflowDraft } from '../../lib/cron/workflowDraft'
afterEach(cleanup)
test('step field diagnostics expose their field and preserve invalid retries', async () => {
  const step = addWorkflowStep(buildWorkflowDraft(undefined, 1), 'command').steps[0]!
  const change = vi.fn()
  render(CronWorkflowStep, {
    step,
    problems: [
      {
        editorID: step.editorID,
        field: 'retries',
        code: 'invalid',
        message: 'Retries must be an integer from 0 through 100.',
      },
    ],
    onchange: change,
    ondelete: vi.fn(),
    onkeyerror: vi.fn(),
  })
  expect(screen.getByRole('alert').textContent).toContain('Retries')
  await fireEvent.input(screen.getByRole('textbox', { name: 'Retries' }), {
    target: { value: '-1' },
  })
  expect(change).toHaveBeenCalledWith('retries', '-1')
})
