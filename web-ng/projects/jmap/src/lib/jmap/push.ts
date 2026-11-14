// Push: one event source (RFC 8620 7.3) that tells the page when something
// changed on the server, so it does not have to ask on a timer.
//
// The browser's EventSource reconnects by itself after a network error, at
// a pace of its own, and gives up for good after an HTTP error. Here every
// error closes the connection and a new one is made after a pause that
// doubles from one second to a minute, with some spread so that the tabs of
// a restarted server do not all come back in the same moment.
//
// While the connection is not up, `connected` is false and the page falls
// back to its timer. Whenever it comes up, onChange is called once: the
// page may have missed changes while it was away.

export interface EventSourceLike {
  addEventListener(type: string, listener: (e: { data?: string }) => void): void
  close(): void
}

export interface PushOptions {
  // The URL to connect to, or null while there is no session to take it
  // from: then the connection is tried again later.
  url: () => string | null
  // Something changed (or may have): fetch the changes.
  onChange: () => void
  // The connection came up or went away.
  onStatus?: (connected: boolean) => void
  // For tests.
  connect?: (url: string) => EventSourceLike
  random?: () => number
}

// The server sends a ping this often; nothing at all for two and a half
// times as long means the connection is dead without having said so.
export const PING_SECONDS = 30
const SILENCE_MS = PING_SECONDS * 2500
export const FIRST_RETRY_MS = 1000
export const MAX_RETRY_MS = 60_000

// retryDelay is the pause before attempt number `failures` (1 for the
// first retry): 1 s, 2 s, 4 s ... up to a minute, each between 75% and
// 100% of that.
export function retryDelay(failures: number, random: () => number = Math.random): number {
  const full = Math.min(MAX_RETRY_MS, FIRST_RETRY_MS * 2 ** Math.max(0, failures - 1))
  return Math.round(full * (0.75 + 0.25 * random()))
}

// eventSourcePath fills the session's eventSourceUrl template for all
// types, a connection that stays open, and pings.
export function eventSourcePath(template: string): string {
  return template
    .replace(/^[a-z][a-z0-9+.-]*:\/\/[^/]+/i, '')
    .replace('{types}', '*')
    .replace('{closeafter}', 'no')
    .replace('{ping}', String(PING_SECONDS))
}

export class Push {
  connected = false
  private source: EventSourceLike | null = null
  private retry: ReturnType<typeof setTimeout> | undefined
  private watchdog: ReturnType<typeof setTimeout> | undefined
  private failures = 0
  private running = false
  private connect: (url: string) => EventSourceLike
  private random: () => number

  constructor(private opts: PushOptions) {
    this.connect = opts.connect ?? ((url) => new EventSource(url, { withCredentials: true }))
    this.random = opts.random ?? Math.random
  }

  start(): void {
    if (this.running) return
    this.running = true
    this.failures = 0
    this.open()
  }

  stop(): void {
    this.running = false
    clearTimeout(this.retry)
    clearTimeout(this.watchdog)
    this.source?.close()
    this.source = null
    this.setConnected(false)
  }

  private setConnected(v: boolean): void {
    if (this.connected === v) return
    this.connected = v
    this.opts.onStatus?.(v)
  }

  private open(): void {
    if (!this.running) return
    const url = this.opts.url()
    if (!url) {
      // No session yet. Not a failure of the connection: look again soon,
      // without lengthening the pauses.
      clearTimeout(this.retry)
      this.retry = setTimeout(() => this.open(), FIRST_RETRY_MS)
      return
    }
    let source: EventSourceLike
    try {
      source = this.connect(url)
    } catch {
      this.fail()
      return
    }
    this.source = source
    // Events of a connection that was replaced are dropped.
    const mine = (fn: () => void) => () => {
      if (this.source === source) fn()
    }
    source.addEventListener('open', mine(() => {
      this.failures = 0
      this.alive()
      this.setConnected(true)
      this.opts.onChange()
    }))
    source.addEventListener('state', mine(() => {
      this.alive()
      this.opts.onChange()
    }))
    source.addEventListener('ping', mine(() => this.alive()))
    source.addEventListener('error', mine(() => this.fail()))
    this.alive()
  }

  // alive restarts the wait for the next sign of life.
  private alive(): void {
    clearTimeout(this.watchdog)
    this.watchdog = setTimeout(() => this.fail(), SILENCE_MS)
  }

  private fail(): void {
    clearTimeout(this.watchdog)
    this.source?.close()
    this.source = null
    this.setConnected(false)
    if (!this.running) return
    this.failures++
    clearTimeout(this.retry)
    this.retry = setTimeout(() => this.open(), retryDelay(this.failures, this.random))
  }
}
