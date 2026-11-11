import type { KataAttachment } from '@kenn-io/kata-ui'
import type { UICapabilities } from '../api/generated'

export function attachmentWritable(capabilities: UICapabilities, projectUID: string): boolean {
  const route = capabilities.attachment_routes?.find((item) => item.project_uid === projectUID)
  return route ? route.write : Boolean(capabilities.attachment_local)
}

export function attachmentAvailable(
  capabilities: UICapabilities,
  projectUID: string,
  attachment: KataAttachment,
): boolean {
  if (!attachment.origin || attachment.origin === capabilities.attachment_instance_uid) {
    return Boolean(capabilities.attachment_local)
  }
  const route = capabilities.attachment_routes?.find((item) => item.project_uid === projectUID)
  return Boolean(route?.read && (!route.origin || route.origin === attachment.origin))
}
