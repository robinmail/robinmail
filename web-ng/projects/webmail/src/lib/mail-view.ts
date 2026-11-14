// What the three panes share: the route read as a list spec, navigation,
// the rows of the list, the selection, and the actions on rows.

import { Injectable, computed, inject, signal } from '@angular/core'
import { NavigationEnd, Router } from '@angular/router'
import { type Email, type Id, type ListSpec, type Mailbox, flattenTree, mailboxTree } from '@robinmail/jmap'
import { store } from './app'
import { displayName } from './util/format'

// A Row is one thread in the list.
export interface Row {
  // The email the query returned for the thread.
  id: Id
  email: Email
  threadId: Id
  // Every email of the thread that is loaded, oldest first.
  emails: Email[]
  // Those in the mailbox of the list: what move and delete act on.
  scope: Email[]
  unread: boolean
  flagged: boolean
  attachment: boolean
  names: string
}

export function buildRow(id: Id, mailboxId: Id, showRecipients: boolean): Row | null {
  const email = store.state.emails[id]
  if (!email) return null
  const thread = store.threadEmails(email.threadId)
  const emails = thread.some((e) => e.id === id) ? thread : [email]
  const scope = emails.filter((e) => e.mailboxIds[mailboxId])
  const names = new Set<string>()
  for (const e of emails) for (const a of (showRecipients ? e.to : e.from) ?? []) names.add(displayName(a))
  return {
    id,
    email,
    threadId: email.threadId,
    emails,
    scope: scope.length ? scope : [email],
    unread: emails.some((e) => !e.keywords.$seen),
    flagged: emails.some((e) => e.keywords.$flagged),
    attachment: emails.some((e) => e.hasAttachment),
    names: (showRecipients ? 'To: ' : '') + ([...names].join(', ') || '(nobody)'),
  }
}

const ids = (list: Email[]) => list.map((e) => e.id)

export const actions = {
  // Read: every email of the threads. Unread: the email of the row.
  setRead(rows: Row[], read: boolean) {
    return store.setKeyword(read ? rows.flatMap((r) => ids(r.emails)) : rows.map((r) => r.id), '$seen', read)
  },
  toggleRead(rows: Row[]) {
    return this.setRead(rows, rows.some((r) => r.unread))
  },
  // Flag: the email of the row. Unflag: every email of the threads.
  toggleFlag(rows: Row[]) {
    const flag = rows.some((r) => !r.flagged)
    return store.setKeyword(flag ? rows.filter((r) => !r.flagged).map((r) => r.id) : rows.flatMap((r) => ids(r.emails)), '$flagged', flag)
  },
  move(rows: Row[], from: Id, to: Id) {
    return store.move(rows.flatMap((r) => ids(r.scope)), from, to)
  },
  remove(rows: Row[]) {
    return store.remove(rows.flatMap((r) => ids(r.scope)))
  },
}

export interface MailboxOption {
  mailbox: Mailbox
  depth: number
}

const FILTER_KEYS = ['q', 'from', 'subject', 'unread', 'flagged', 'attachment'] as const

// The route carries what a reload or the back button must bring back:
// /mail/<mailbox id>/<thread id>?q=&from=&subject=&unread=1&flagged=1&attachment=1
@Injectable({ providedIn: 'root' })
export class MailView {
  private readonly router = inject(Router)
  private readonly url = signal(this.router.url)

  // The selected rows of the list, by row id. Empty again in another list.
  readonly selection = signal(new Set<Id>())

  constructor() {
    this.router.events.subscribe((e) => {
      if (e instanceof NavigationEnd) this.url.set(e.urlAfterRedirects)
    })
  }

  private readonly parsed = computed(() => {
    const tree = this.router.parseUrl(this.url())
    const segments = tree.root.children['primary']?.segments.map((s) => s.path) ?? []
    const mail = segments[0] === 'mail'
    return { mail, mailbox: (mail && segments[1]) || '', thread: (mail && segments[2]) || '', query: tree.queryParams as Record<string, unknown> }
  })
  private query(name: string): string {
    const v = this.parsed().query[name]
    return typeof v === 'string' ? v : ''
  }

  readonly onMail = computed(() => this.parsed().mail)
  readonly mailboxId = computed(() => this.parsed().mailbox)
  readonly threadId = computed(() => this.parsed().thread)
  readonly mailbox = computed<Mailbox | undefined>(() => store.state.mailboxes[this.mailboxId()])
  readonly spec = computed<ListSpec>(
    () => ({
      mailboxId: this.mailboxId(),
      text: this.query('q'),
      from: this.query('from'),
      subject: this.query('subject'),
      unread: this.query('unread') === '1',
      flagged: this.query('flagged') === '1',
      attachment: this.query('attachment') === '1',
    }),
    { equal: (a, b) => JSON.stringify(a) === JSON.stringify(b) },
  )
  readonly filterCount = computed(() => {
    const s = this.spec()
    return [s.text, s.from, s.subject, s.unread, s.flagged, s.attachment].filter(Boolean).length
  })

  // All mailboxes in display order, with their depth.
  readonly mailboxes = computed<MailboxOption[]>(() =>
    flattenTree(mailboxTree(Object.values(store.state.mailboxes))).map((n) => ({ mailbox: n.mailbox, depth: n.depth })),
  )

  readonly rows = computed<Row[]>(() => {
    const role = this.mailbox()?.role
    const showRecipients = role === 'sent' || role === 'drafts'
    return store.state.list.ids.map((id) => buildRow(id, this.mailboxId(), showRecipients)).filter((r): r is Row => !!r)
  })
  readonly currentRow = computed(() => this.rows().find((r) => r.threadId === this.threadId()))
  readonly selectedRows = computed(() => this.rows().filter((r) => this.selection().has(r.id)))

  private specQuery(s: ListSpec): Record<string, string> {
    return {
      ...(s.text ? { q: s.text } : {}),
      ...(s.from ? { from: s.from } : {}),
      ...(s.subject ? { subject: s.subject } : {}),
      ...(s.unread ? { unread: '1' } : {}),
      ...(s.flagged ? { flagged: '1' } : {}),
      ...(s.attachment ? { attachment: '1' } : {}),
    }
  }

  // The filter part of the query as it is now, for links that keep it.
  readonly filterQuery = computed(() => {
    const out: Record<string, string> = {}
    for (const k of FILTER_KEYS) {
      const v = this.query(k)
      if (v) out[k] = v
    }
    return out
  })

  mailboxLink(mailbox: Id): string[] {
    return ['/mail', mailbox]
  }
  threadLink(thread: Id): string[] {
    // On the way to another page the route has no mailbox for a moment.
    return this.mailboxId() ? ['/mail', this.mailboxId(), thread] : ['/mail']
  }

  // Opening the first thread adds a history entry, so "back" returns to
  // the list; going from thread to thread replaces it.
  openThread(thread: Id) {
    return this.router.navigate(this.threadLink(thread), { queryParams: this.filterQuery(), replaceUrl: !!this.threadId() })
  }
  closeThread() {
    return this.router.navigate(['/mail', this.mailboxId()], { queryParams: this.filterQuery(), replaceUrl: true })
  }
  setFilter(change: Partial<ListSpec>) {
    const path = ['/mail', this.mailboxId(), ...(this.threadId() ? [this.threadId()] : [])]
    return this.router.navigate(path, { queryParams: this.specQuery({ ...this.spec(), ...change }), replaceUrl: true })
  }

  // step opens the thread after (1) or before (-1) the open one.
  step(by: 1 | -1): void {
    const list = this.rows()
    if (list.length === 0) return
    const at = list.findIndex((r) => r.threadId === this.threadId())
    const next = at === -1 ? (by === 1 ? 0 : list.length - 1) : at + by
    const row = list[next]
    if (row) void this.openThread(row.threadId)
  }

  // leave is called when the open thread is moved or deleted: back to the
  // list. It does not open the next thread: that would mark it read
  // without the user having asked for it.
  leave() {
    return this.closeThread()
  }
}
