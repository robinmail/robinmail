// A JMAP server in memory, for the unit tests: as much of robinmail's
// behaviour as the client relies on. One modseq is the state of every
// type, as on the real server.

import type { Uploader } from '../jmap/client'
import type { Email, EmailSubmission, Filter, FilterCondition, Id, Identity, Invocation, Mailbox, SetError } from '../jmap/types'

type Args = Record<string, any>

interface Change {
  modseq: number
  type: 'Email' | 'Mailbox' | 'Thread' | 'EmailSubmission' | 'Identity'
  id: Id
  op: 'created' | 'updated' | 'destroyed'
}

export class FakeServer {
  accountId = 'acct1'
  modseq = 1
  // States at or below this cannot be used for /changes any more.
  trimmed = 0
  maxChanges = 1000
  mailboxes = new Map<Id, { id: Id; name: string; parentId: Id | null; role: string | null; sortOrder: number }>()
  emails = new Map<Id, Email>()
  log: Change[] = []
  // Every request body, and every method call, the server got.
  requests: Invocation[][] = []
  // Set to answer the next requests with this HTTP status.
  httpStatus = 0
  // May answer a call itself: return a response invocation, or null.
  intercept: (name: string, args: Args) => [string, Args] | null = () => null
  // Held requests wait until release() is called.
  private gate: Promise<void> | null = null
  private open: (() => void) | null = null
  // What Email/set create was given, by the id of the Email it made.
  created = new Map<Id, Args>()
  // Blob ids that exist: uploads, messages and their parts.
  blobs = new Set<Id>()
  identities = new Map<Id, Identity>()
  submissions = new Map<Id, EmailSubmission>()
  // Set to refuse the next Email/set create or EmailSubmission/set create.
  failCreate: SetError | null = null
  failSend: SetError | null = null
  // Set to answer uploads with this HTTP status.
  uploadStatus = 0
  maxSizeUpload = 50_000_000
  uploads: { type: string; size: number }[] = []
  private nextEmail = 1
  private nextMailbox = 1
  private nextBlob = 1
  private nextSubmission = 1
  private nextIdentity = 2
  // "#creation id" to id, within one request.
  private creations = new Map<string, Id>()
  // Responses a call adds after its own (an implied call).
  private implied: [string, Args][] = []

  constructor() {
    for (const role of ['inbox', 'drafts', 'sent', 'junk', 'trash']) {
      this.addMailbox(role[0]!.toUpperCase() + role.slice(1), null, role)
    }
    this.identities.set('i1', {
      id: 'i1', name: '', email: 'dev@robinmail.test', replyTo: null, bcc: null, textSignature: '', htmlSignature: '', mayDelete: false,
    })
    this.log = []
  }

  get calls(): Invocation[] {
    return this.requests.flat()
  }

  hold(): void {
    this.gate = new Promise((resolve) => (this.open = resolve))
  }

  release(): void {
    this.open?.()
    this.gate = null
  }

  private bump(): number {
    return ++this.modseq
  }

  private note(type: Change['type'], id: Id, op: Change['op']): void {
    this.log.push({ modseq: this.modseq, type, id, op })
  }

  addMailbox(name: string, parentId: Id | null = null, role: string | null = null): Id {
    const id = `m${this.nextMailbox++}`
    this.bump()
    this.mailboxes.set(id, { id, name, parentId, role, sortOrder: 0 })
    this.note('Mailbox', id, 'created')
    return id
  }

  // deliver stores a new email, as a delivery from outside does.
  deliver(e: { mailbox?: Id; thread?: Id; subject?: string; from?: string; seen?: boolean; receivedAt?: string } & Omit<Partial<Email>, 'from' | 'subject' | 'receivedAt'> = {}): Email {
    const id = `e${this.nextEmail++}`
    const threadId = e.thread ?? `t${id.slice(1)}`
    const isNewThread = ![...this.emails.values()].some((x) => x.threadId === threadId)
    this.bump()
    const email: Email = {
      id,
      blobId: `blob-${id}`,
      threadId,
      mailboxIds: { [e.mailbox ?? 'm1']: true },
      keywords: e.seen ? { $seen: true } : {},
      size: 100,
      receivedAt: e.receivedAt ?? new Date(Date.UTC(2026, 0, 1, 0, 0, this.nextEmail)).toISOString(),
      hasAttachment: false,
      preview: 'preview',
      from: [{ name: null, email: e.from ?? 'a@example.com' }],
      to: [{ name: null, email: 'dev@robinmail.test' }],
      subject: e.subject ?? `Subject ${id}`,
      messageId: [`${id}@fake`],
      ...(e.preview ? { preview: e.preview } : {}),
      ...(e.to ? { to: e.to } : {}),
      ...(e.cc ? { cc: e.cc } : {}),
      ...(e.keywords ? { keywords: e.keywords } : {}),
      ...(e.mailboxIds ? { mailboxIds: e.mailboxIds } : {}),
      ...(e.inReplyTo ? { inReplyTo: e.inReplyTo } : {}),
      ...(e.references ? { references: e.references } : {}),
    }
    this.blobs.add(email.blobId)
    this.emails.set(id, email)
    this.note('Email', id, 'created')
    this.note('Thread', threadId, isNewThread ? 'created' : 'updated')
    for (const m of Object.keys(email.mailboxIds)) this.note('Mailbox', m, 'updated')
    return email
  }

  // touch changes an email as another client would.
  touch(id: Id, f: (e: Email) => void): void {
    const e = this.emails.get(id)!
    this.bump()
    const mailboxes = new Set(Object.keys(e.mailboxIds))
    f(e)
    for (const m of Object.keys(e.mailboxIds)) mailboxes.add(m)
    this.note('Email', id, 'updated')
    this.note('Thread', e.threadId, 'updated')
    for (const m of mailboxes) this.note('Mailbox', m, 'updated')
  }

  private destroyEmail(id: Id): void {
    const e = this.emails.get(id)!
    this.bump()
    this.emails.delete(id)
    this.note('Email', id, 'destroyed')
    const left = [...this.emails.values()].some((x) => x.threadId === e.threadId)
    this.note('Thread', e.threadId, left ? 'updated' : 'destroyed')
    for (const m of Object.keys(e.mailboxIds)) this.note('Mailbox', m, 'updated')
  }

  mailbox(id: Id): Mailbox {
    const m = this.mailboxes.get(id)!
    const emails = [...this.emails.values()].filter((e) => e.mailboxIds[id])
    const unread = emails.filter((e) => !e.keywords.$seen)
    return {
      ...m,
      totalEmails: emails.length,
      unreadEmails: unread.length,
      totalThreads: new Set(emails.map((e) => e.threadId)).size,
      unreadThreads: new Set(unread.map((e) => e.threadId)).size,
      myRights: {
        mayReadItems: true, mayAddItems: true, mayRemoveItems: true, maySetSeen: true, maySetKeywords: true,
        mayCreateChild: true, mayRename: true, mayDelete: m.role === null, maySubmit: true,
      },
      isSubscribed: true,
    }
  }

  // uploader stands in for the browser's upload (see JmapClient).
  uploader: Uploader = async (path, file, type, opts) => {
    if (this.gate) await this.gate
    if (opts.signal?.aborted) throw new DOMException('aborted', 'AbortError')
    if (this.uploadStatus) return { status: this.uploadStatus, body: '{}' }
    if (path !== `/jmap/upload/${this.accountId}/`) return { status: 404, body: '' }
    opts.onProgress?.(1)
    const blobId = `upload${this.nextBlob++}`
    this.blobs.add(blobId)
    this.uploads.push({ type, size: file.size })
    return { status: 201, body: JSON.stringify({ accountId: this.accountId, blobId, type, size: file.size }) }
  }

  // fetch stands in for the browser's fetch.
  fetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const path = String(input)
    if (this.gate) await this.gate
    if (this.httpStatus) return new Response(JSON.stringify({ error: 'x' }), { status: this.httpStatus })
    if (path === '/.well-known/jmap') {
      return Response.json({
        username: 'dev@robinmail.test',
        apiUrl: 'http://mail.example/jmap/api',
        downloadUrl: 'http://mail.example/jmap/download/{accountId}/{blobId}/{name}?type={type}',
        uploadUrl: 'http://mail.example/jmap/upload/{accountId}/',
        eventSourceUrl: '',
        state: 's',
        capabilities: { 'urn:ietf:params:jmap:core': { maxSizeUpload: this.maxSizeUpload } },
        accounts: { [this.accountId]: { name: 'dev', isPersonal: true, isReadOnly: false, accountCapabilities: {} } },
        primaryAccounts: { 'urn:ietf:params:jmap:mail': this.accountId },
      })
    }
    if (path !== '/jmap/api') return new Response('not found', { status: 404 })
    const body = JSON.parse(String(init?.body)) as { methodCalls: Invocation[] }
    this.requests.push(body.methodCalls)
    const responses: Invocation[] = []
    this.creations.clear()
    for (const [name, rawArgs, callId] of body.methodCalls) {
      let out: [string, Args]
      try {
        const args = this.resolve(rawArgs, responses)
        out = this.intercept(name, args) ?? this.method(name, args)
      } catch (e) {
        if (!(e instanceof Fail)) throw e
        out = ['error', { type: e.type }]
      }
      responses.push([out[0], out[1], callId])
      for (const extra of this.implied.splice(0)) responses.push([extra[0], extra[1], callId])
    }
    return Response.json({ methodResponses: responses, sessionState: 's' })
  }

  // resolve replaces "#name" arguments by the part of an earlier response
  // they point at (RFC 8620 3.7).
  private resolve(args: Args, responses: Invocation[]): Args {
    const out: Args = {}
    for (const [k, v] of Object.entries(args)) {
      if (!k.startsWith('#')) {
        out[k] = v
        continue
      }
      const ref = v as { resultOf: string; name: string; path: string }
      const r = responses.find((x) => x[2] === ref.resultOf && x[0] === ref.name)
      if (!r) throw new Fail('invalidResultReference')
      out[k.slice(1)] = pointer(r[1], ref.path.split('/').slice(1))
    }
    return out
  }

  private changes(type: Change['type'], since: string): [string, Args] {
    const from = Number(since)
    if (!Number.isInteger(from) || from < this.trimmed || from > this.modseq) throw new Fail('cannotCalculateChanges')
    const all = this.log.filter((c) => c.type === type && c.modseq > from)
    // Whole modseqs only, at most maxChanges of them.
    const seqs = [...new Set(all.map((c) => c.modseq))].sort((a, b) => a - b)
    const upTo = seqs.length > this.maxChanges ? seqs[this.maxChanges - 1]! : this.modseq
    const last = new Map<Id, Change['op'][]>()
    for (const c of all) if (c.modseq <= upTo) last.set(c.id, [...(last.get(c.id) ?? []), c.op])
    const created: Id[] = []
    const updated: Id[] = []
    const destroyed: Id[] = []
    for (const [id, ops] of last) {
      const born = ops.includes('created')
      const died = ops.includes('destroyed')
      if (born && died) continue
      if (died) destroyed.push(id)
      else if (born) created.push(id)
      else updated.push(id)
    }
    return [`${type}/changes`, {
      accountId: this.accountId, oldState: since, newState: String(upTo), hasMoreChanges: upTo < this.modseq,
      created, updated, destroyed, ...(type === 'Mailbox' ? { updatedProperties: null } : {}),
    }]
  }

  private query(args: Args): Id[] {
    const match = (e: Email, f: Filter | null | undefined): boolean => {
      if (!f) return true
      if ('operator' in f) {
        if (f.operator === 'AND') return f.conditions.every((c) => match(e, c))
        if (f.operator === 'OR') return f.conditions.some((c) => match(e, c))
        return !f.conditions.some((c) => match(e, c))
      }
      const c = f as FilterCondition
      if (c.inMailbox !== undefined && !e.mailboxIds[c.inMailbox]) return false
      if (c.hasKeyword !== undefined && !e.keywords[c.hasKeyword]) return false
      if (c.notKeyword !== undefined && e.keywords[c.notKeyword]) return false
      if (c.subject !== undefined && !(e.subject ?? '').toLowerCase().includes(c.subject.toLowerCase())) return false
      if (c.from !== undefined && !(e.from ?? []).some((a) => a.email.includes(c.from!))) return false
      if (c.hasAttachment !== undefined && e.hasAttachment !== c.hasAttachment) return false
      // Whole words, all required, in the subject or the preview.
      if (c.text !== undefined && !words(c.text).every((w) => words(`${e.subject ?? ''} ${e.preview}`).includes(w))) return false
      return true
    }
    const sorted = [...this.emails.values()]
      .filter((e) => match(e, args.filter))
      .sort((a, b) => (a.receivedAt < b.receivedAt ? 1 : a.receivedAt > b.receivedAt ? -1 : Number(b.id.slice(1)) - Number(a.id.slice(1))))
    if (!args.collapseThreads) return sorted.map((e) => e.id)
    const seen = new Set<Id>()
    return sorted.filter((e) => !seen.has(e.threadId) && seen.add(e.threadId)).map((e) => e.id)
  }

  private method(name: string, args: Args): [string, Args] {
    const accountId = this.accountId
    const state = String(this.modseq)
    switch (name) {
      case 'Mailbox/get': {
        const ids: Id[] = args.ids ?? [...this.mailboxes.keys()]
        return [name, {
          accountId, state,
          list: ids.filter((id) => this.mailboxes.has(id)).map((id) => this.mailbox(id)),
          notFound: ids.filter((id) => !this.mailboxes.has(id)),
        }]
      }
      case 'Mailbox/changes':
        return this.changes('Mailbox', args.sinceState)
      case 'Email/changes':
        return this.changes('Email', args.sinceState)
      case 'Thread/changes':
        return this.changes('Thread', args.sinceState)
      case 'Thread/get': {
        const ids: Id[] = [...new Set(args.ids as Id[])]
        const list = ids
          .map((id) => ({
            id,
            emailIds: [...this.emails.values()].filter((e) => e.threadId === id)
              .sort((a, b) => (a.receivedAt < b.receivedAt ? -1 : 1)).map((e) => e.id),
          }))
          .filter((t) => t.emailIds.length > 0)
        return [name, { accountId, state, list, notFound: ids.filter((id) => !list.some((t) => t.id === id)) }]
      }
      case 'Email/get': {
        if (args.ids == null) throw new Fail('requestTooLarge')
        const ids: Id[] = [...new Set(args.ids as Id[])]
        const props: string[] | null = args.properties ?? null
        const list = ids.filter((id) => this.emails.has(id)).map((id) => {
          const e = structuredClone(this.emails.get(id)!) as unknown as Record<string, unknown>
          const made = this.created.get(id)
          if (made && (!props || props.some((p) => ['bodyValues', 'attachments', 'textBody', 'htmlBody'].includes(p)))) {
            // As the server builds it: the text, the HTML, then the attachments.
            const part = (n: number, type: string) => ({ partId: String(n), blobId: `${e.blobId}-${n}`, size: 1, name: null, type, disposition: null, cid: null })
            e.textBody = [part(1, 'text/plain')]
            e.htmlBody = made.htmlBody ? [part(2, 'text/html')] : e.textBody
            const first = made.htmlBody ? 3 : 2
            e.attachments = ((made.attachments ?? []) as Args[]).map((a, i) => ({
              partId: String(first + i), blobId: `${e.blobId}-${first + i}`, size: 10, name: a.name ?? null, type: a.type,
              disposition: a.disposition ?? null, cid: a.cid ?? null,
            }))
            e.bodyValues = {
              1: { value: made.bodyValues[made.textBody[0].partId].value, isEncodingProblem: false, isTruncated: false },
              ...(made.htmlBody ? { 2: { value: made.bodyValues[made.htmlBody[0].partId].value, isEncodingProblem: false, isTruncated: false } } : {}),
            }
            for (const [k, v] of Object.entries(made)) if (k.startsWith('header:')) e[k] = v
          } else if (props?.includes('bodyValues')) {
            e.textBody = [{ partId: '1', blobId: `${e.blobId}-1`, size: 4, name: null, type: 'text/plain', disposition: null, cid: null }]
            e.htmlBody = e.textBody
            e.attachments = []
            e.bodyValues = { 1: { value: `body of ${id}`, isEncodingProblem: false, isTruncated: false } }
          }
          if (!props) return e
          return Object.fromEntries([
            ...props.filter((p) => p.startsWith('header:')).map((p) => [p, null]),
            ...Object.entries(e).filter(([k]) => k === 'id' || props.includes(k)),
          ])
        })
        return [name, { accountId, state, list, notFound: ids.filter((id) => !this.emails.has(id)) }]
      }
      case 'Email/query': {
        const all = this.query(args)
        const position: number = args.position ?? 0
        return [name, {
          accountId, queryState: state, canCalculateChanges: true, position,
          ids: all.slice(position, position + (args.limit ?? 500)),
          ...(args.calculateTotal ? { total: all.length } : {}),
        }]
      }
      case 'SearchSnippet/get': {
        const want = words((args.filter as FilterCondition | null)?.text ?? '')
        const mark = (s: string) => {
          let hit = false
          const out = s.replace(/[<>&]|[\p{L}\p{N}]+/gu, (m) => {
            if (m === '<') return '&lt;'
            if (m === '>') return '&gt;'
            if (m === '&') return '&amp;'
            if (!want.includes(m.toLowerCase())) return m
            hit = true
            return `<mark>${m}</mark>`
          })
          return hit ? out : null
        }
        const ids: Id[] = args.emailIds ?? []
        const notFound = ids.filter((id) => !this.emails.has(id))
        return [name, {
          accountId,
          list: ids.filter((id) => this.emails.has(id)).map((id) => {
            const e = this.emails.get(id)!
            return { emailId: id, subject: mark(e.subject ?? ''), preview: mark(e.preview) }
          }),
          notFound: notFound.length ? notFound : null,
        }]
      }
      case 'Email/queryChanges': {
        const from = Number(args.sinceQueryState)
        if (!Number.isInteger(from) || from < this.trimmed) throw new Fail('cannotCalculateChanges')
        // As robinmail does: every email that changed, and every email of
        // a thread that changed, is removed and, if it is a result now,
        // added at its index.
        const threads = new Set(this.log.filter((c) => c.type === 'Thread' && c.modseq > from).map((c) => c.id))
        const changed = new Set(this.log.filter((c) => c.type === 'Email' && c.modseq > from).map((c) => c.id))
        for (const e of this.emails.values()) if (threads.has(e.threadId)) changed.add(e.id)
        const now = this.query(args)
        return [name, {
          accountId, oldQueryState: args.sinceQueryState, newQueryState: state,
          ...(args.calculateTotal ? { total: now.length } : {}),
          removed: [...changed],
          added: now.map((id, index) => ({ id, index })).filter((a) => changed.has(a.id)),
        }]
      }
      case 'Email/set': {
        if (args.ifInState != null && args.ifInState !== state) throw new Fail('stateMismatch')
        const updated: Args = {}
        const notUpdated: Args = {}
        const destroyed: Id[] = []
        const notDestroyed: Args = {}
        const created: Args = {}
        const notCreated: Args = {}
        for (const [cid, raw] of Object.entries((args.create ?? {}) as Record<string, Args>)) {
          const lost = ((raw.attachments ?? []) as Args[]).map((a) => a.blobId as Id).filter((b) => !this.blobs.has(b))
          if (this.failCreate) {
            notCreated[cid] = this.failCreate
            this.failCreate = null
            continue
          }
          if (lost.length) {
            notCreated[cid] = { type: 'blobNotFound', notFound: lost }
            continue
          }
          const id = `e${this.nextEmail++}`
          // A reply joins the thread of the message it answers.
          const refs: string[] = [...(raw.inReplyTo ?? []), ...(raw.references ?? [])]
          const parent = [...this.emails.values()].find((x) => (x.messageId ?? []).some((m) => refs.includes(m)))
          const threadId = parent?.threadId ?? `t${id.slice(1)}`
          this.bump()
          const email: Email = {
            id, blobId: `blob-${id}`, threadId, mailboxIds: { ...raw.mailboxIds }, keywords: { ...raw.keywords }, size: 100,
            receivedAt: new Date(Date.UTC(2026, 0, 2, 0, 0, this.nextEmail)).toISOString(), hasAttachment: (raw.attachments ?? []).length > 0,
            preview: String(raw.bodyValues?.[raw.textBody?.[0]?.partId]?.value ?? '').slice(0, 100),
            from: raw.from ?? null, to: raw.to ?? null, cc: raw.cc ?? null, bcc: raw.bcc ?? null, subject: raw.subject ?? null,
            messageId: [`${id}@fake`], inReplyTo: raw.inReplyTo ?? null, references: raw.references ?? null,
          }
          this.emails.set(id, email)
          this.created.set(id, raw)
          this.blobs.add(email.blobId)
          ;((raw.attachments ?? []) as Args[]).forEach((_, i) => this.blobs.add(`${email.blobId}-${(raw.htmlBody ? 3 : 2) + i}`))
          this.note('Email', id, 'created')
          this.note('Thread', threadId, parent ? 'updated' : 'created')
          for (const m of Object.keys(email.mailboxIds)) this.note('Mailbox', m, 'updated')
          this.creations.set(cid, id)
          created[cid] = { id, blobId: email.blobId, threadId, size: 100 }
        }
        for (const [key, patch] of Object.entries((args.update ?? {}) as Record<Id, Args>)) {
          const id = key.startsWith('#') ? this.creations.get(key.slice(1)) ?? key : key
          const e = this.emails.get(id)
          if (!e) {
            notUpdated[id] = { type: 'notFound' }
            continue
          }
          const next = structuredClone(e)
          for (const [path, value] of Object.entries(patch)) {
            const [prop, key] = path.split('/') as ['keywords' | 'mailboxIds', string | undefined]
            if (key === undefined) next[prop] = value
            else if (value === null) delete next[prop][key]
            else next[prop][key] = true
          }
          const boxes = Object.keys(next.mailboxIds)
          if (boxes.length === 0 || boxes.some((m) => !this.mailboxes.has(m))) {
            notUpdated[id] = { type: 'invalidProperties', properties: ['mailboxIds'] }
            continue
          }
          this.touch(id, (x) => Object.assign(x, next))
          updated[id] = null
        }
        for (const id of (args.destroy ?? []) as Id[]) {
          if (!this.emails.has(id)) {
            notDestroyed[id] = { type: 'notFound' }
            continue
          }
          const e = this.emails.get(id)!
          for (const b of [...this.blobs]) if (b === e.blobId || b.startsWith(`${e.blobId}-`)) this.blobs.delete(b)
          this.destroyEmail(id)
          destroyed.push(id)
        }
        return [name, {
          accountId, oldState: state, newState: String(this.modseq), created: Object.keys(created).length ? created : null,
          updated, destroyed, notCreated: Object.keys(notCreated).length ? notCreated : null, notUpdated, notDestroyed,
        }]
      }
      case 'Mailbox/set': {
        if (args.ifInState != null && args.ifInState !== state) throw new Fail('stateMismatch')
        const created: Args = {}
        const updated: Args = {}
        const notUpdated: Args = {}
        const destroyed: Id[] = []
        const notDestroyed: Args = {}
        for (const [cid, m] of Object.entries((args.create ?? {}) as Record<string, Args>)) {
          created[cid] = { id: this.addMailbox(m.name, m.parentId ?? null) }
        }
        for (const [id, patch] of Object.entries((args.update ?? {}) as Record<Id, Args>)) {
          const m = this.mailboxes.get(id)
          if (!m) {
            notUpdated[id] = { type: 'notFound' }
            continue
          }
          this.bump()
          Object.assign(m, patch)
          this.note('Mailbox', id, 'updated')
          updated[id] = null
        }
        for (const id of (args.destroy ?? []) as Id[]) {
          const m = this.mailboxes.get(id)
          const emails = [...this.emails.values()].filter((e) => e.mailboxIds[id])
          if (!m) notDestroyed[id] = { type: 'notFound' }
          else if (m.role) notDestroyed[id] = { type: 'forbidden' }
          else if (emails.length && !args.onDestroyRemoveEmails) notDestroyed[id] = { type: 'mailboxHasEmail' }
          else {
            for (const e of emails) {
              if (Object.keys(e.mailboxIds).length === 1) this.destroyEmail(e.id)
              else this.touch(e.id, (x) => delete x.mailboxIds[id])
            }
            this.bump()
            this.mailboxes.delete(id)
            this.note('Mailbox', id, 'destroyed')
            destroyed.push(id)
          }
        }
        return [name, {
          accountId, oldState: state, newState: String(this.modseq),
          created, updated, destroyed, notCreated: null, notUpdated, notDestroyed,
        }]
      }
      case 'Identity/get': {
        const ids: Id[] = args.ids ?? [...this.identities.keys()]
        return [name, {
          accountId, state, list: ids.filter((id) => this.identities.has(id)).map((id) => structuredClone(this.identities.get(id)!)),
          notFound: ids.filter((id) => !this.identities.has(id)),
        }]
      }
      case 'Identity/set': {
        const created: Args = {}
        const notCreated: Args = {}
        const updated: Args = {}
        const notUpdated: Args = {}
        const destroyed: Id[] = []
        const notDestroyed: Args = {}
        for (const [cid, raw] of Object.entries((args.create ?? {}) as Record<string, Args>)) {
          if (!/^dev(\+[^@]+)?@robinmail\.test$/.test(raw.email ?? '')) {
            notCreated[cid] = { type: 'forbiddenFrom' }
            continue
          }
          const id = `i${this.nextIdentity++}`
          this.bump()
          this.identities.set(id, { id, name: '', replyTo: null, bcc: null, textSignature: '', htmlSignature: '', mayDelete: true, ...raw } as Identity)
          created[cid] = { id, mayDelete: true }
        }
        for (const [id, patch] of Object.entries((args.update ?? {}) as Record<Id, Args>)) {
          const i = this.identities.get(id)
          if (!i) notUpdated[id] = { type: 'notFound' }
          else {
            this.bump()
            Object.assign(i, patch)
            updated[id] = null
          }
        }
        for (const id of (args.destroy ?? []) as Id[]) {
          if (!this.identities.has(id)) notDestroyed[id] = { type: 'notFound' }
          else if (id === 'i1') notDestroyed[id] = { type: 'forbidden' }
          else {
            this.bump()
            this.identities.delete(id)
            destroyed.push(id)
          }
        }
        return [name, { accountId, oldState: state, newState: String(this.modseq), created, updated, destroyed, notCreated, notUpdated, notDestroyed }]
      }
      case 'EmailSubmission/get': {
        const ids: Id[] = args.ids ?? [...this.submissions.keys()]
        return [name, {
          accountId, state, list: ids.filter((id) => this.submissions.has(id)).map((id) => structuredClone(this.submissions.get(id)!)),
          notFound: ids.filter((id) => !this.submissions.has(id)),
        }]
      }
      case 'EmailSubmission/changes':
        return this.changes('EmailSubmission', args.sinceState)
      case 'EmailSubmission/query': {
        const want: Id[] | undefined = args.filter?.emailIds
        const ids = [...this.submissions.values()].filter((s) => !want || want.includes(s.emailId)).map((s) => s.id)
        return [name, { accountId, queryState: state, canCalculateChanges: false, position: 0, ids }]
      }
      case 'EmailSubmission/set': {
        const created: Args = {}
        const notCreated: Args = {}
        const updated: Args = {}
        const notUpdated: Args = {}
        const emailOf = new Map<string, Id>()
        for (const [cid, raw] of Object.entries((args.create ?? {}) as Record<string, Args>)) {
          const ref = String(raw.emailId)
          const emailId = ref.startsWith('#') ? this.creations.get(ref.slice(1)) : ref
          const email = emailId ? this.emails.get(emailId) : undefined
          if (!email || !this.identities.has(raw.identityId)) {
            notCreated[cid] = { type: 'invalidProperties', properties: [email ? 'identityId' : 'emailId'] }
            continue
          }
          if (this.failSend) {
            notCreated[cid] = this.failSend
            this.failSend = null
            continue
          }
          const rcpts = [...(email.to ?? []), ...(email.cc ?? []), ...(email.bcc ?? [])].map((a) => a.email)
          if (rcpts.length === 0) {
            notCreated[cid] = { type: 'noRecipients' }
            continue
          }
          const id = `s${this.nextSubmission++}`
          this.bump()
          const sub: EmailSubmission = {
            id, identityId: raw.identityId, emailId: email.id, threadId: email.threadId, sendAt: new Date(Date.now() + 10_000).toISOString(),
            undoStatus: 'pending', deliveryStatus: Object.fromEntries(rcpts.map((r) => [r, { smtpReply: '', delivered: 'queued', displayed: 'unknown' }])),
          }
          this.submissions.set(id, sub)
          this.note('EmailSubmission', id, 'created')
          emailOf.set(`#${cid}`, email.id)
          created[cid] = { id, threadId: sub.threadId, sendAt: sub.sendAt, undoStatus: sub.undoStatus, deliveryStatus: sub.deliveryStatus, dsnBlobIds: [], mdnBlobIds: [] }
        }
        for (const [id, patch] of Object.entries((args.update ?? {}) as Record<Id, Args>)) {
          const sub = this.submissions.get(id)
          if (!sub) notUpdated[id] = { type: 'notFound' }
          else if (patch.undoStatus !== 'canceled') notUpdated[id] = { type: 'invalidProperties' }
          else if (sub.undoStatus !== 'pending') notUpdated[id] = { type: 'cannotUnsend' }
          else {
            this.bump()
            sub.undoStatus = 'canceled'
            this.note('EmailSubmission', id, 'updated')
            updated[id] = null
          }
        }
        const response: [string, Args] = [name, {
          accountId, oldState: state, newState: String(this.modseq), created: Object.keys(created).length ? created : null,
          updated, destroyed: [], notCreated: Object.keys(notCreated).length ? notCreated : null, notUpdated, notDestroyed: null,
        }]
        // onSuccess*: an Email/set of its own, answered after this call.
        const update: Args = {}
        for (const [key, patch] of Object.entries((args.onSuccessUpdateEmail ?? {}) as Record<string, Args>)) {
          const emailId = emailOf.get(key)
          if (emailId) update[emailId] = patch
        }
        const destroy = ((args.onSuccessDestroyEmail ?? []) as string[]).map((k) => emailOf.get(k)).filter((x): x is Id => !!x)
        if (Object.keys(update).length || destroy.length) this.implied.push(this.method('Email/set', { update, destroy }))
        return response
      }
    }
    throw new Fail('unknownMethod')
  }
}

class Fail extends Error {
  constructor(public type: string) {
    super(type)
  }
}

// pointer evaluates a JSON pointer in which "*" maps over an array and
// flattens the result (RFC 8620 3.7).
function pointer(value: any, path: string[]): any {
  if (path.length === 0) return value
  const [head, ...rest] = path as [string, ...string[]]
  if (head === '*') {
    if (!Array.isArray(value)) throw new Fail('invalidResultReference')
    return value.flatMap((v) => pointer(v, rest))
  }
  if (value === null || typeof value !== 'object' || !(head in value)) throw new Fail('invalidResultReference')
  return pointer(value[head], rest)
}

// words are the lower-case words of a text, as a search sees them.
function words(s: string): string[] {
  return s.toLowerCase().match(/[\p{L}\p{N}]+/gu) ?? []
}
