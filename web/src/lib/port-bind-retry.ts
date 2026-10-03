const maxPortBindAttempts = 4
const addressInUsePattern =
  /(?:EADDRINUSE|address already in use|only one usage of each socket address)/i

export async function retryPortBind<T>(
  allocatePort: () => Promise<number>,
  start: (port: number) => Promise<T>,
): Promise<T> {
  for (let attempt = 0; attempt < maxPortBindAttempts; attempt += 1) {
    const port = await allocatePort()
    try {
      return await start(port)
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error)
      if (!addressInUsePattern.test(message) || attempt === maxPortBindAttempts - 1) throw error
    }
  }

  throw new Error('port bind retries exhausted')
}
