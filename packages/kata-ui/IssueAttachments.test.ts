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
  expect(screen.getByRole('button', { name: 'Preview screen.png' }).textContent?.trim()).toBe(
    'Preview',
  )
  expect(screen.getByRole('button', { name: 'Download screen.png' }).textContent?.trim()).toBe(
    'Download',
  )
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

it('revokes an open preview when only the attachment origin changes', async () => {
  vi.mocked(URL.createObjectURL).mockReturnValueOnce('blob:old-origin')
  const { rerender } = render(IssueAttachments, {
    attachmentsEnabled: true,
    attachments: { [id]: entry },
    baseURL: '/attachments',
    fetchAttachment: async () => new Blob(['old origin'], { type: 'image/png' }),
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Preview screen.png' }))
  await waitFor(() => expect(screen.getByAltText('screen.png').getAttribute('src')).toBe('blob:old-origin'))

  await rerender({
    attachments: { [id]: { ...entry, origin: '01ARZ3NDEKTSV4RRFFQ69G5FAV' } },
  })

  expect(screen.queryByAltText('screen.png')).toBeNull()
  expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:old-origin')
  expect(screen.getByRole('button', { name: 'Preview screen.png' })).toBeTruthy()
})

it('ignores an in-flight preview response after the attachment origin changes', async () => {
  let finishOld!: (blob: Blob) => void
  let settleOld!: () => void
  const oldSettled = new Promise<void>((resolve) => {
    settleOld = resolve
  })
  const fetchAttachment = vi
    .fn()
    .mockImplementationOnce(
      () =>
        new Promise<Blob>((resolve) => {
          finishOld = (blob) => {
            resolve(blob)
            settleOld()
          }
        }),
    )
    .mockResolvedValueOnce(new Blob(['new origin'], { type: 'image/png' }))
  vi.mocked(URL.createObjectURL).mockReturnValueOnce('blob:new-origin')
  const { rerender } = render(IssueAttachments, {
    attachmentsEnabled: true,
    attachments: { [id]: entry },
    baseURL: '/attachments',
    fetchAttachment,
  })
  await fireEvent.click(screen.getByRole('button', { name: 'Preview screen.png' }))
  await waitFor(() => expect(fetchAttachment).toHaveBeenCalledTimes(1))

  await rerender({
    attachments: { [id]: { ...entry, origin: '01ARZ3NDEKTSV4RRFFQ69G5FAV' } },
  })
  finishOld(new Blob(['old origin'], { type: 'image/png' }))
  await oldSettled
  await Promise.resolve()

  expect(URL.createObjectURL).not.toHaveBeenCalled()
  expect(screen.queryByAltText('screen.png')).toBeNull()
  const preview = screen.getByRole('button', { name: 'Preview screen.png' })
  expect((preview as HTMLButtonElement).disabled).toBe(false)
  await fireEvent.click(preview)
  await waitFor(() => expect(screen.getByAltText('screen.png').getAttribute('src')).toBe('blob:new-origin'))
  expect(fetchAttachment).toHaveBeenCalledTimes(2)
})

it.each([
  { action: 'Preview', name: 'screen.png', media_type: 'image/png' },
  { action: 'Play', name: 'clip.mp4', media_type: 'video/mp4' },
  { action: 'Download', name: 'screen.png', media_type: 'image/png' },
])(
  'allows $action with read authority while writes are disabled',
  async ({ action, name, media_type }) => {
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const fetchAttachment = vi.fn(async () => new Blob(['file']))
    render(IssueAttachments, {
      attachmentsEnabled: true,
      attachments: { [id]: { ...entry, name, media_type } },
      baseURL: '/attachments',
      actionsDisabled: true,
      fetchAttachment,
    })
    const button = screen.getByRole('button', { name: `${action} ${name}` }) as HTMLButtonElement
    expect(button.disabled).toBe(false)
    await fireEvent.click(button)
    await waitFor(() => expect(fetchAttachment).toHaveBeenCalledWith(`/attachments/${id}/content`))
  },
)
it('caps image previews at 20 MiB and shows the CLI hint above 100 MiB', () => {
  render(IssueAttachments, {
    attachmentsEnabled: true,
    attachments: {
      [id.slice(0, -1) + 'B']: { ...entry, size: 21 << 20 },
      [id.slice(0, -1) + 'C']: { ...entry, name: 'large.png', size: 101 << 20 },
    },
    baseURL: '/attachments',
    fetchAttachment: vi.fn(),
  })
  expect(screen.queryByRole('button', { name: /Preview/ })).toBeNull()
  expect(screen.getByText(/kata docbank attachments get/)).toBeTruthy()
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
it('uploads the selected batch through a refresh and shows per-file progress and errors', async () => {
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
  const view = render(IssueAttachments, {
    attachmentsEnabled: true,
    baseURL: '/attachments',
    uploadAttachment,
  })
  const files = [new File(['image'], 'shot.png'), new File(['video'], 'clip.mp4')]
  const input = view.container.querySelector('input[type="file"]')!
  await fireEvent.change(input, { target: { files } })
  await waitFor(() => expect(screen.getByText('shot.png uploading')).toBeTruthy())
  expect(screen.getByText('clip.mp4 queued')).toBeTruthy()
  expect(screen.getByRole('button', { name: 'Attach files' }).hasAttribute('disabled')).toBe(true)
  await view.rerender({
    attachments: { [id]: { ...entry, name: 'shot.png', size: files[0]!.size } },
  })
  expect(screen.getByText('shot.png')).toBeTruthy()
  expect(screen.getByText('shot.png uploading')).toBeTruthy()
  expect(screen.getByText('clip.mp4 queued')).toBeTruthy()
  finish()
  await waitFor(() => expect(uploadAttachment).toHaveBeenCalledTimes(2))
  expect(uploadAttachment).toHaveBeenNthCalledWith(1, files[0])
  expect(uploadAttachment).toHaveBeenNthCalledWith(2, files[1])
  await waitFor(() =>
    expect(screen.getByRole('status').textContent).toBe('clip.mp4 attachment_too_large'),
  )
  expect(screen.getByRole('button', { name: 'Attach files' }).hasAttribute('disabled')).toBe(false)
})

it('plays a video with a typed blob and revokes it when context changes', async () => {
  const media_type = 'video/quicktime'
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
  expect(screen.getByRole('button', { name: 'Open video in expanded view' })).toBeTruthy()
  expect(video.preload).toBe('metadata')
  const blob = vi.mocked(URL.createObjectURL).mock.calls[0]![0] as Blob
  expect(blob.type).toBe(media_type)
  await rerender({ baseURL: '/other-issue/attachments' })
  expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:preview')
  expect(container.querySelector('video')).toBeNull()
})
it('keeps file details without actions when Docbank is unavailable', () => {
  render(IssueAttachments, {
    attachments: { [id]: entry },
    baseURL: '/attachments',
    attachmentsEnabled: false,
    fetchAttachment: vi.fn(),
    uploadAttachment: vi.fn(),
  })
  expect(screen.getByText('screen.png')).toBeTruthy()
  expect(screen.getByText('image/png · 12 bytes')).toBeTruthy()
  expect(screen.queryByRole('button')).toBeNull()
})

it('keeps an open video when another attachment changes and revokes removed or replaced references', async () => {
  vi.mocked(URL.createObjectURL)
    .mockReturnValueOnce('blob:video')
    .mockReturnValueOnce('blob:download')
    .mockReturnValueOnce('blob:image')
  const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
  let finishDownload!: (blob: Blob) => void
  const fetchAttachment = vi
    .fn()
    .mockResolvedValueOnce(new Blob(['video']))
    .mockImplementationOnce(
      () =>
        new Promise<Blob>((resolve) => {
          finishDownload = resolve
        }),
    )
    .mockResolvedValueOnce(new Blob(['image']))
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
  await fireEvent.click(screen.getByRole('button', { name: 'Download clip.mp4' }))
  await waitFor(() => expect(fetchAttachment).toHaveBeenCalledTimes(2))
  await view.rerender({ attachments: { [id]: { ...videoEntry } } })
  expect(view.container.querySelector('video')).toBe(video)
  expect(URL.revokeObjectURL).not.toHaveBeenCalled()
  finishDownload(new Blob(['download']))
  await waitFor(() => expect(click).toHaveBeenCalledOnce())
  expect(click.mock.instances[0]).toHaveProperty('download', 'clip.mp4')
  expect(click.mock.instances[0]).toHaveProperty('href', 'blob:download')
  await waitFor(() => expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:download'))
  vi.mocked(URL.revokeObjectURL).mockClear()
  const reordered = { ...videoEntry }
  await view.rerender({ attachments: { [id]: reordered, [otherID]: entry } })
  expect(view.container.querySelector('video')).toBe(video)
  expect(URL.revokeObjectURL).not.toHaveBeenCalled()
  await fireEvent.click(screen.getByRole('button', { name: 'Preview screen.png' }))
  await waitFor(() => expect(screen.getByAltText('screen.png')).toBeTruthy())
  await view.rerender({
    attachments: { [id]: videoEntry },
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

it('gives each reference a stable history anchor', () => {
  const { container } = render(IssueAttachments, { attachments: { [id]: entry }, baseURL: '/attachments' })
  expect(container.querySelector(`[id="kata-attachment-${id}"]`)?.textContent).toContain('screen.png')
})
