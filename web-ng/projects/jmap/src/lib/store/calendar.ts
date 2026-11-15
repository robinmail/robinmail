// The calendar store: the calendars of every account the user sees (their
// own and those shared with them), the events of the range on screen,
// expanded into occurrences by the server, the invitations waiting for an
// answer, and the actions on them.

import { reactive } from '../reactive'
import type { JmapClient } from '../jmap/client'
import { isMethodError } from '../jmap/client'
import type { Calendar, CalendarEvent, Id, Participant, RecurrenceRule, SetError } from '../jmap/types'
import { addDuration, browserZone, formatDuration, localDateTime, parseLocal } from '../calendar/time'

export const CAL = 'urn:ietf:params:jmap:calendars'

export interface CalendarView {
  accountId: Id
  // owner is the name of the account: the user's address, or the one who
  // shares the calendar.
  owner: string
  own: boolean
  calendar: Calendar
  color: string
}

export interface Occurrence {
  // id is what CalendarEvent/set takes: the event's, or an instance's.
  id: Id
  baseId: Id
  accountId: Id
  calendarId: Id
  event: CalendarEvent
  start: Date
  end: Date
  allDay: boolean
  color: string
  recurring: boolean
  isOrigin: boolean
  // myStatus is the user's participationStatus, when they are invited.
  myStatus: string | null
}

export interface EventDraft {
  title: string
  accountId: Id
  calendarId: Id
  allDay: boolean
  start: Date
  end: Date
  timeZone: string
  location: string
  description: string
  repeat: '' | 'daily' | 'weekly' | 'monthly' | 'yearly'
  until: Date | null
  count: number | null
  attendees: string[]
  alertMinutes: number | null
}

const PALETTE = ['#3b82f6', '#16a34a', '#d97706', '#dc2626', '#9333ea', '#0891b2', '#db2777', '#4b5563']

export const EVENT_PROPS = [
  'uid', 'title', 'description', 'start', 'duration', 'timeZone', 'showWithoutTime', 'utcStart', 'utcEnd', 'calendarIds', 'participants',
  'organizerCalendarAddress', 'locations', 'baseEventId', 'alerts', 'status', 'recurrenceRule', 'isOrigin', 'sequence',
]

export class CalendarError extends Error {}

function setError(e: SetError | undefined, what: string): CalendarError {
  return new CalendarError(e?.description ? `${what}: ${e.description}` : what)
}

export class CalendarStore {
  readonly state = reactive({
    ready: false,
    calendars: [] as CalendarView[],
    // Calendars the user switched off in this browser.
    hidden: {} as Record<string, boolean>,
    occurrences: [] as Occurrence[],
    invitations: [] as Occurrence[],
    loading: false,
    error: '',
  })
  private range: { from: Date; to: Date } | null = null

  constructor(private client: JmapClient) {}

  // me is the user's calendar address.
  get me(): string {
    return 'mailto:' + (this.client.session?.username ?? '').toLowerCase()
  }

  private accounts(): { id: Id; name: string; own: boolean }[] {
    const s = this.client.session
    if (!s) return []
    const primary = s.primaryAccounts[CAL] ?? this.client.accountId
    return Object.entries(s.accounts)
      .filter(([, a]) => CAL in a.accountCapabilities)
      .map(([id, a]) => ({ id, name: a.name, own: id === primary }))
      .sort((a, b) => Number(b.own) - Number(a.own) || a.name.localeCompare(b.name))
  }

  key(c: { accountId: Id; calendarId?: Id; calendar?: Calendar }): string {
    return `${c.accountId}/${c.calendarId ?? c.calendar?.id}`
  }

  async load(): Promise<void> {
    if (!this.client.session) await this.client.loadSession()
    const accounts = this.accounts()
    const b = this.client.batch()
    const calls = accounts.map((a) => b.call('Calendar/get', { accountId: a.id }))
    const res = await b.send()
    const out: CalendarView[] = []
    accounts.forEach((a, i) => {
      for (const c of res.get(calls[i]!).list.sort((x, y) => x.sortOrder - y.sortOrder || x.name.localeCompare(y.name))) {
        out.push({ accountId: a.id, owner: a.name, own: a.own, calendar: c, color: c.color || PALETTE[out.length % PALETTE.length]! })
      }
    })
    this.state.calendars = out
    this.state.ready = true
  }

  calendarOf(accountId: Id, calendarId: Id): CalendarView | undefined {
    return this.state.calendars.find((c) => c.accountId === accountId && c.calendar.id === calendarId)
  }

  // occurrence turns an event (or an instance) from the server into what
  // the screens draw.
  occurrence(accountId: Id, ev: CalendarEvent): Occurrence {
    const calendarId = Object.keys(ev.calendarIds ?? {})[0] ?? ''
    const allDay = !!ev.showWithoutTime
    let start: Date
    if (allDay || !ev.timeZone) start = parseLocal(ev.start) ?? new Date(ev.utcStart ?? ev.start)
    else start = new Date(ev.utcStart ?? ev.start)
    let end = ev.utcEnd && !allDay && ev.timeZone ? new Date(ev.utcEnd) : addDuration(start, ev.duration ?? (allDay ? 'P1D' : 'PT0S'))
    if (end < start) end = start
    let myStatus: string | null = null
    for (const p of Object.values(ev.participants ?? {})) {
      if (this.isMe(p) && (p.roles?.['attendee'] || !p.roles?.['owner'])) myStatus = p.participationStatus ?? 'needs-action'
    }
    const isOrigin = ev.isOrigin ?? true
    if (isOrigin) myStatus = null
    return {
      id: ev.id, baseId: ev.baseEventId ?? ev.id, accountId, calendarId, event: ev, start, end, allDay,
      color: this.calendarOf(accountId, calendarId)?.color ?? PALETTE[0]!, recurring: !!ev.baseEventId || !!ev.recurrenceRule,
      isOrigin, myStatus,
    }
  }

  isMe(p: Participant): boolean {
    const a = (p.calendarAddress ?? (p.email ? 'mailto:' + p.email : '')).toLowerCase()
    return a === this.me
  }

  // loadRange fetches the occurrences between two instants.
  async loadRange(from: Date, to: Date): Promise<void> {
    this.range = { from, to }
    this.state.loading = true
    try {
      const accounts = this.accounts()
      const b = this.client.batch()
      const gets = accounts.map((a) => {
        const q = b.call('CalendarEvent/query', {
          accountId: a.id, filter: { after: localDateTime(from), before: localDateTime(to) }, expandRecurrences: true, timeZone: browserZone(), limit: 500,
        })
        return b.call('CalendarEvent/get', { accountId: a.id, '#ids': q.ref('/ids'), properties: EVENT_PROPS.filter((p) => p !== 'recurrenceRule') })
      })
      const res = await b.send()
      const out: Occurrence[] = []
      accounts.forEach((a, i) => {
        for (const ev of res.get(gets[i]!).list) out.push(this.occurrence(a.id, ev))
      })
      // Only the answer to the newest request counts.
      if (this.range.from === from && this.range.to === to) {
        this.state.occurrences = out.sort((x, y) => x.start.getTime() - y.start.getTime())
        this.state.error = ''
      }
    } catch (e) {
      this.state.error = (e as Error).message
    } finally {
      this.state.loading = false
    }
  }

  async refresh(): Promise<void> {
    await this.load()
    if (this.range) await this.loadRange(this.range.from, this.range.to)
    await this.loadInvitations()
  }

  // loadInvitations finds the coming events the user has not answered.
  async loadInvitations(): Promise<void> {
    const own = this.accounts().find((a) => a.own)
    if (!own) return
    const now = new Date()
    const b = this.client.batch()
    const q = b.call('CalendarEvent/query', { accountId: own.id, filter: { after: localDateTime(now), participant: this.me.slice(7) }, limit: 100 })
    const g = b.call('CalendarEvent/get', { accountId: own.id, '#ids': q.ref('/ids'), properties: EVENT_PROPS })
    const res = await b.send()
    this.state.invitations = res.get(g).list.map((ev) => this.occurrence(own.id, ev)).filter((o) => o.myStatus === 'needs-action')
  }

  visible(o: { accountId: Id; calendarId: Id }): boolean {
    return !this.state.hidden[this.key(o)]
  }

  toggle(c: CalendarView): void {
    const k = this.key({ accountId: c.accountId, calendar: c.calendar })
    this.state.hidden = { ...this.state.hidden, [k]: !this.state.hidden[k] }
  }

  // eventFromDraft makes the JSCalendar properties of a draft.
  eventFromDraft(d: EventDraft): Partial<CalendarEvent> {
    const ev: Partial<CalendarEvent> = {
      calendarIds: { [d.calendarId]: true },
      title: d.title,
      description: d.description || undefined,
      showWithoutTime: d.allDay,
      start: d.allDay ? localDateTime(new Date(d.start.getFullYear(), d.start.getMonth(), d.start.getDate())) : localDateTime(d.start),
      duration: formatDuration(d.start, d.end, d.allDay),
    }
    ev.timeZone = d.allDay ? null : d.timeZone || browserZone()
    ev.locations = d.location ? { l1: { '@type': 'Location', name: d.location } } : null
    if (d.repeat) {
      const r: RecurrenceRule = { '@type': 'RecurrenceRule', frequency: d.repeat }
      if (d.count) r.count = d.count
      else if (d.until) r.until = localDateTime(new Date(d.until.getFullYear(), d.until.getMonth(), d.until.getDate(), 23, 59, 59))
      ev.recurrenceRule = r
    } else {
      ev.recurrenceRule = null
    }
    ev.alerts = d.alertMinutes === null ? null : { a1: { '@type': 'Alert', trigger: { '@type': 'OffsetTrigger', offset: d.alertMinutes === 0 ? 'PT0S' : `-PT${d.alertMinutes}M` } } }
    if (d.attendees.length) {
      const parts: Record<string, Participant> = {
        me: { '@type': 'Participant', calendarAddress: this.me, email: this.me.slice(7), roles: { owner: true, attendee: true }, participationStatus: 'accepted' },
      }
      d.attendees.forEach((a, i) => {
        parts[`a${i}`] = { '@type': 'Participant', calendarAddress: 'mailto:' + a, email: a, roles: { attendee: true }, participationStatus: 'needs-action', expectReply: true }
      })
      ev.participants = parts
    } else {
      ev.participants = null
    }
    return ev
  }

  // create makes a new event; with attendees the invitations go out.
  async create(d: EventDraft): Promise<Id> {
    const b = this.client.batch()
    const s = b.call('CalendarEvent/set', { accountId: d.accountId, create: { new: this.eventFromDraft(d) }, sendSchedulingMessages: d.attendees.length > 0 })
    const res = (await b.send()).get(s)
    if (!res.created?.['new']) throw setError(res.notCreated?.['new'], 'The event was not saved')
    await this.refresh()
    return res.created['new'].id
  }

  // update changes an event: one occurrence (scope "one"), or the whole
  // series. A whole series keeps its own start date; the time of day and
  // the length come from the draft.
  async update(o: Occurrence, d: EventDraft, scope: 'one' | 'all'): Promise<void> {
    const ev = this.eventFromDraft(d)
    let id = o.id
    const patch: Record<string, unknown> = { ...ev }
    if (scope === 'all' && o.recurring) {
      id = o.baseId
      const b = this.client.batch()
      const g = b.call('CalendarEvent/get', { accountId: o.accountId, ids: [id], properties: ['start', 'timeZone', 'showWithoutTime'] })
      const base = (await b.send()).get(g).list[0]
      const baseStart = base ? parseLocal(base.start) : null
      if (baseStart) {
        const shift = d.start.getTime() - o.start.getTime()
        const moved = new Date(baseStart.getTime() + shift)
        patch['start'] = localDateTime(d.allDay ? new Date(moved.getFullYear(), moved.getMonth(), moved.getDate()) : moved)
      }
    } else if (scope === 'one' && o.recurring) {
      delete patch['recurrenceRule']
      delete patch['calendarIds']
    }
    if (d.accountId !== o.accountId) throw new CalendarError('An event cannot move to a calendar of another account.')
    const b = this.client.batch()
    const s = b.call('CalendarEvent/set', { accountId: o.accountId, update: { [id]: patch }, sendSchedulingMessages: !!o.event.participants })
    const res = (await b.send()).get(s)
    if (res.notUpdated?.[id]) throw setError(res.notUpdated[id], 'The event was not saved')
    await this.refresh()
  }

  async remove(o: Occurrence, scope: 'one' | 'all'): Promise<void> {
    const id = scope === 'all' ? o.baseId : o.id
    const b = this.client.batch()
    const s = b.call('CalendarEvent/set', { accountId: o.accountId, destroy: [id], sendSchedulingMessages: !!o.event.participants })
    const res = (await b.send()).get(s)
    if (res.notDestroyed?.[id]) throw setError(res.notDestroyed[id], 'The event was not deleted')
    await this.refresh()
  }

  // respond answers an invitation: for the whole event, which tells the
  // organizer (JMAP Calendars 7, a REPLY).
  async respond(o: Occurrence, status: 'accepted' | 'tentative' | 'declined'): Promise<void> {
    const pid = Object.entries(o.event.participants ?? {}).find(([, p]) => this.isMe(p))?.[0]
    if (!pid) throw new CalendarError('You are not among the participants.')
    const b = this.client.batch()
    const s = b.call('CalendarEvent/set', {
      accountId: o.accountId, update: { [o.baseId]: { [`participants/${pid}/participationStatus`]: status } }, sendSchedulingMessages: true,
    })
    const res = (await b.send()).get(s)
    if (res.notUpdated?.[o.baseId]) throw setError(res.notUpdated[o.baseId], 'The answer was not saved')
    await this.refresh()
  }

  async createCalendar(name: string, color: string): Promise<void> {
    const own = this.accounts().find((a) => a.own)
    const b = this.client.batch()
    const s = b.call('Calendar/set', { accountId: own?.id, create: { c: { name, color } } })
    const res = (await b.send()).get(s)
    if (!res.created?.['c']) throw setError(res.notCreated?.['c'], 'The calendar was not made')
    await this.load()
  }

  async updateCalendar(c: CalendarView, patch: Record<string, unknown>): Promise<void> {
    const b = this.client.batch()
    const s = b.call('Calendar/set', { accountId: c.accountId, update: { [c.calendar.id]: patch } })
    const res = (await b.send()).get(s)
    if (res.notUpdated?.[c.calendar.id]) throw setError(res.notUpdated[c.calendar.id], 'The calendar was not changed')
    await this.load()
  }

  async deleteCalendar(c: CalendarView): Promise<void> {
    const b = this.client.batch()
    const s = b.call('Calendar/set', { accountId: c.accountId, destroy: [c.calendar.id], onDestroyRemoveEvents: true })
    const res = (await b.send()).get(s)
    if (res.notDestroyed?.[c.calendar.id]) throw setError(res.notDestroyed[c.calendar.id], 'The calendar was not deleted')
    await this.refresh()
  }

  // principalByEmail finds the account of an address on this server.
  async principalByEmail(email: string): Promise<Id | null> {
    const b = this.client.batch()
    const q = b.call('Principal/query', { filter: { email } })
    return (await b.send()).get(q).ids[0] ?? null
  }

  // share gives an address read or write access to a calendar, or with
  // rights null takes it away.
  async share(c: CalendarView, email: string, rights: 'read' | 'write' | null): Promise<void> {
    const pid = await this.principalByEmail(email)
    if (!pid) throw new CalendarError(`${email} has no account on this server.`)
    const shareWith: Record<string, unknown> = {}
    for (const [k, v] of Object.entries(c.calendar.shareWith ?? {})) shareWith[k] = v
    if (rights === null) delete shareWith[pid]
    else shareWith[pid] = { mayReadFreeBusy: true, mayReadItems: true, mayWriteAll: rights === 'write', mayWriteOwn: rights === 'write', mayUpdatePrivate: rights === 'write', mayRSVP: rights === 'write', mayShare: false, mayDelete: false }
    await this.updateCalendar(c, { shareWith })
  }

  // importICS takes the events of an .ics file into a calendar, without
  // sending invitations: they are a copy of what is elsewhere.
  async importICS(file: Blob, c: CalendarView): Promise<number> {
    const up = await this.client.upload(file, 'text/calendar', {})
    const b = this.client.batch()
    const p = b.call('CalendarEvent/parse', { accountId: this.client.accountId, blobIds: [up.blobId] })
    const parsed = (await b.send()).get(p)
    const events = parsed.parsed?.[up.blobId] ?? []
    if (!events.length) throw new CalendarError('The file holds no events.')
    const create: Record<string, Partial<CalendarEvent>> = {}
    events.forEach((ev, i) => {
      const x: Record<string, unknown> = { ...ev, calendarIds: { [c.calendar.id]: true } }
      for (const k of ['id', 'baseEventId', 'isDraft', 'isOrigin', 'method']) delete x[k]
      create[`i${i}`] = x as Partial<CalendarEvent>
    })
    const b2 = this.client.batch()
    const s = b2.call('CalendarEvent/set', { accountId: c.accountId, create })
    let res
    try {
      res = (await b2.send()).get(s)
    } catch (e) {
      if (isMethodError(e)) throw new CalendarError(e.description || e.type)
      throw e
    }
    await this.refresh()
    return Object.keys(res.created ?? {}).length
  }

  // exportURL is where the whole calendar can be downloaded as .ics.
  exportURL(c: CalendarView): string {
    const me = this.accounts().find((a) => a.own)?.id ?? ''
    const seg = c.own ? c.calendar.id : `${c.accountId}-${c.calendar.id}`
    return `/dav/calendars/${me}/${seg}/`
  }
}
