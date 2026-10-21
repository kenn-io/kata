// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import IssueAttachments from './src/components/IssueAttachments.svelte'

const id = '01JB3Z6V2K8M4Q9T1X5W7Y0RNA'
const entry = {
  name: 'screen.png',
  media_type: 'image/png',
  size: 12,
  added_by: 'operator',
  added_at: '2026-10-20T23:10:00Z',
  docbank: {
    vault_uid: '6f1c2e8a-1111-4111-8111-111111111111',
    node_id: 42,
    content_version_id: '6f1c2e8a-2222-4222-8222-222222222222',
    content_sha256: 'a'.repeat(64),
  },
}
beforeEach(() => {
  vi.stubGlobal(
    'URL',
    Object.assign(URL, {
      createObjectURL: vi.fn(() => 'blob:preview'),
      revokeObjectURL: vi.fn(),
    }),
  )
})
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})
it('loads image previews through the host fetch and revokes URLs on unmount', async () => {
  const fetchAttachment = vi.fn(async () => new Blob(['image'], { type: 'image/png' }))
  const result = render(IssueAttachments, {
    attachments: { [id]: entry },
    baseURL: '/api/v1/projects/7/issues/abc4/attachments',
    fetchAttachment,
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Preview screen.png' }))
  await waitFor(() =>
    expect(fetchAttachment).toHaveBeenCalledWith(
      `/api/v1/projects/7/issues/abc4/attachments/${id}/content`,
    ),
  )
  await waitFor(() =>
    expect(screen.getByAltText('screen.png').getAttribute('src')).toBe('blob:preview'),
  )
  result.unmount()
  expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:preview')
})
it('clears previews when authority is lost, including pending fetches', async () => {
  let resolve!: (b: Blob) => void
  const result = render(IssueAttachments, {
    attachments: { [id]: entry },
    baseURL: '/attachments',
    fetchAttachment: () =>
      new Promise<Blob>((done) => {
        resolve = done
      }),
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Preview screen.png' }))
  await result.rerender({
    attachments: { [id]: entry },
    baseURL: '/attachments',
    fetchAttachment: undefined,
  })
  resolve(new Blob(['image']))
  await Promise.resolve()
  expect(screen.queryByAltText('screen.png')).toBeNull()
  expect(URL.createObjectURL).not.toHaveBeenCalled()
})
it('offers download only for video and images over 20 MiB; large files show the CLI hint', () => {
  render(IssueAttachments, {
    attachments: {
      [id]: { ...entry, name: 'recording.mp4', media_type: 'video/mp4' },
      [id.slice(0, -1) + 'B']: { ...entry, size: 21 << 20 },
      [id.slice(0, -1) + 'C']: { ...entry, name: 'large.png', size: 101 << 20 },
    },
    baseURL: '/attachments',
    fetchAttachment: vi.fn(),
  })
  expect(screen.queryByRole('button', { name: /Preview/ })).toBeNull()
  expect(screen.getByRole('button', { name: 'Download recording.mp4' })).toBeTruthy()
  expect(screen.getByText(/kata docbank attachments get/)).toBeTruthy()
  expect(screen.getByText(/video\/mp4/)).toBeTruthy()
})
it('shows an unavailable state while preserving name, type and size', async () => {
  render(IssueAttachments, {
    attachments: { [id]: entry },
    baseURL: '/attachments',
    fetchAttachment: async () => {
      throw new Error('docbank_not_configured')
    },
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Preview screen.png' }))
  await waitFor(() => expect(screen.getByText('docbank_not_configured')).toBeTruthy())
  expect(screen.getByText('screen.png')).toBeTruthy()
  expect(screen.getByText(/12 bytes/)).toBeTruthy()
})
it('lists malformed references as invalid without a fetch control', () => {
  render(IssueAttachments, {
    attachments: { [id]: { ...entry, docbank: {} } },
    baseURL: '/attachments',
    fetchAttachment: vi.fn(),
  })
  expect(screen.getByText('invalid')).toBeTruthy()
  expect(screen.queryByRole('button')).toBeNull()
})
it('renders nothing for an empty map', () => {
  const { container } = render(IssueAttachments, {
    attachments: {},
    baseURL: '/attachments',
  })
  expect(container.textContent).toBe('')
})

it('allows a normal 1 MiB image to preview and download', () => {
  render(IssueAttachments, {
    attachments: { [id]: { ...entry, size: 1 << 20 } },
    baseURL: '/attachments',
    fetchAttachment: vi.fn(),
  })
  expect(screen.getByRole('button', { name: 'Preview screen.png' })).toBeTruthy()
  expect(screen.getByRole('button', { name: 'Download screen.png' })).toBeTruthy()
})
it('keeps an open preview across unchanged snapshots and clears changed references', async () => {
  const fetchAttachment = vi.fn(async () => new Blob(['image'], { type: 'image/png' }))
  const view = render(IssueAttachments, {
    attachments: { [id]: entry },
    baseURL: '/attachments',
    fetchAttachment,
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Preview screen.png' }))
  await waitFor(() => expect(screen.getByAltText('screen.png')).toBeTruthy())
  await view.rerender({
    attachments: structuredClone({ [id]: entry }),
    baseURL: '/attachments',
    fetchAttachment,
  })
  expect(screen.getByAltText('screen.png')).toBeTruthy()
  expect(URL.revokeObjectURL).not.toHaveBeenCalled()
  await view.rerender({
    attachments: {},
    baseURL: '/attachments',
    fetchAttachment,
  })
  expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:preview')
})
