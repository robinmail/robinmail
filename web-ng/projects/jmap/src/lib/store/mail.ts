// The mail store: what the webmail knows of one account, kept in sync with
// the server through /changes and /queryChanges, and the actions on it.
// Actions change the local state at once and send the Email/set after; a
// refused change is put back.

import { reactive } from '../reactive'
import type { Batch, Call, JmapClient } from '../jmap/client'
import { HttpError, isMethodError } from '../jmap/client'
import { adjustCounts, applyPatch, applyQueryChanges, type EmailPatch } from '../jmap/sync'
import type { Email, EmailQueryArgs, Filter, Id, Mailbox, Quota, ResultReference, SearchSnippet, SetError, Thread } from '../jmap/types'

export const PAGE = 30

export const LIST_PROPS = [
  'id', 'blobId', 'threadId', 'mailboxIds', 'keywords', 'size', 'receivedAt', 'hasAttachment', 'preview', 'from', 'to', 'subject',
]
export const BODY_PROPS = [
  ...LIST_PROPS, 'cc', 'bcc', 'replyTo', 'sentAt', 'messageId', 'inReplyTo', 'references', 'textBody', 'htmlBody', 'attachments', 'bodyValues',
]
export const BODY_PART_PROPS = ['partId', 'blobId', 'size', 'name', 'type', 'charset', 'disposition', 'cid']

// ListSpec is what the route says about the message list.
export interface ListSpec {
  mailboxId: Id
  // text is what the search box holds: full-text search in the mailbox.
  text: string
  from: string
  subject: string
  unread: boolean
  flagged: boolean
  attachment: boolean
}

export function buildFilter(spec: ListSpec): Filter {
  const c: Filter[] = [{ inMailbox: spec.mailboxId }]
  if (spec.text) c.push({ text: spec.text })
  if (spec.from) c.push({ from: spec.from })
  if (spec.subject) c.push({ subject: spec.subject })
  if (spec.unread) c.push({ notKeyword: '$seen' })
  if (spec.flagged) c.push({ hasKeyword: '$flagged' })
  if (spec.attachment) c.push({ hasAttachment: true })
  return c.length === 1 ? c[0]! : { operator: 'AND', conditions: c }
}

function queryArgs(spec: ListSpec): EmailQueryArgs {
  return { filter: buildFilter(spec), sort: [{ property: 'receivedAt', isAscending: false }], collapseThreads: true }
}

export type Change = EmailPatch | 'destroy'

interface ListState {
  key: string
  spec: ListSpec | null
  // One email id per thread: the newest email of the thread that matches.
  ids: Id[]
  total: number
  queryState: string
  // For a search: what matched in each email, by email id.
  snippets: Record<Id, SearchSnippet>
  loading: boolean
  loadingMore: boolean
  error: string
}

export class MailStore {
  readonly state = reactive({
    ready: false,
    username: '',
    mailboxes: {} as Record<Id, Mailbox>,
    emails: {} as Record<Id, Email>,
    threads: {} as Record<Id, Thread>,
    list: { key: '', spec: null, ids: [], total: 0, queryState: '', snippets: {}, loading: false, loadingMore: false, error: '' } as ListState,
    // The limits of the account and how full it is; empty without limits.
    quota: [] as Quota[],
    // The server did not answer the last refresh.
    offline: false,
  })

  // Told when an action was refused or failed.
  onError: (message: string) => void = () => {}

  // Counts local changes. A refresh that was under way when one happened
  // carries data from before it and is dropped.
  private epoch = 0
  // Changes made here whose Email/set has not been answered. While there
  // are any, what the server says is older than what we show.
  private pending = 0
  private listToken = 0
  private queue: Promise<unknown> = Promise.resolve()
  private refreshing: Promise<void> | null = null
  private again = false

  constructor(readonly client: JmapClient) {}

  // serial runs requests that write or merge state one after another.
  private serial<T>(fn: () => Promise<T>): Promise<T> {
    const p = this.queue.then(fn, fn)
    this.queue = p.catch(() => {})
    return p
  }

  reset(): void {
    this.epoch++
    this.listToken++
    const s = this.state
    s.ready = false
    s.username = ''
    s.mailboxes = {}
    s.emails = {}
    s.threads = {}
    s.quota = []
    s.list = { key: '', spec: null, ids: [], total: 0, queryState: '', snippets: {}, loading: false, loadingMore: false, error: '' }
    s.offline = false
    this.client.reset()
  }

  // load fetches the session and all mailboxes, and learns the state of
  // Email and Thread. It is called while the store holds no emails or
  // threads, or by reloadAll, which fetches the ones it holds again.
  async load(): Promise<void> {
    const session = await this.client.loadSession()
    const b = this.client.batch()
    const m = b.call('Mailbox/get', { ids: null })
    const e = b.call('Email/get', { ids: [], properties: ['id'] })
    const t = b.call('Thread/get', { ids: [] })
    const quota = b.call('Quota/get', { ids: null })
    const r = await b.send()
    const s = this.state
    // A server without quotas answers with an error: then there are none.
    s.quota = r.error(quota) ? [] : r.get(quota).list
    this.setMailboxes(r.get(m).list)
    this.client.setState('Mailbox', r.get(m).state)
    this.client.setState('Email', r.get(e).state)
    this.client.setState('Thread', r.get(t).state)
    s.username = session.username
    s.ready = true
    s.offline = false
  }

  private setMailboxes(list: Mailbox[]): void {
    this.state.mailboxes = Object.fromEntries(list.map((m) => [m.id, m]))
  }

  mailboxByRole(role: string): Mailbox | undefined {
    return Object.values(this.state.mailboxes).find((m) => m.role === role)
  }

  private mergeEmails(list: Email[]): void {
    for (const e of list) this.state.emails[e.id] = { ...this.state.emails[e.id], ...e }
  }

  private mergeThreads(list: Thread[]): void {
    for (const t of list) this.state.threads[t.id] = t
  }

  // threadEmails returns the emails of a thread that are loaded, oldest
  // first, as Thread/get orders them.
  threadEmails(threadId: Id): Email[] {
    const t = this.state.threads[threadId]
    if (!t) return []
    return t.emailIds.map((id) => this.state.emails[id]).filter((e): e is Email => !!e)
  }

  // threadChain adds the calls that fetch, for some email ids, their
  // threads and every email of those threads.
  private threadChain(b: Batch, ids: ResultReference): { threads: Call<'Thread/get'>; emails: Call<'Email/get'> } {
    const first = b.call('Email/get', { '#ids': ids, properties: ['threadId'] })
    const threads = b.call('Thread/get', { '#ids': first.ref('/list/*/threadId') })
    const emails = b.call('Email/get', { '#ids': threads.ref('/list/*/emailIds'), properties: LIST_PROPS })
    return { threads, emails }
  }

  // --- the message list ---

  // openList shows the list of a spec, from the server.
  async openList(spec: ListSpec): Promise<void> {
    const key = JSON.stringify(spec)
    const list = this.state.list
    if (key === list.key && !list.error) return
    this.listToken++
    Object.assign(list, { key, spec, ids: [], total: 0, queryState: '', snippets: {}, loading: true, loadingMore: false, error: '' })
    await this.fetchList(0, PAGE, true)
  }

  // reloadList runs the query again for the window that is loaded.
  reloadList(): Promise<void> {
    if (!this.state.list.spec) return Promise.resolve()
    this.listToken++
    return this.fetchList(0, Math.max(PAGE, this.state.list.ids.length), true)
  }

  async loadMore(): Promise<void> {
    const list = this.state.list
    if (!list.spec || list.loading || list.loadingMore || list.ids.length >= list.total) return
    list.loadingMore = true
    await this.fetchList(list.ids.length, PAGE, false)
  }

  private fetchList(position: number, limit: number, replace: boolean): Promise<void> {
    const list = this.state.list
    const token = this.listToken
    const spec = list.spec!
    return this.serial(async () => {
      if (token !== this.listToken) return
      if (this.pending > 0) {
        // Behind the changes that are waiting in the queue.
        void this.fetchList(position, limit, replace)
        return
      }
      const epoch = this.epoch
      try {
        const b = this.client.batch()
        const q = b.call('Email/query', { ...queryArgs(spec), position, limit, calculateTotal: true })
        const chain = this.threadChain(b, q.ref('/ids'))
        // For a search, what matched in each result comes along.
        const snip = spec.text ? b.call('SearchSnippet/get', { filter: { text: spec.text }, '#emailIds': q.ref('/ids') }) : null
        const r = await b.send()
        if (token !== this.listToken) return
        const qr = r.get(q)
        if (snip) {
          const found = Object.fromEntries(r.get(snip).list.map((x) => [x.emailId, x]))
          list.snippets = replace ? found : { ...list.snippets, ...found }
        }
        this.mergeThreads(r.get(chain.threads).list)
        // Emails changed here since the request left are newer than the answer.
        if (epoch === this.epoch) this.mergeEmails(r.get(chain.emails).list)
        else this.mergeEmails(r.get(chain.emails).list.filter((e) => !this.state.emails[e.id]))
        let stale = epoch !== this.epoch
        if (replace) {
          list.ids = qr.ids
          list.queryState = qr.queryState
        } else {
          const have = new Set(list.ids)
          list.ids.push(...qr.ids.filter((id) => !have.has(id)))
          // The result moved between the pages: a row may have slipped
          // through the gap. Fetch the whole window again.
          stale ||= qr.queryState !== list.queryState
        }
        list.total = qr.total ?? list.ids.length
        list.error = ''
        if (stale) void this.reloadList()
      } catch (e) {
        if (token !== this.listToken) return
        list.error = e instanceof Error ? e.message : String(e)
      } finally {
        if (token === this.listToken) {
          list.loading = false
          list.loadingMore = false
        }
      }
    })
  }

  // --- a thread ---

  // openThread loads a thread with the bodies of its emails.
  openThread(threadId: Id): Promise<void> {
    return this.serial(async () => {
      const b = this.client.batch()
      const t = b.call('Thread/get', { ids: [threadId] })
      const e = this.bodyCall(b, { '#ids': t.ref('/list/*/emailIds') })
      const r = await b.send()
      this.mergeThreads(r.get(t).list)
      this.mergeBodies(r.get(e).list)
      if (r.get(t).notFound.includes(threadId)) delete this.state.threads[threadId]
    })
  }

  // loadBodies loads the bodies of emails that came in through a refresh.
  loadBodies(ids: Id[]): Promise<void> {
    return this.serial(async () => {
      const b = this.client.batch()
      const e = this.bodyCall(b, { ids })
      this.mergeBodies((await b.send()).get(e).list)
    })
  }

  private bodyCall(b: Batch, ids: { ids: Id[] } | { '#ids': ResultReference }): Call<'Email/get'> {
    return b.call('Email/get', {
      ...ids,
      properties: BODY_PROPS,
      bodyProperties: BODY_PART_PROPS,
      fetchTextBodyValues: true,
      maxBodyValueBytes: 1_000_000,
    })
  }

  // mergeBodies takes the body properties of freshly fetched emails, and
  // the rest only for emails not held yet: keywords and mailboxes we hold
  // may be newer than this answer (a change of ours on its way).
  private mergeBodies(list: Email[]): void {
    for (const e of list) {
      const have = this.state.emails[e.id]
      if (!have) {
        this.state.emails[e.id] = e
        continue
      }
      const { keywords: _k, mailboxIds: _m, ...rest } = e
      this.state.emails[e.id] = { ...have, ...rest }
    }
  }

  // --- keeping in sync ---

  // refresh fetches what changed since the states we hold. One runs at a
  // time; a call during a refresh makes one more follow.
  refresh(): Promise<void> {
    if (!this.state.ready) return Promise.resolve()
    if (this.refreshing) {
      this.again = true
      return this.refreshing
    }
    this.refreshing = this.serial(() => this.refreshLoop())
      .then(
        () => {
          this.state.offline = false
        },
        (e) => {
          if (e instanceof HttpError && e.status === 401) return
          this.state.offline = true
        },
      )
      .finally(() => {
        this.refreshing = null
        if (this.again) {
          this.again = false
          void this.refresh()
        }
      })
    return this.refreshing
  }

  private async refreshLoop(): Promise<void> {
    // hasMoreChanges: ask again, but not forever.
    for (let i = 0; i < 20; i++) {
      if (!(await this.refreshOnce())) return
    }
  }

  // refreshOnce applies one round of changes. It reports whether the
  // server has more.
  private async refreshOnce(): Promise<boolean> {
    const { client, state } = this
    // A change of ours is waiting to be sent: it refreshes when it is done.
    if (this.pending > 0) return false
    const epoch = this.epoch
    const token = this.listToken
    const list = state.list
    const b = client.batch()
    const mc = b.call('Mailbox/changes', { sinceState: client.states.Mailbox ?? '' })
    const mCreated = b.call('Mailbox/get', { '#ids': mc.ref('/created') })
    const mUpdated = b.call('Mailbox/get', { '#ids': mc.ref('/updated') })
    const ec = b.call('Email/changes', { sinceState: client.states.Email ?? '' })
    const eCreated = b.call('Email/get', { '#ids': ec.ref('/created'), properties: LIST_PROPS })
    const eUpdated = b.call('Email/get', { '#ids': ec.ref('/updated'), properties: LIST_PROPS })
    const tc = b.call('Thread/changes', { sinceState: client.states.Thread ?? '' })
    const tUpdated = b.call('Thread/get', { '#ids': tc.ref('/updated') })
    let qc: Call<'Email/queryChanges'> | null = null
    let chain: ReturnType<MailStore['threadChain']> | null = null
    if (list.spec && list.queryState && !list.loading) {
      qc = b.call('Email/queryChanges', { ...queryArgs(list.spec), sinceQueryState: list.queryState, calculateTotal: true })
      chain = this.threadChain(b, qc.ref('/added/*/id'))
    }
    // How full the account is moves with the mail: two small objects.
    const quota = b.call('Quota/get', { ids: null })
    const r = await b.send()
    if (epoch !== this.epoch) return false // from before a local change; the action refreshes when it is done

    // A state too old for the server: start over.
    for (const c of [mc, ec, tc]) {
      const err = r.error(c)
      if (err) {
        if (err.type !== 'cannotCalculateChanges') throw err
        await this.reloadAll()
        return false
      }
    }

    if (!r.error(quota)) state.quota = r.get(quota).list
    const mr = r.get(mc)
    for (const m of [...r.get(mCreated).list, ...r.get(mUpdated).list]) state.mailboxes[m.id] = m
    for (const id of mr.destroyed) delete state.mailboxes[id]
    client.setState('Mailbox', mr.newState)

    const er = r.get(ec)
    for (const id of er.destroyed) this.dropEmail(id)
    this.mergeEmails([...r.get(eCreated).list, ...r.get(eUpdated).list])
    client.setState('Email', er.newState)

    const tr = r.get(tc)
    this.mergeThreads(r.get(tUpdated).list)
    for (const id of tr.destroyed) delete state.threads[id]
    client.setState('Thread', tr.newState)

    if (qc && chain && token === this.listToken) {
      if (r.error(qc)) {
        void this.reloadList()
      } else {
        const qr = r.get(qc)
        this.mergeThreads(r.get(chain.threads).list)
        this.mergeEmails(r.get(chain.emails).list)
        list.ids = applyQueryChanges(list.ids, qr.removed, qr.added)
        list.queryState = qr.newQueryState
        if (qr.total !== undefined) list.total = qr.total
      }
    }
    return mr.hasMoreChanges || er.hasMoreChanges || tr.hasMoreChanges
  }

  // reloadAll starts over after cannotCalculateChanges: the mailboxes, the
  // list, and every email and thread we hold, since we cannot know which
  // of them changed. What the server no longer has is dropped.
  private async reloadAll(): Promise<void> {
    const { state } = this
    await this.load()
    const emailIds = Object.keys(state.emails)
    const threadIds = Object.keys(state.threads)
    for (let i = 0; i < Math.max(emailIds.length, threadIds.length); i += 500) {
      const b = this.client.batch()
      const e = b.call('Email/get', { ids: emailIds.slice(i, i + 500), properties: LIST_PROPS })
      const t = b.call('Thread/get', { ids: threadIds.slice(i, i + 500) })
      const r = await b.send()
      this.mergeEmails(r.get(e).list)
      this.mergeThreads(r.get(t).list)
      for (const id of r.get(e).notFound) this.dropEmail(id)
      for (const id of r.get(t).notFound) delete state.threads[id]
    }
    void this.reloadList()
  }

  private dropEmail(id: Id): void {
    const e = this.state.emails[id]
    if (!e) return
    delete this.state.emails[id]
    const t = this.state.threads[e.threadId]
    if (t) t.emailIds = t.emailIds.filter((x) => x !== id)
  }

  // --- actions on emails ---

  // change applies changes to emails here, sends them, and puts back what
  // the server refused. It resolves to whether all of it was accepted.
  change(changes: Map<Id, Change>): Promise<boolean> {
    const { state } = this
    const before = new Map<Id, { email: Email; index: number }>()
    const update: Record<Id, EmailPatch> = {}
    const destroy: Id[] = []
    const rowOfThread = new Map<Id, Id>()
    for (const id of state.list.ids) {
      const e = state.emails[id]
      if (e) rowOfThread.set(e.threadId, id)
    }

    this.epoch++
    for (const [id, c] of changes) {
      const email = state.emails[id]
      if (!email) continue
      const thread = state.threads[email.threadId]
      before.set(id, { email, index: thread ? thread.emailIds.indexOf(id) : -1 })
      if (c === 'destroy') {
        adjustCounts(state.mailboxes, email, null)
        this.dropEmail(id)
        destroy.push(id)
      } else {
        const after = applyPatch(email, c)
        adjustCounts(state.mailboxes, email, after)
        state.emails[id] = after
        update[id] = c
      }
    }
    if (before.size === 0) return Promise.resolve(true)
    this.fixList(rowOfThread, new Set([...before.values()].map((b) => b.email.threadId)))

    const rollback = (ids: Iterable<Id>) => {
      this.epoch++
      for (const id of ids) {
        const b = before.get(id)
        if (!b) continue
        adjustCounts(state.mailboxes, state.emails[id] ?? null, b.email)
        state.emails[id] = b.email
        const t = state.threads[b.email.threadId]
        if (t && !t.emailIds.includes(id)) t.emailIds.splice(b.index < 0 ? t.emailIds.length : b.index, 0, id)
      }
      void this.reloadList()
    }

    this.pending++
    return this.serial(async () => {
      try {
        const b = this.client.batch()
        const set = b.call('Email/set', {
          ...(Object.keys(update).length ? { update } : {}),
          ...(destroy.length ? { destroy } : {}),
        })
        const r = (await b.send()).get(set)
        const failed: Record<Id, SetError> = { ...r.notUpdated, ...r.notDestroyed }
        // "notFound" for a destroy: someone else was first. That is what we wanted.
        for (const id of destroy) if (failed[id]?.type === 'notFound') delete failed[id]
        const ids = Object.keys(failed)
        if (ids.length === 0) return true
        rollback(ids)
        const first = failed[ids[0]!]!
        this.onError(`The server refused the change: ${first.description || first.type}`)
        return false
      } catch (e) {
        rollback(before.keys())
        if (!(e instanceof HttpError && e.status === 401)) {
          this.onError(isMethodError(e) ? `The server refused the change: ${e.message}` : 'The change could not be sent. It was put back.')
        }
        return false
      } finally {
        this.pending--
        void this.refresh()
      }
    })
  }

  // fixList keeps the list in step with a local change, for the mailbox
  // of the list: a thread with no email left in it loses its row, and a
  // row whose email left gets the newest one that stayed. The other
  // conditions of a filter are left to the server's /queryChanges.
  private fixList(rowOfThread: Map<Id, Id>, threads: Set<Id>): void {
    const list = this.state.list
    if (!list.spec) return
    const mailbox = list.spec.mailboxId
    for (const threadId of threads) {
      const row = rowOfThread.get(threadId)
      if (row === undefined) continue
      const at = list.ids.indexOf(row)
      if (at === -1) continue
      const left = this.threadEmails(threadId).filter((e) => e.mailboxIds[mailbox])
      if (left.length === 0) {
        list.ids.splice(at, 1)
        list.total = Math.max(0, list.total - 1)
      } else if (!left.some((e) => e.id === row)) {
        list.ids[at] = left.reduce((a, e) => (e.receivedAt >= a.receivedAt ? e : a)).id
      }
    }
  }

  private emails(ids: Id[]): Email[] {
    return ids.map((id) => this.state.emails[id]).filter((e): e is Email => !!e)
  }

  setKeyword(ids: Id[], keyword: string, on: boolean): Promise<boolean> {
    const changes = new Map<Id, Change>()
    for (const e of this.emails(ids)) {
      if (!!e.keywords[keyword] !== on) changes.set(e.id, { [`keywords/${keyword}`]: on ? true : null })
    }
    return this.change(changes)
  }

  // move takes emails out of one mailbox and puts them in another.
  move(ids: Id[], from: Id, to: Id): Promise<boolean> {
    const changes = new Map<Id, Change>()
    if (from === to) return Promise.resolve(true)
    for (const e of this.emails(ids)) {
      if (e.mailboxIds[from]) changes.set(e.id, { [`mailboxIds/${from}`]: null, [`mailboxIds/${to}`]: true })
    }
    return this.change(changes)
  }

  // label adds emails to a mailbox or takes them out. An email stays in
  // at least one mailbox.
  label(ids: Id[], mailboxId: Id, on: boolean): Promise<boolean> {
    const changes = new Map<Id, Change>()
    for (const e of this.emails(ids)) {
      if (!!e.mailboxIds[mailboxId] === on) continue
      if (!on && Object.keys(e.mailboxIds).length === 1) {
        this.onError('A message has to stay in at least one mailbox.')
        continue
      }
      changes.set(e.id, { [`mailboxIds/${mailboxId}`]: on ? true : null })
    }
    return this.change(changes)
  }

  // remove moves emails to Trash, out of every other mailbox. An email
  // that is in Trash already is destroyed.
  remove(ids: Id[]): Promise<boolean> {
    const trash = this.mailboxByRole('trash')
    const changes = new Map<Id, Change>()
    for (const e of this.emails(ids)) {
      if (!trash || e.mailboxIds[trash.id]) changes.set(e.id, 'destroy')
      else changes.set(e.id, { mailboxIds: { [trash.id]: true } })
    }
    return this.change(changes)
  }

  // --- actions on mailboxes ---

  // mailboxSet sends a Mailbox/set that only holds for the mailboxes as we
  // know them (ifInState), followed by a fresh list of all mailboxes. When
  // the state had moved, the list is new now; if stillValid says the
  // change still makes sense, it is tried once more.
  private mailboxSet(
    args: () => { create?: Record<string, Partial<Mailbox>>; update?: Record<Id, Record<string, unknown>>; destroy?: Id[]; onDestroyRemoveEmails?: boolean },
    stillValid: () => boolean,
  ): Promise<{ created: Id | null }> {
    return this.serial(async () => {
      for (let attempt = 0; ; attempt++) {
        const b = this.client.batch()
        const set = b.call('Mailbox/set', { ifInState: this.client.states.Mailbox, ...args() })
        const get = b.call('Mailbox/get', { ids: null })
        const r = await b.send()
        const all = r.get(get)
        this.setMailboxes(all.list)
        this.client.setState('Mailbox', all.state)
        const err = r.error(set)
        if (err) {
          if (err.type === 'stateMismatch' && attempt === 0 && stillValid()) continue
          if (err.type === 'stateMismatch') throw new Error('The mailboxes were changed somewhere else. Check them and try again.')
          throw err
        }
        const sr = r.get(set)
        const failed = { ...sr.notCreated, ...sr.notUpdated, ...sr.notDestroyed }
        const first = Object.values(failed)[0]
        if (first) throw new Error(mailboxErrorText(first))
        void this.refresh()
        return { created: Object.values(sr.created ?? {})[0]?.id ?? null }
      }
    })
  }

  async createMailbox(name: string, parentId: Id | null): Promise<Id | null> {
    const r = await this.mailboxSet(
      () => ({ create: { new: { name, parentId } } }),
      () => parentId === null || !!this.state.mailboxes[parentId],
    )
    return r.created
  }

  async renameMailbox(id: Id, name: string): Promise<void> {
    const was = this.state.mailboxes[id]?.name
    await this.mailboxSet(
      () => ({ update: { [id]: { name } } }),
      () => this.state.mailboxes[id]?.name === was,
    )
  }

  // destroyMailbox removes a mailbox. With removeEmails its emails go too:
  // those that are in no other mailbox are destroyed.
  async destroyMailbox(id: Id, removeEmails: boolean): Promise<void> {
    const was = this.state.mailboxes[id]?.totalEmails
    await this.mailboxSet(
      () => ({ destroy: [id], onDestroyRemoveEmails: removeEmails }),
      // Not when mail arrived in it meanwhile: the user confirmed a count.
      () => this.state.mailboxes[id]?.totalEmails === was,
    )
  }
}

function mailboxErrorText(e: SetError): string {
  switch (e.type) {
    case 'mailboxHasChild':
      return 'This mailbox has mailboxes inside it. Remove those first.'
    case 'mailboxHasEmail':
      return 'This mailbox still has mail in it.'
    case 'forbidden':
      return 'This mailbox cannot be changed.'
    case 'notFound':
      return 'This mailbox no longer exists.'
    default:
      return e.description || e.type
  }
}
