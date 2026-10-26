import type { KataTaskEvent } from '../kata/types'

interface Attachment {
  name: string
  media_type: string
  size: number
  docbank_version: string
}
function attachment(value: unknown): Attachment | undefined {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return undefined
  const raw = value as Record<string, unknown>
  if (
    typeof raw.name !== 'string' ||
    raw.name.length === 0 ||
    new TextEncoder().encode(raw.name).length > 255 ||
    raw.name === '.' ||
    raw.name === '..' ||
    /[/\\\p{Cc}]/u.test(raw.name) ||
    typeof raw.media_type !== 'string' ||
    raw.media_type.length === 0 ||
    typeof raw.size !== 'number' ||
    !Number.isFinite(raw.size) ||
    raw.size < 0 ||
    !Number.isInteger(raw.size) ||
    typeof raw.docbank_version !== 'string' ||
    !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(raw.docbank_version) ||
    raw.docbank_version === '00000000-0000-0000-0000-000000000000'
  )
    return undefined
  return raw as unknown as Attachment
}

export function closingAttachments(
  event: KataTaskEvent,
  metadata: Record<string, unknown>,
): { label: string; href?: string }[] {
  if (event.type !== 'issue.closed' || !Array.isArray(event.payload?.evidence)) return []
  return event.payload.evidence.flatMap((value: unknown) => {
    if (!value || typeof value !== 'object' || Array.isArray(value)) return []
    const evidence = value as Record<string, unknown>
    if (
      evidence.type !== 'attachment' ||
      typeof evidence.attachment_id !== 'string' ||
      !evidence.attachment_id
    )
      return []
    const id = evidence.attachment_id
    const current = attachment(metadata[`attachment.${id}`])
    const hasSnapshot = Object.hasOwn(evidence, 'attachment')
    const frozen = hasSnapshot ? attachment(evidence.attachment) : current
    if (!frozen)
      return [{ label: `attachment ${id} (${hasSnapshot ? 'invalid snapshot' : 'removed'})` }]
    const matches =
      current && current.docbank_version.toLowerCase() === frozen.docbank_version.toLowerCase()
    const state = matches ? '' : current ? ' (changed)' : ' (removed)'
    const label = `${frozen.name} · ${frozen.media_type} · ${frozen.size} bytes${state}`
    return [{ label, ...(matches ? { href: `#kata-attachment-${encodeURIComponent(id)}` } : {}) }]
  })
}
