export function startSessionEndedReporting(
  post: (duration: string) => Promise<unknown>,
  quiet = false,
) {
  let stopped = false
  let started = quiet || document.hidden ? undefined : performance.now()
  let elapsed = 0
  let hasInterval = started !== undefined
  let ended = false
  let authenticated = true
  let hiddenAt: number | undefined
  let timer: ReturnType<typeof setTimeout> | undefined
  const pause = () => {
    if (started === undefined) return
    elapsed += performance.now() - started
    started = undefined
  }
  const flush = () => {
    if (quiet || !authenticated || !ended || !hasInterval) return
    const duration =
      elapsed < 60_000
        ? 'under_1m'
        : elapsed < 300_000
          ? '1_to_5m'
          : elapsed <= 1_800_000
            ? '5_to_30m'
            : 'over_30m'
    elapsed = 0
    hasInterval = false
    ended = false
    void post(duration).catch(() => undefined)
  }
  const end = () => {
    clearTimeout(timer)
    hiddenAt = undefined
    pause()
    if (!hasInterval) return
    ended = true
    flush()
  }
  const resume = () => {
    if (!quiet && authenticated && !document.hidden && !ended && started === undefined) {
      hasInterval = true
      started = performance.now()
    }
  }
  const visibility = () => {
    if (document.hidden) {
      pause()
      // Wall-clock time, since performance.now() can pause while the machine sleeps.
      hiddenAt = Date.now()
      timer = setTimeout(end, 1_800_000)
    } else {
      clearTimeout(timer)
      if (hiddenAt !== undefined && Date.now() - hiddenAt >= 1_800_000) end()
      hiddenAt = undefined
      resume()
    }
  }
  const listen = (enabled: boolean) => {
    if (enabled) {
      document.addEventListener('visibilitychange', visibility)
      window.addEventListener('pagehide', end)
      window.addEventListener('pageshow', resume)
    } else {
      document.removeEventListener('visibilitychange', visibility)
      window.removeEventListener('pagehide', end)
      window.removeEventListener('pageshow', resume)
    }
  }
  listen(!quiet)
  return {
    setQuiet: (value: boolean) => {
      if (stopped || quiet === value) return
      quiet = value
      listen(!quiet)
      if (quiet) {
        clearTimeout(timer)
        pause()
        hiddenAt = undefined
      } else {
        flush()
        resume()
      }
    },
    pause: () => {
      authenticated = false
      pause()
    },
    resume: () => {
      authenticated = true
      flush()
      resume()
    },
    stop: () => {
      stopped = true
      clearTimeout(timer)
      started = undefined
      listen(false)
    },
  }
}
