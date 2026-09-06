const assetsSegment = '/assets/'

export function applicationBaseURL(entrypoint: URL, origin: string): URL {
  const marker = entrypoint.pathname.lastIndexOf(assetsSegment)
  if (marker < 0) return new URL('/', origin)
  const prefix = entrypoint.pathname.slice(0, marker)
  return new URL(prefix ? `${prefix}/` : '/', origin)
}

const runtimeBaseURL = applicationBaseURL(new URL(import.meta.url), window.location.origin)

export function currentApplicationBaseURL(): URL {
  return new URL(runtimeBaseURL)
}

export function applicationRoutePath(base = runtimeBaseURL): string {
  return base.pathname === '/' ? '/kata' : base.pathname
}

export function applicationURL(input: string | URL, base = runtimeBaseURL): URL {
  const target = new URL(String(input), base.origin)
  if (target.origin !== base.origin || base.pathname === '/') return target
  if (target.pathname === base.pathname.slice(0, -1) || target.pathname.startsWith(base.pathname)) {
    return target
  }
  if (target.pathname === '/api' || target.pathname.startsWith('/api/')) {
    target.pathname = `${base.pathname}${target.pathname.slice(1)}`
  }
  return target
}

export function applicationRequest(
  input: RequestInfo | URL,
  base = runtimeBaseURL,
): RequestInfo | URL {
  if (base.pathname === '/') return input
  if (input instanceof Request) {
    const target = applicationURL(new URL(input.url), base)
    return target.href === input.url ? input : new Request(target, input)
  }
  return applicationURL(input instanceof URL ? input : String(input), base)
}
