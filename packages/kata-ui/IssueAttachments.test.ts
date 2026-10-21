// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import IssueAttachments from './src/components/IssueAttachments.svelte'

const id = '01JB3Z6V2K8M4Q9T1X5W7Y0RNA'
const entry = {
  name: 'screen.png',
  media_type: 'image/png',
  size: 12,
  docbank_version: '6f1c2e8a-2222-4222-8222-222222222222',
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
    attachmentsEnabled: true,
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
    attachmentsEnabled: true,
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
it('offers video playback and downloads while large files show the CLI hint', () => {
  render(IssueAttachments, {
    attachmentsEnabled: true,
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
  expect(screen.getByRole('button', { name: 'Play recording.mp4' })).toBeTruthy()
  expect(screen.getByText(/kata docbank attachments get/)).toBeTruthy()
  expect(screen.getByText(/video\/mp4/)).toBeTruthy()
})
it('shows an unavailable state while preserving name, type and size', async () => {
  render(IssueAttachments, {
    attachmentsEnabled: true,
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
it('uploads each selected file and shows per-file progress and errors', async () => {
  let finish!: () => void
  const uploadAttachment = vi
    .fn()
    .mockImplementationOnce(
      () =>
        new Promise<void>((resolve) => {
          finish = resolve
        }),
    )
    .mockRejectedValueOnce(new Error('attachment_too_large'))
  const { container } = render(IssueAttachments, {
    attachmentsEnabled: true,
    attachments: {},
    baseURL: '/attachments',
    uploadAttachment,
  })
  const files = [new File(['image'], 'shot.png'), new File(['video'], 'clip.mp4')]
  const input = container.querySelector('input[type="file"]')!
  await fireEvent.change(input, { target: { files } })
  await waitFor(() => expect(screen.getByText('shot.png uploading')).toBeTruthy())
  expect(screen.getByText('clip.mp4 queued')).toBeTruthy()
  expect(screen.getByRole('button', { name: 'Attach files' }).hasAttribute('disabled')).toBe(true)
  finish()
  await waitFor(() => expect(uploadAttachment).toHaveBeenCalledTimes(2))
  expect(uploadAttachment).toHaveBeenNthCalledWith(1, files[0])
  expect(uploadAttachment).toHaveBeenNthCalledWith(2, files[1])
  await waitFor(() =>
    expect(screen.getByRole('status').textContent).toBe('clip.mp4 attachment_too_large'),
  )
  expect(screen.getByRole('button', { name: 'Attach files' }).hasAttribute('disabled')).toBe(false)
})

it('uploads the whole selected batch while a refresh disables controls', async () => {
  let finish!: () => void
  const uploadAttachment = vi
    .fn()
    .mockImplementationOnce(
      () =>
        new Promise<void>((resolve) => {
          finish = resolve
        }),
    )
    .mockResolvedValue(undefined)
  const view = render(IssueAttachments, {
    attachmentsEnabled: true,
    baseURL: '/attachments',
    uploadAttachment,
  })
  const files = [new File(['image'], 'shot.png'), new File(['video'], 'clip.mp4')]
  await fireEvent.change(view.container.querySelector('input[type="file"]')!, {
    target: { files },
  })
  await waitFor(() => expect(uploadAttachment).toHaveBeenCalledTimes(1))
  await view.rerender({
    actionsDisabled: true,
  })
  finish()
  await waitFor(() => expect(screen.queryByText('shot.png uploading')).toBeNull())
  await waitFor(() => expect(uploadAttachment).toHaveBeenCalledTimes(2))
  expect(uploadAttachment).toHaveBeenNthCalledWith(2, files[1])
})

it('hides uploads without Docbank capability and disables them with stale or read-only authority', async () => {
  const view = render(IssueAttachments, {
    baseURL: '/attachments',
    uploadAttachment: vi.fn(),
    attachmentsEnabled: false,
  })
  expect(screen.queryByRole('button', { name: 'Attach files' })).toBeNull()
  await view.rerender({ attachmentsEnabled: true, actionsDisabled: true })
  expect(screen.getByRole('button', { name: 'Attach files' }).hasAttribute('disabled')).toBe(true)
})

it.each(['video/mp4', 'video/x-m4v', 'video/webm', 'video/ogg', 'video/quicktime'])(
  'plays %s with a typed blob and revokes it when context changes',
  async (media_type) => {
    const fetchAttachment = vi.fn(
      async () => new Blob(['video'], { type: 'application/octet-stream' }),
    )
    const { container, rerender } = render(IssueAttachments, {
      attachmentsEnabled: true,
      attachments: {
        [id]: { ...entry, name: 'clip', media_type, size: 100 << 20 },
      },
      baseURL: '/attachments',
      fetchAttachment,
    })
    await fireEvent.click(screen.getByRole('button', { name: 'Play clip' }))
    await waitFor(() =>
      expect(container.querySelector('video')?.getAttribute('src')).toBe('blob:preview'),
    )
    const video = container.querySelector('video')!
    expect(video.controls).toBe(true)
    expect(video.preload).toBe('metadata')
    const blob = vi.mocked(URL.createObjectURL).mock.calls[0]![0] as Blob
    expect(blob.type).toBe(media_type)
    await rerender({ baseURL: '/other-issue/attachments' })
    expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:preview')
    expect(container.querySelector('video')).toBeNull()
  },
)
it('keeps file details without actions when Docbank is unavailable', () => {
  render(IssueAttachments, {
    attachments: { [id]: entry },
    baseURL: '/attachments',
    attachmentsEnabled: false,
    fetchAttachment: vi.fn(),
  })
  expect(screen.getByText('screen.png')).toBeTruthy()
  expect(screen.getByText('image/png · 12 bytes')).toBeTruthy()
  expect(screen.queryByRole('button')).toBeNull()
})

it('renders nothing for an empty map', () => {
  const { container } = render(IssueAttachments, {
    attachmentsEnabled: true,
    attachments: {},
    baseURL: '/attachments',
  })
  expect(container.textContent).toBe('')
})

it('allows a normal 1 MiB image to preview and download', () => {
  render(IssueAttachments, {
    attachmentsEnabled: true,
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
    attachmentsEnabled: true,
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

it('keeps an open video when another attachment changes and revokes only replaced references', async () => {
  vi.mocked(URL.createObjectURL).mockReturnValueOnce('blob:video').mockReturnValueOnce('blob:image')
  const fetchAttachment = vi.fn(async () => new Blob(['video']))
  const videoEntry = { ...entry, name: 'clip.mp4', media_type: 'video/mp4' }
  const otherID = id.slice(0, -1) + 'B'
  const view = render(IssueAttachments, {
    attachmentsEnabled: true,
    attachments: { [id]: videoEntry },
    baseURL: '/attachments',
    fetchAttachment,
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Play clip.mp4' }))
  await waitFor(() => expect(view.container.querySelector('video')).toBeTruthy())
  const video = view.container.querySelector('video')!
  const reordered = { ...videoEntry }
  await view.rerender({ attachments: { [id]: reordered, [otherID]: entry } })
  expect(view.container.querySelector('video')).toBe(video)
  expect(URL.revokeObjectURL).not.toHaveBeenCalled()
  await fireEvent.click(screen.getByRole('button', { name: 'Preview screen.png' }))
  await waitFor(() => expect(screen.getByAltText('screen.png')).toBeTruthy())
  await view.rerender({
    attachments: { [id]: videoEntry, [otherID]: { ...entry, size: 13 } },
  })
  expect(view.container.querySelector('video')).toBe(video)
  expect(URL.revokeObjectURL).toHaveBeenCalledTimes(1)
  expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:image')
  expect(URL.revokeObjectURL).not.toHaveBeenCalledWith('blob:video')
  expect(screen.queryByAltText('screen.png')).toBeNull()
  await view.rerender({ attachments: { [id]: { ...videoEntry, size: 13 } } })
  expect(view.container.querySelector('video')).toBeNull()
  expect(URL.revokeObjectURL).toHaveBeenCalledTimes(2)
  expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:video')
})

it('offers downloads for types outside the inline list', () => {
  render(IssueAttachments, {
    attachmentsEnabled: true,
    attachments: {
      [id]: { ...entry, media_type: 'image/png ; charset=utf-8' },
    },
    baseURL: '/attachments',
    fetchAttachment: async () => new Blob(['image']),
  })
  expect(screen.queryByRole('button', { name: 'Preview screen.png' })).toBeNull()
  expect(screen.getByRole('button', { name: 'Download screen.png' })).toBeTruthy()
})
