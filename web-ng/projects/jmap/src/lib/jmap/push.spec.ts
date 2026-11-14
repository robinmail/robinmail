import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { eventSourcePath, FIRST_RETRY_MS, MAX_RETRY_MS, PING_SECONDS, Push, retryDelay, type EventSourceLike } from './push'

// A stand-in for the browser's EventSource: the test decides what it says.
class FakeSource implements EventSourceLike {
  closed = false
  private listeners: Record<string, Array<(e: { data?: string }) => void>> = {}
  constructor(public url: string) {}
  addEventListener(type: string, listener: (e: { data?: string }) => void): void {
    ;(this.listeners[type] ??= []).push(listener)
  }
  close(): void {
    this.closed = true
  }
  emit(type: string, data?: string): void {
    for (const l of this.listeners[type] ?? []) l({ data })
  }
}

function setup(url: () => string | null = () => '/jmap/eventsource/?types=*') {
  const sources: FakeSource[] = []
  const changes: number[] = []
  const status: boolean[] = []
  const push = new Push({
    url,
    onChange: () => changes.push(Date.now()),
    onStatus: (c) => status.push(c),
    connect: (u) => {
      const s = new FakeSource(u)
      sources.push(s)
      return s
    },
    random: () => 1, // the full pause, no spread
  })
  return { push, sources, changes, status, last: () => sources[sources.length - 1]! }
}

beforeEach(() => vi.useFakeTimers())
afterEach(() => vi.useRealTimers())

describe('eventSourcePath', () => {
  it('fills the template of the session (RFC 8620 7.3) and drops the origin', () => {
    expect(eventSourcePath('https://mail.example/jmap/eventsource/?types={types}&closeafter={closeafter}&ping={ping}')).toBe(
      `/jmap/eventsource/?types=*&closeafter=no&ping=${PING_SECONDS}`,
    )
  })
})

describe('retryDelay', () => {
  it('doubles from one second to a minute', () => {
    const full = () => 1
    expect([1, 2, 3, 4, 5, 6, 7, 8, 20].map((n) => retryDelay(n, full))).toEqual([1000, 2000, 4000, 8000, 16000, 32000, 60000, 60000, 60000])
  })
  it('spreads each pause between 75% and 100%', () => {
    expect(retryDelay(3, () => 0)).toBe(3000)
    expect(retryDelay(3, () => 0.5)).toBe(3500)
    for (let i = 0; i < 50; i++) {
      const d = retryDelay(4)
      expect(d).toBeGreaterThanOrEqual(6000)
      expect(d).toBeLessThanOrEqual(8000)
    }
  })
})

describe('Push', () => {
  it('connects, and asks for the changes when the connection is up and on every state event', () => {
    const t = setup()
    t.push.start()
    expect(t.sources).toHaveLength(1)
    expect(t.push.connected).toBe(false)
    expect(t.changes).toHaveLength(0)

    t.last().emit('open')
    expect(t.push.connected).toBe(true)
    expect(t.status).toEqual([true])
    expect(t.changes).toHaveLength(1) // what happened before the connection

    t.last().emit('state', '{"@type":"StateChange","changed":{"a":{"Email":"7"}}}')
    t.last().emit('state', '{"@type":"StateChange","changed":{"a":{"Mailbox":"8"}}}')
    expect(t.changes).toHaveLength(3)

    t.last().emit('ping', '{"interval":30}')
    expect(t.changes).toHaveLength(3) // a ping is not a change
  })

  it('reconnects after an error, waiting twice as long each time, up to a minute', () => {
    const t = setup()
    t.push.start()
    const waits: number[] = []
    for (let i = 0; i < 9; i++) {
      const before = t.sources.length
      t.last().emit('error')
      expect(t.last().closed).toBe(true)
      expect(t.push.connected).toBe(false)
      // Nothing happens until the pause is over.
      let waited = 0
      while (t.sources.length === before) {
        vi.advanceTimersByTime(500)
        waited += 500
        if (waited > 2 * MAX_RETRY_MS) throw new Error('no reconnect')
      }
      waits.push(waited)
    }
    expect(waits).toEqual([1000, 2000, 4000, 8000, 16000, 32000, 60000, 60000, 60000])
  })

  it('starts the pauses over once a connection came up', () => {
    const t = setup()
    t.push.start()
    for (let i = 0; i < 4; i++) {
      t.last().emit('error')
      vi.advanceTimersByTime(MAX_RETRY_MS)
    }
    t.last().emit('open')
    expect(t.push.connected).toBe(true)
    expect(t.changes).toHaveLength(1)
    t.last().emit('error')
    expect(t.status).toEqual([true, false])
    const n = t.sources.length
    vi.advanceTimersByTime(FIRST_RETRY_MS - 1)
    expect(t.sources).toHaveLength(n)
    vi.advanceTimersByTime(1)
    expect(t.sources).toHaveLength(n + 1)
    // Up again: the page catches up on what it missed.
    t.last().emit('open')
    expect(t.changes).toHaveLength(2)
  })

  it('takes silence for a dead connection', () => {
    const t = setup()
    t.push.start()
    t.last().emit('open')
    // Pings keep it alive.
    for (let i = 0; i < 10; i++) {
      vi.advanceTimersByTime(PING_SECONDS * 1000)
      t.last().emit('ping')
    }
    expect(t.sources).toHaveLength(1)
    expect(t.push.connected).toBe(true)
    // Then nothing for two and a half intervals.
    vi.advanceTimersByTime(PING_SECONDS * 2500)
    expect(t.push.connected).toBe(false)
    expect(t.sources[0]!.closed).toBe(true)
    vi.advanceTimersByTime(FIRST_RETRY_MS)
    expect(t.sources).toHaveLength(2)
  })

  it('also gives up on a connection that never opens', () => {
    const t = setup()
    t.push.start()
    vi.advanceTimersByTime(PING_SECONDS * 2500)
    expect(t.sources[0]!.closed).toBe(true)
    vi.advanceTimersByTime(FIRST_RETRY_MS)
    expect(t.sources).toHaveLength(2)
  })

  it('stops: closes the connection, makes no new one, hears nothing more', () => {
    const t = setup()
    t.push.start()
    t.last().emit('open')
    const s = t.last()
    t.push.stop()
    expect(s.closed).toBe(true)
    expect(t.push.connected).toBe(false)
    s.emit('state')
    s.emit('error')
    vi.advanceTimersByTime(10 * MAX_RETRY_MS)
    expect(t.sources).toHaveLength(1)
    expect(t.changes).toHaveLength(1)

    // Stopped in the middle of a pause.
    t.push.start()
    t.last().emit('error')
    t.push.stop()
    vi.advanceTimersByTime(10 * MAX_RETRY_MS)
    expect(t.sources).toHaveLength(2)
    // And started again: a new connection, from the first pause.
    t.push.start()
    expect(t.sources).toHaveLength(3)
    t.push.start() // twice is once
    expect(t.sources).toHaveLength(3)
  })

  it('waits for a session when there is no URL yet', () => {
    let url: string | null = null
    const t = setup(() => url)
    t.push.start()
    expect(t.sources).toHaveLength(0)
    vi.advanceTimersByTime(FIRST_RETRY_MS)
    expect(t.sources).toHaveLength(0)
    vi.advanceTimersByTime(10 * FIRST_RETRY_MS)
    expect(t.sources).toHaveLength(0)
    url = '/jmap/eventsource/?types=*'
    // Waiting for the session did not lengthen the pause.
    vi.advanceTimersByTime(FIRST_RETRY_MS)
    expect(t.sources).toHaveLength(1)
    expect(t.last().url).toBe(url)
  })

  it('ignores events of a connection it replaced', () => {
    const t = setup()
    t.push.start()
    const old = t.last()
    old.emit('error')
    vi.advanceTimersByTime(FIRST_RETRY_MS)
    expect(t.sources).toHaveLength(2)
    old.emit('open')
    old.emit('state')
    expect(t.push.connected).toBe(false)
    expect(t.changes).toHaveLength(0)
  })
})
