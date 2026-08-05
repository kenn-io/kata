import { isAuthenticationRequiredError } from '../auth/session'
import type { EventFrame } from './sse'

type Refresh = (full: boolean) => Promise<boolean> | boolean

export class InvalidationController {
  readonly #refresh: Refresh
  #pending: ReturnType<typeof setTimeout> | undefined
  #full = false
  #dirty = false
  #running = false
  #stopped = false
  #retryDelay = 1000

  constructor(refresh: Refresh) {
    this.#refresh = refresh
  }

  frame(frame: EventFrame): void {
    this.#full ||= frame.event === 'sync.reset_required'
    this.refresh()
  }

  refresh(): void {
    this.#stopped = false
    this.#dirty = true
    this.#schedule()
  }

  async resume(): Promise<boolean> {
    this.#stopped = false
    const full = this.#full
    let accepted = false
    try {
      accepted = await this.#refresh(full)
    } catch {
      // The visible recovery action can retry without losing a reset latch.
    }
    if (full && accepted) this.#full = false
    return accepted
  }

  #schedule(delay = 0): void {
    if (this.#pending !== undefined || this.#running) return
    this.#pending = setTimeout(() => {
      this.#pending = undefined
      void this.#drain()
    }, delay)
  }

  async #drain(): Promise<void> {
    if (this.#running) return
    this.#running = true
    let retry = false
    try {
      while (this.#dirty) {
        this.#dirty = false
        const full = this.#full
        let accepted = false
        try {
          accepted = await this.#refresh(full)
        } catch {
          // A bounded retry preserves event authority after transient failure.
        }
        if (accepted) this.#retryDelay = 1000
        if (full && accepted) this.#full = false
        if (!accepted) {
          retry = true
          break
        }
      }
    } finally {
      this.#running = false
      if (!this.#stopped) {
        if (this.#dirty) this.#schedule()
        else if (retry) {
          this.#dirty = true
          const delay = this.#retryDelay
          this.#retryDelay = Math.min(this.#retryDelay * 2, 30_000)
          this.#schedule(delay)
        }
      }
    }
  }

  pause(): void {
    if (this.#pending !== undefined) clearTimeout(this.#pending)
    this.#pending = undefined
    this.#dirty = false
    this.#stopped = true
  }

  stop(): void {
    this.pause()
    this.#full = false
  }
}

export class ReconnectBackoff {
  #next = 1000

  afterDisconnect(productive: boolean): number {
    if (productive) {
      this.#next = 1000
      return 1000
    }
    const delay = this.#next
    this.#next = Math.min(this.#next * 2, 30_000)
    return delay
  }
}

interface EventStreamControllerOptions {
  connect: (cursor: number, signal: AbortSignal) => AsyncIterable<EventFrame>
  onFrame: (frame: EventFrame) => void
  onState?: ((state: 'connecting' | 'online' | 'reconnecting' | 'stopped') => void) | undefined
  onAuthenticationRequired?: (() => void) | undefined
  wait?: (delay: number, signal: AbortSignal) => Promise<void>
}

export class EventStreamController {
  readonly #connect: EventStreamControllerOptions['connect']
  readonly #onFrame: EventStreamControllerOptions['onFrame']
  readonly #onState: NonNullable<EventStreamControllerOptions['onState']>
  readonly #wait: NonNullable<EventStreamControllerOptions['wait']>
  readonly #onAuthenticationRequired: NonNullable<
    EventStreamControllerOptions['onAuthenticationRequired']
  >
  #abort: AbortController | undefined
  #cursor = 0

  constructor(options: EventStreamControllerOptions) {
    this.#connect = options.connect
    this.#onFrame = options.onFrame
    this.#onState = options.onState ?? (() => {})
    this.#wait = options.wait ?? waitForReconnect
    this.#onAuthenticationRequired = options.onAuthenticationRequired ?? (() => undefined)
  }

  start(cursor: number): void {
    this.stop()
    this.#cursor = cursor
    const abort = new AbortController()
    this.#abort = abort
    this.#onState('connecting')
    void this.#run(abort.signal)
  }

  stop(): void {
    const wasRunning = this.#abort !== undefined
    this.#abort?.abort()
    this.#abort = undefined
    if (wasRunning) this.#onState('stopped')
  }

  async #run(signal: AbortSignal): Promise<void> {
    const backoff = new ReconnectBackoff()
    while (!signal.aborted) {
      let productive = false
      try {
        for await (const frame of this.#connect(this.#cursor, signal)) {
          if (signal.aborted) return
          if (!productive) this.#onState('online')
          productive = true
          const eventID = Number.parseInt(frame.id, 10)
          if (Number.isSafeInteger(eventID) && eventID >= 0) this.#cursor = eventID
          this.#onFrame(frame)
        }
      } catch (error) {
        if (isAuthenticationRequiredError(error)) {
          this.#onAuthenticationRequired()
          return
        }
        // Reconnect state is intentionally credential- and response-detail-free.
      }
      if (signal.aborted) return
      this.#onState('reconnecting')
      try {
        await this.#wait(backoff.afterDisconnect(productive), signal)
      } catch {
        return
      }
    }
  }
}

interface RefreshSchedulerOptions {
  refresh: () => Promise<void> | void
  openEvents: () => void
  now?: () => Date
  timeZone?: () => string
}

export class RefreshScheduler {
  readonly #refresh: () => Promise<void> | void
  readonly #openEvents: () => void
  readonly #now: () => Date
  readonly #timeZone: () => string
  #pollTimer: ReturnType<typeof setTimeout> | undefined
  #midnightTimer: ReturnType<typeof setTimeout> | undefined
  #updates: 'sse' | 'poll' = 'poll'
  #visible = true
  #lastTimeZone = ''
  #stopped = true

  constructor(options: RefreshSchedulerOptions) {
    this.#refresh = options.refresh
    this.#openEvents = options.openEvents
    this.#now = options.now ?? (() => new Date())
    this.#timeZone = options.timeZone ?? (() => Intl.DateTimeFormat().resolvedOptions().timeZone)
  }

  start(updates: 'sse' | 'poll'): void {
    this.stop()
    this.#stopped = false
    this.#updates = updates
    this.#lastTimeZone = this.#timeZone()
    if (updates === 'sse') this.#openEvents()
    else this.#schedulePoll()
    this.#scheduleMidnight()
  }

  stop(): void {
    this.#stopped = true
    if (this.#pollTimer !== undefined) clearTimeout(this.#pollTimer)
    if (this.#midnightTimer !== undefined) clearTimeout(this.#midnightTimer)
    this.#pollTimer = undefined
    this.#midnightTimer = undefined
  }

  visibilityChanged(visible: boolean): void {
    this.#visible = visible
    if (this.#updates === 'poll') {
      if (this.#pollTimer !== undefined) clearTimeout(this.#pollTimer)
      this.#schedulePoll()
    }
    if (visible) this.#refreshNow()
  }

  focused(): void {
    if (this.#visible) this.#refreshNow()
  }

  environmentChanged(): void {
    const zone = this.#timeZone()
    if (zone === this.#lastTimeZone) return
    this.#lastTimeZone = zone
    this.#refreshNow()
  }

  #schedulePoll(): void {
    if (this.#stopped || this.#updates !== 'poll') return
    this.#pollTimer = setTimeout(
      () => {
        this.#pollTimer = undefined
        if (this.#visible) this.#refreshNow()
        this.#schedulePoll()
      },
      this.#visible ? 15_000 : 60_000,
    )
  }

  #scheduleMidnight(): void {
    if (this.#stopped) return
    const now = this.#now()
    const next = new Date(now)
    next.setHours(24, 0, 0, 0)
    this.#midnightTimer = setTimeout(
      () => {
        this.#midnightTimer = undefined
        if (this.#visible) this.#refreshNow()
        this.#scheduleMidnight()
      },
      Math.max(1, next.getTime() - now.getTime()),
    )
  }

  #refreshNow(): void {
    void Promise.resolve(this.#refresh()).catch(() => undefined)
  }
}

function waitForReconnect(delay: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(resolve, delay)
    signal.addEventListener(
      'abort',
      () => {
        clearTimeout(timer)
        reject(new DOMException('Aborted', 'AbortError'))
      },
      { once: true },
    )
  })
}
