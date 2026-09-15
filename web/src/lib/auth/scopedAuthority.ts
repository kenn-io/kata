interface ScopedCapabilityEnvelope {
  scope?: unknown
  expires_at?: unknown
  allowed_actions?: unknown
  close_requires_evidence?: unknown
}

const canonicalUID = /^[0-9A-HJKMNP-TV-Z]{26}$/

export function assertValidIssueScopedCapabilities(
  capabilities: ScopedCapabilityEnvelope,
  message: string,
): void {
  if (capabilities.scope === undefined) return
  const scope = capabilities.scope
  const expiresAt =
    typeof capabilities.expires_at === 'string' ? Date.parse(capabilities.expires_at) : NaN
  if (
    typeof scope !== 'object' ||
    scope === null ||
    !('kind' in scope) ||
    scope.kind !== 'issue_subtree' ||
    !('project_uid' in scope) ||
    typeof scope.project_uid !== 'string' ||
    !canonicalUID.test(scope.project_uid) ||
    !('root_issue_uid' in scope) ||
    typeof scope.root_issue_uid !== 'string' ||
    !canonicalUID.test(scope.root_issue_uid) ||
    !Number.isFinite(expiresAt) ||
    expiresAt <= Date.now() ||
    !Array.isArray(capabilities.allowed_actions) ||
    !capabilities.allowed_actions.includes('issue.read') ||
    capabilities.close_requires_evidence !== true
  ) {
    throw new Error(message)
  }
}
