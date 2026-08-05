const preloadRecoveryStorageKey = 'kata.preload-recovery.v1'

interface StorageLike {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
  removeItem(key: string): void
}

interface PreloadRecoveryOptions {
  target: EventTarget
  storage: StorageLike
  entrypoint: string
  reload: () => void
  showMismatch: () => void
}

export function retirePreloadRecovery(storage: StorageLike, entrypoint: string): void {
  const source = storage.getItem(preloadRecoveryStorageKey)
  if (source && source !== entrypoint) storage.removeItem(preloadRecoveryStorageKey)
}

export function installPreloadRecovery({
  target,
  storage,
  entrypoint,
  reload,
  showMismatch,
}: PreloadRecoveryOptions): () => void {
  retirePreloadRecovery(storage, entrypoint)
  const recover = (event: Event): void => {
    event.preventDefault()
    if (storage.getItem(preloadRecoveryStorageKey) === entrypoint) {
      showMismatch()
      return
    }
    storage.setItem(preloadRecoveryStorageKey, entrypoint)
    reload()
  }
  target.addEventListener('vite:preloadError', recover)
  return () => target.removeEventListener('vite:preloadError', recover)
}
