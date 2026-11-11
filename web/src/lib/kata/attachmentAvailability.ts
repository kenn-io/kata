import type { KataAttachment } from '@kenn-io/kata-ui'
import type { UICapabilities, ReadUIAttachmentRoute200Route } from '../api/generated'

export function attachmentWritable(
  capabilities: UICapabilities,
  projectUID: string,
  route: ReadUIAttachmentRoute200Route | undefined,
): boolean {
  if (route === undefined) return false
  if (route && route.project_uid !== projectUID) return false
  return route ? route.write : Boolean(capabilities.attachments)
}

export function attachmentAvailable(
  capabilities: UICapabilities,
  projectUID: string,
  route: ReadUIAttachmentRoute200Route | undefined,
  attachment: KataAttachment,
): boolean {
  if (!attachment.origin || capabilities.attachment_local_origins?.includes(attachment.origin)) {
    return Boolean(capabilities.attachments)
  }
  return Boolean(
    route?.project_uid === projectUID &&
    route.read &&
    (!route.origin || route.origin === attachment.origin),
  )
}
