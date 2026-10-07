export function startSessionEndedReporting(
  post: (duration: string) => Promise<unknown>,
): () => void {
  let started = document.hidden ? undefined : performance.now()
  const end = () => {
    if (started === undefined) return
    const elapsed = performance.now() - started
    started = undefined
    const duration =
      elapsed < 60_000
        ? 'under_1m'
        : elapsed < 300_000
          ? '1_to_5m'
          : elapsed <= 1_800_000
            ? '5_to_30m'
            : 'over_30m'
    void post(duration).catch(() => undefined)
  }
  const resume = () => {
    if (!document.hidden && started === undefined) started = performance.now()
  }
  const visibility = () => (document.hidden ? end() : resume())
  document.addEventListener('visibilitychange', visibility)
  window.addEventListener('pagehide', end)
  window.addEventListener('pageshow', resume)
  return () => {
    started = undefined
    document.removeEventListener('visibilitychange', visibility)
    window.removeEventListener('pagehide', end)
    window.removeEventListener('pageshow', resume)
  }
}
