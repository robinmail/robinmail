// Dates for the calendar screens: days, weeks and months in the browser's
// zone, the LocalDateTime and Duration strings of JSCalendar (RFC 8984
// 1.4), and how events lie in a day.

export const DAY_MS = 86_400_000

export function startOfDay(d: Date): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate())
}

export function addDays(d: Date, n: number): Date {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() + n, d.getHours(), d.getMinutes(), d.getSeconds())
}

export function addMonths(d: Date, n: number): Date {
  const x = new Date(d.getFullYear(), d.getMonth() + n, 1)
  const last = new Date(x.getFullYear(), x.getMonth() + 1, 0).getDate()
  return new Date(x.getFullYear(), x.getMonth(), Math.min(d.getDate(), last))
}

// startOfWeek: weeks start on Monday (ISO 8601), as in most of Europe.
export function startOfWeek(d: Date, firstDay = 1): Date {
  const day = startOfDay(d)
  const off = (day.getDay() - firstDay + 7) % 7
  return addDays(day, -off)
}

export function sameDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate()
}

// monthGrid: the six weeks shown for a month, Monday first.
export function monthGrid(month: Date): Date[] {
  const first = new Date(month.getFullYear(), month.getMonth(), 1)
  const start = startOfWeek(first)
  return Array.from({ length: 42 }, (_, i) => addDays(start, i))
}

const pad = (n: number, w = 2) => String(n).padStart(w, '0')

// localDateTime writes the wall clock of a Date (in the browser's zone)
// as a JSCalendar LocalDateTime.
export function localDateTime(d: Date): string {
  return `${pad(d.getFullYear(), 4)}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

export function localDate(d: Date): string {
  return localDateTime(d).slice(0, 10)
}

// parseLocal reads a LocalDateTime (or a date, or what an <input
// type="datetime-local"> gives) as a wall clock in the browser's zone.
export function parseLocal(s: string): Date | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})(?:T(\d{2}):(\d{2})(?::(\d{2}))?)?$/.exec(s)
  if (!m) return null
  const d = new Date(+m[1]!, +m[2]! - 1, +m[3]!, +(m[4] ?? 0), +(m[5] ?? 0), +(m[6] ?? 0))
  return Number.isNaN(d.getTime()) ? null : d
}

// The parts of a Duration: weeks and days, then hours, minutes, seconds.
export function parseDuration(s: string): { days: number; ms: number } | null {
  const m = /^(-)?P(?:(\d+)W)?(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$/.exec(s ?? '')
  if (!m || s === 'P' || s.endsWith('T')) return null
  const sign = m[1] ? -1 : 1
  const days = (+(m[2] ?? 0) * 7 + +(m[3] ?? 0)) * sign
  const ms = ((+(m[4] ?? 0) * 60 + +(m[5] ?? 0)) * 60 + +(m[6] ?? 0)) * 1000 * sign
  return { days, ms }
}

export function addDuration(d: Date, s: string): Date {
  const p = parseDuration(s)
  if (!p) return d
  return new Date(addDays(d, p.days).getTime() + p.ms)
}

// formatDuration writes the time between two Dates: whole days as days.
export function formatDuration(from: Date, to: Date, allDay: boolean): string {
  if (allDay) {
    const days = Math.max(1, Math.round((startOfDay(to).getTime() - startOfDay(from).getTime()) / DAY_MS))
    return `P${days}D`
  }
  let s = Math.max(0, Math.round((to.getTime() - from.getTime()) / 1000))
  const days = Math.floor(s / 86400)
  s -= days * 86400
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  const sec = s % 60
  let out = 'P' + (days ? `${days}D` : '')
  if (h || m || sec || !days) out += 'T' + (h ? `${h}H` : '') + (m ? `${m}M` : '') + (sec || (!h && !m) ? `${sec}S` : '')
  return out
}

// A positioned event in a day of the week view.
export interface Placed<T> {
  item: T
  top: number // minutes from midnight
  height: number // minutes
  column: number
  columns: number
}

// layoutDay places the timed events of one day in columns, so that events
// that overlap stand side by side.
export function layoutDay<T extends { start: Date; end: Date }>(day: Date, items: T[]): Placed<T>[] {
  const dayStart = startOfDay(day).getTime()
  const dayEnd = addDays(startOfDay(day), 1).getTime()
  const parts = items
    .filter((e) => e.start.getTime() < dayEnd && e.end.getTime() > dayStart)
    .map((e) => {
      const s = Math.max(e.start.getTime(), dayStart)
      const end = Math.min(Math.max(e.end.getTime(), e.start.getTime() + 15 * 60_000), dayEnd)
      return { item: e, s, e: end }
    })
    .sort((a, b) => a.s - b.s || b.e - a.e)
  const out: Placed<T>[] = []
  // Clusters of events that overlap one another get the same width.
  let cluster: { p: Placed<T>; e: number }[] = []
  let clusterEnd = 0
  const flush = () => {
    const n = Math.max(1, ...cluster.map((c) => c.p.column + 1))
    for (const c of cluster) c.p.columns = n
    cluster = []
  }
  for (const x of parts) {
    if (x.s >= clusterEnd) flush()
    const used = new Set(cluster.filter((c) => c.e > x.s).map((c) => c.p.column))
    let col = 0
    while (used.has(col)) col++
    const p: Placed<T> = { item: x.item, top: (x.s - dayStart) / 60_000, height: (x.e - x.s) / 60_000, column: col, columns: 1 }
    cluster.push({ p, e: x.e })
    out.push(p)
    clusterEnd = Math.max(clusterEnd, x.e)
  }
  flush()
  return out
}

const DAYS = ['sunday', 'monday', 'tuesday', 'wednesday', 'thursday', 'friday', 'saturday']
const SHORT = ['su', 'mo', 'tu', 'we', 'th', 'fr', 'sa']

// describeRule says in words how an event repeats.
export function describeRule(r: { frequency: string; interval?: number; count?: number; until?: string; byDay?: { day: string }[] } | null | undefined): string {
  if (!r) return 'Does not repeat'
  const n = r.interval ?? 1
  const unit = { daily: 'day', weekly: 'week', monthly: 'month', yearly: 'year', hourly: 'hour', minutely: 'minute', secondly: 'second' }[r.frequency] ?? r.frequency
  let s = n === 1 ? `Every ${unit}` : `Every ${n} ${unit}s`
  if (r.byDay?.length) s += ' on ' + r.byDay.map((d) => DAYS[SHORT.indexOf(d.day)] ?? d.day).map((d) => d[0]!.toUpperCase() + d.slice(1)).join(', ')
  if (r.count) s += `, ${r.count} times`
  if (r.until) s += `, until ${r.until.slice(0, 10)}`
  return s
}

// browserZone is the IANA name of the browser's time zone.
export function browserZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone || 'Etc/UTC'
}
