import { getProjectFederationStatus, type FederationProjectStatus } from './api/generated'

export interface ProjectSyncRead {
  status: FederationProjectStatus | undefined
  stale: boolean
}

const isTransientStatus = (status: number) => status >= 500 || status === 408 || status === 429

// A temporary server failure keeps the last summary of this same project,
// marked stale. Every other failure throws so the caller drops it.
export async function readProjectSync(
  projectId: number,
  uid: string,
  signal: AbortSignal,
  previous: ProjectSyncRead | undefined,
  clearPrevious: () => void,
): Promise<ProjectSyncRead> {
  let response: Awaited<ReturnType<typeof getProjectFederationStatus>>
  let responseStatus: number | undefined
  try {
    response = await getProjectFederationStatus(
      { projectId },
      { compact: true },
      {
        signal,
        onResponse: (raw) => {
          responseStatus = raw.status
          if (raw.status >= 400 && raw.status < 500 && !isTransientStatus(raw.status)) {
            previous = undefined
            clearPrevious()
          }
        },
      },
    )
  } catch (error) {
    if (
      previous &&
      !signal.aborted &&
      ((responseStatus === undefined && error instanceof TypeError) ||
        (responseStatus !== undefined && isTransientStatus(responseStatus)))
    )
      return { status: previous.status, stale: true }
    throw error
  }
  if (previous && isTransientStatus(response.status))
    return { status: previous.status, stale: true }
  if (response.status !== 200) throw new Error()
  const found = response.data.statuses.find(
    (entry) => entry.project_id === projectId && entry.project_uid === uid,
  )
  if (response.data.statuses.length && !found) throw new Error()
  return { status: found, stale: false }
}
