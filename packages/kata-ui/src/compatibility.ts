export function supportsKataAPISchema(version: string): boolean {
  const match = /^(\d+)\.(\d+)\.(\d+)$/.exec(version.trim())
  if (!match) return false

  const major = Number(match[1])
  const minor = Number(match[2])
  return major === 0 && (minor === 9 || minor === 10)
}
