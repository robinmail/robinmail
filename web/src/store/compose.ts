// The messages being written: open composers, their drafts on the server,
// uploads, sending and taking a send back, the identities to send as, and
// the delivery status of what was sent.
//
// What a person typed lives here, in memory, until the server has it. A
// failed save, a lost connection or an expired session changes nothing
// about that: the composer stays, says what is wrong and tries again.

import { reactive } from 'vue'
import type { Batch, Call, JmapClient } from '../jmap/client'
import { HttpError, isMethodError } from '../jmap/client'
import type { Email, EmailAddress, EmailCreate, EmailSubmission, Id, Identity, SetError } from '../jmap/types'
import type { Mailto } from '../compose/address'
import {
  DRAFT_HEADER, buildEmail, emptyDraft, fromStored, isBlank, newCid, setRich, startFrom, swapSignature,
  type Attachment, type Draft, type Kind,
} from '../compose/message'
import { htmlToText, textToHtml } from '../compose/text'
import { BODY_PART_PROPS, BODY_PROPS, type MailStore } from './mail'

export interface Composer {
  key: number
  kind: Kind
  draft: Draft
  minimized: boolean
  // The Email in Drafts that holds this draft, once it was saved.
  savedId: Id | null
  // version counts changes; savedVersion is the one the server has.
  version: number
  savedVersion: number
  saving: boolean
  saveError: string
  sending: boolean
  sendError: string
}

// A message that was sent and can still be taken back.
export interface Sent {
  key: number
  composer: Composer
  submissionId: Id
  emailId: Id
  // When the server lets go of it (ms since the epoch).
  until: number
  // The keyword this send put on the original, to take off on undo.
  marked: { emailId: Id; keyword: string } | null
}

export interface IdentityInput {
  name: string
  replyTo: EmailAddress[]
  textSignature: string
}

interface SendOptions {
  email: EmailCreate
  identityId: Id
  draftsId: Id
  sentId: Id | null
  // The saved draft the new Email replaces.
  replaces: Id | null
  related: Draft['related']
}

// addSendCalls puts a send into a batch: the message is created in
// Drafts (and the draft it replaces destroyed), submitted, and on success
// moved to Sent without $draft by the server (onSuccessUpdateEmail, RFC
// 8621 7.5). Without a Sent mailbox no copy is kept.
//
// onSuccessUpdateEmail can only change the message that was sent. The
// keyword on the message it answers is a third call, which must not run
// when the send failed: it takes "destroy" from the submission that was
// created (its dsnBlobIds, an empty list). When no submission was
// created the reference does not resolve and the server refuses the call
// (invalidResultReference) instead of running it.
export function addSendCalls(b: Batch, o: SendOptions): { set: Call<'Email/set'>; submit: Call<'EmailSubmission/set'>; mark: Call<'Email/set'> | null } {
  const set = b.call('Email/set', { create: { draft: o.email }, ...(o.replaces ? { destroy: [o.replaces] } : {}) })
  const submit = b.call('EmailSubmission/set', {
    create: { send: { identityId: o.identityId, emailId: '#draft' } },
    ...(o.sentId
      ? { onSuccessUpdateEmail: { '#send': { [`mailboxIds/${o.draftsId}`]: null, [`mailboxIds/${o.sentId}`]: true, 'keywords/$draft': null } } }
      : { onSuccessDestroyEmail: ['#send'] }),
  })
  const mark = o.related
    ? b.call('Email/set', {
        update: { [o.related.emailId]: { [`keywords/${o.related.keyword}`]: true } },
        '#destroy': submit.ref('/created/send/dsnBlobIds'),
      })
    : null
  return { set, submit, mark }
}

// sendErrorText says why a submission was refused (RFC 8621 7.5).
export function sendErrorText(e: SetError | undefined): string {
  if (!e) return 'The server did not send the message and did not say why.'
  switch (e.type) {
    case 'forbiddenFrom':
    case 'forbiddenMailFrom':
      return 'You may not send from this address. Choose another address under From.'
    case 'tooManyRecipients':
      return e.maxRecipients
        ? `Too many recipients: the server takes at most ${e.maxRecipients}${/hour/i.test(e.description ?? '') ? ' an hour' : ' for one message'}.`
        : `Too many recipients. ${e.description ?? ''}`.trim()
    case 'invalidRecipients':
      return e.invalidRecipients?.length
        ? `The server refused ${e.invalidRecipients.length === 1 ? 'this address' : 'these addresses'}: ${e.invalidRecipients.join(', ')}`
        : `The server refused a recipient. ${e.description ?? ''}`.trim()
    case 'noRecipients':
      return 'Add at least one recipient.'
    case 'forbiddenToSend':
      return `The server did not accept this message for sending${e.description ? `: ${e.description}` : '.'}`
    case 'tooLarge':
      return e.maxSize ? `The message is too large. The limit is ${Math.floor(e.maxSize / 1_000_000)} MB.` : 'The message is too large.'
    case 'invalidEmail':
      return `The message cannot be sent as it is${e.description ? `: ${e.description}` : '.'}`
    default:
      return `The message was not sent: ${e.description || e.type}`
  }
}

function createErrorText(e: SetError | undefined): string {
  if (!e) return 'The server did not store the message.'
  if (e.type === 'tooLarge') return 'The message with its attachments is too large for the server.'
  if (e.type === 'blobNotFound') return 'An attachment is no longer on the server. Attach it again.'
  return `The server did not store the message: ${e.description || e.type}`
}

export interface ComposeOptions {
  // How long after the last change a draft is saved.
  debounceMs?: number
  retryMs?: number
}

export class ComposeStore {
  readonly state = reactive({
    composers: [] as Composer[],
    identities: [] as Identity[],
    identitiesLoaded: false,
    // The newest submission of an Email, by email id.
    submissions: {} as Record<Id, EmailSubmission>,
    sent: [] as Sent[],
  })

  // Whether a new message starts as rich text. The screen keeps it.
  rich = false
  // Told when something went wrong that no composer can show.
  onError: (message: string) => void = () => {}

  private nextKey = 1
  private owner = ''
  private chains = new Map<number, Promise<unknown>>()
  private timers = new Map<number, ReturnType<typeof setTimeout>>()
  private uploads = new Map<string, { file: Blob; abort: AbortController }>()
  private asked = new Set<Id>()
  private debounceMs: number
  private retryMs: number

  constructor(readonly client: JmapClient, readonly mail: MailStore, opts: ComposeOptions = {}) {
    this.debounceMs = opts.debounceMs ?? 1500
    this.retryMs = opts.retryMs ?? 10_000
  }

  // --- start, after login ---

  // init loads the identities and learns the state of the submissions.
  // After a new login as the same user the composers are still here and
  // their drafts are saved now; another user's are dropped.
  async init(): Promise<void> {
    const user = this.mail.state.username
    if (this.owner && this.owner !== user) this.clear()
    this.owner = user
    const b = this.client.batch()
    const i = b.call('Identity/get', { ids: null })
    const s = b.call('EmailSubmission/get', { ids: [] })
    const r = await b.send()
    this.state.identities = r.get(i).list
    this.state.identitiesLoaded = true
    this.client.setState('EmailSubmission', r.get(s).state)
    for (const c of this.state.composers) {
      if (!c.draft.identityId) c.draft.identityId = this.state.identities[0]?.id ?? ''
      if (c.version !== c.savedVersion) this.schedule(c, 0)
    }
  }

  // clear forgets everything: logout.
  clear(): void {
    for (const t of this.timers.values()) clearTimeout(t)
    this.timers.clear()
    for (const u of this.uploads.values()) u.abort.abort()
    this.uploads.clear()
    this.chains.clear()
    this.asked.clear()
    this.owner = ''
    Object.assign(this.state, { composers: [], identities: [], identitiesLoaded: false, submissions: {}, sent: [] })
  }

  identity(id: Id): Identity | undefined {
    return this.state.identities.find((i) => i.id === id) ?? this.state.identities[0]
  }

  // hasUnsaved: closing the tab now would lose something.
  hasUnsaved(): boolean {
    return this.state.composers.some((c) => c.version !== c.savedVersion || c.saving || c.sending || c.draft.attachments.some((a) => a.state === 'uploading'))
  }

  // --- opening ---

  private add(kind: Kind, draft: Draft, savedId: Id | null = null): Composer {
    const c: Composer = reactive({
      key: this.nextKey++, kind, draft, minimized: false, savedId, version: 0, savedVersion: 0,
      saving: false, saveError: '', sending: false, sendError: '',
    })
    this.state.composers.push(c)
    return this.state.composers.at(-1)!
  }

  focus(c: Composer): void {
    c.minimized = false
  }

  // compose opens an empty message, or one filled from a mailto: link.
  compose(mailto?: Mailto | null): Composer {
    const d = emptyDraft(this.state.identities[0], this.rich)
    const c = this.add('new', d)
    if (mailto) {
      d.to = mailto.to
      d.cc = mailto.cc
      d.bcc = [...d.bcc, ...mailto.bcc]
      d.subject = mailto.subject
      if (mailto.body) {
        if (d.rich) d.html = textToHtml(mailto.body) + d.html
        else d.text = mailto.body + d.text
      }
      if (mailto.subject || mailto.body) this.touch(c)
    }
    return c
  }

  // respond opens a reply to, or a forward of, a message whose body is loaded.
  respond(kind: Exclude<Kind, 'new'>, email: Email): Composer {
    return this.add(kind, startFrom(kind, email, this.state.identities, this.rich))
  }

  // openDraft opens a draft the server holds. One that is open already
  // comes to the front.
  async openDraft(emailId: Id): Promise<Composer> {
    const open = this.state.composers.find((c) => c.savedId === emailId)
    if (open) {
      this.focus(open)
      return open
    }
    const b = this.client.batch()
    const g = b.call('Email/get', {
      ids: [emailId],
      properties: [...BODY_PROPS, DRAFT_HEADER],
      bodyProperties: BODY_PART_PROPS,
      fetchAllBodyValues: true,
      maxBodyValueBytes: 5_000_000,
    })
    const email = (await b.send()).get(g).list[0]
    if (!email) throw new Error('This draft no longer exists.')
    // Asked twice at once (a double click): still one composer.
    const again = this.state.composers.find((c) => c.savedId === emailId)
    if (again) return again
    return this.add('new', fromStored(email, this.state.identities), emailId)
  }

  // --- editing ---

  // touch notes a change and saves it a moment later.
  touch(c: Composer): void {
    c.version++
    c.sendError = ''
    this.schedule(c, this.debounceMs)
  }

  private schedule(c: Composer, ms: number): void {
    clearTimeout(this.timers.get(c.key))
    this.timers.set(c.key, setTimeout(() => {
      this.timers.delete(c.key)
      void this.save(c)
    }, ms))
  }

  setIdentity(c: Composer, id: Id): void {
    const from = this.identity(c.draft.identityId)
    const to = this.identity(id)
    if (!to || from?.id === to.id) return
    c.draft.identityId = to.id
    if (!c.draft.rich) c.draft.text = swapSignature(c.draft.text, from?.textSignature ?? '', to.textSignature)
    this.touch(c)
  }

  setRich(c: Composer, rich: boolean): void {
    setRich(c.draft, rich)
    this.rich = rich
    this.touch(c)
  }

  // --- attachments ---

  // attach uploads files and adds them to the draft. It answers the files
  // it refused, with the reason.
  attach(c: Composer, files: { file: Blob; name: string }[], inline = false): { refused: string[]; added: Attachment[] } {
    const max = this.client.maxSizeUpload
    const refused: string[] = []
    const added: Attachment[] = []
    for (const { file, name } of files) {
      if (max > 0 && file.size > max) {
        refused.push(`${name} is larger than the ${Math.floor(max / 1_000_000)} MB the server accepts.`)
        continue
      }
      c.draft.attachments.push({
        key: newCid(), name, type: file.type || 'application/octet-stream', size: file.size, blobId: null, cid: newCid(),
        inline: inline && c.draft.rich && file.type.startsWith('image/'), state: 'uploading', progress: 0, error: '',
      })
      const a = c.draft.attachments.at(-1)!
      added.push(a)
      this.upload(c, a, file)
    }
    return { refused, added }
  }

  private upload(c: Composer, a: Attachment, file: Blob): void {
    const abort = new AbortController()
    this.uploads.set(a.key, { file, abort })
    a.state = 'uploading'
    a.progress = 0
    a.error = ''
    this.client.upload(file, a.type, { signal: abort.signal, onProgress: (f) => (a.progress = f) }).then(
      (u) => {
        this.uploads.delete(a.key)
        a.blobId = u.blobId
        a.size = u.size
        a.state = 'done'
        a.progress = 1
        if (c.draft.attachments.includes(a)) this.touch(c)
      },
      (e: unknown) => {
        if (e instanceof DOMException && e.name === 'AbortError') return
        a.state = 'failed'
        a.error = e instanceof HttpError && e.status === 401 ? 'You were logged out. Log in and try again.' : e instanceof Error ? e.message : String(e)
      },
    )
  }

  // retryUpload sends a failed upload again.
  retryUpload(c: Composer, a: Attachment): void {
    const u = this.uploads.get(a.key)
    if (u && a.state === 'failed') this.upload(c, a, u.file)
  }

  // removeAttachment takes an attachment off the draft; an upload that is
  // under way is cancelled.
  removeAttachment(c: Composer, a: Attachment): void {
    this.uploads.get(a.key)?.abort.abort()
    this.uploads.delete(a.key)
    const at = c.draft.attachments.indexOf(a)
    if (at !== -1) c.draft.attachments.splice(at, 1)
    this.touch(c)
  }

  // --- saving ---

  // chain runs the saves and the send of one composer one after another.
  private chain<T>(c: Composer, fn: () => Promise<T>): Promise<T> {
    const p = (this.chains.get(c.key) ?? Promise.resolve()).then(fn, fn)
    this.chains.set(c.key, p.catch(() => {}))
    return p
  }

  // save stores the draft in the Drafts mailbox if it changed since the
  // last save. Emails cannot be changed, so it is a new Email and the
  // destruction of the one before, in one Email/set.
  save(c: Composer): Promise<boolean> {
    clearTimeout(this.timers.get(c.key))
    this.timers.delete(c.key)
    return this.chain(c, () => this.saveNow(c))
  }

  private async saveNow(c: Composer): Promise<boolean> {
    if (c.version === c.savedVersion) return true
    if (!this.state.composers.includes(c)) return true
    const drafts = this.mail.mailboxByRole('drafts')
    const identity = this.identity(c.draft.identityId)
    if (!drafts || !identity) {
      // Not loaded (again) yet: init saves when it is.
      c.saveError = this.mail.state.ready ? 'There is no Drafts mailbox to save this in.' : 'Not saved yet.'
      return false
    }
    const version = c.version
    c.saving = true
    try {
      const b = this.client.batch()
      const set = b.call('Email/set', { create: { draft: buildEmail(c.draft, identity, drafts.id, false) }, ...(c.savedId ? { destroy: [c.savedId] } : {}) })
      const r = (await b.send()).get(set)
      const created = r.created?.draft
      if (c.savedId && r.destroyed?.includes(c.savedId)) c.savedId = null
      if (!created) {
        const err = r.notCreated?.draft
        c.saveError = createErrorText(err)
        if (this.dropLostBlobs(c, err)) this.schedule(c, 0)
        return false
      }
      c.savedId = created.id
      c.savedVersion = version
      c.saveError = ''
      await this.remap(c, created.id)
      void this.mail.refresh()
      return true
    } catch (e) {
      if (e instanceof HttpError && e.status === 401) c.saveError = 'You were logged out. Log in again and this draft is saved.'
      else {
        c.saveError = isMethodError(e) ? `The draft could not be saved: ${e.message}` : 'The draft could not be saved: the server does not answer. Trying again.'
        this.schedule(c, this.retryMs)
      }
      return false
    } finally {
      c.saving = false
      if (!c.saveError && c.version !== c.savedVersion && !this.timers.has(c.key)) this.schedule(c, this.debounceMs)
    }
  }

  // dropLostBlobs marks the attachments the server no longer has.
  private dropLostBlobs(c: Composer, err: SetError | undefined): boolean {
    if (err?.type !== 'blobNotFound' || !err.notFound?.length) return false
    let any = false
    for (const a of c.draft.attachments) {
      if (a.blobId && err.notFound.includes(a.blobId)) {
        a.state = 'failed'
        a.blobId = null
        a.error = 'No longer on the server. Remove it and attach it again.'
        any = true
      }
    }
    return any
  }

  // remap points the attachments at their copies inside the Email that
  // was just made. What they pointed at before may go away: the draft
  // that was replaced, or an upload, which the server keeps for a day.
  private async remap(c: Composer, emailId: Id): Promise<void> {
    if (!c.draft.attachments.some((a) => a.blobId)) return
    try {
      const b = this.client.batch()
      const g = b.call('Email/get', { ids: [emailId], properties: ['attachments'], bodyProperties: BODY_PART_PROPS })
      for (const part of (await b.send()).get(g).list[0]?.attachments ?? []) {
        const a = c.draft.attachments.find((x) => x.cid === part.cid)
        if (a && part.blobId && a.state === 'done') a.blobId = part.blobId
      }
    } catch {
      // The old blobs still work for now; the next save tries again.
    }
  }

  // --- closing ---

  private remove(c: Composer): void {
    clearTimeout(this.timers.get(c.key))
    this.timers.delete(c.key)
    for (const a of c.draft.attachments) {
      this.uploads.get(a.key)?.abort.abort()
      this.uploads.delete(a.key)
    }
    const at = this.state.composers.indexOf(c)
    if (at !== -1) this.state.composers.splice(at, 1)
  }

  // close puts the composer away and keeps the draft. A message nobody
  // wrote anything in leaves no draft. It answers false when the draft
  // could not be saved; the composer then stays.
  async close(c: Composer): Promise<boolean> {
    if (c.version === 0 && !c.savedId) {
      this.remove(c)
      return true
    }
    if (!c.savedId && isBlank(c.draft, this.identity(c.draft.identityId)?.textSignature ?? '')) {
      this.remove(c)
      return true
    }
    if (!(await this.save(c))) return false
    this.remove(c)
    return true
  }

  // discard closes the composer and destroys its draft.
  async discard(c: Composer): Promise<void> {
    this.remove(c)
    const id = await this.chain(c, async () => c.savedId)
    this.chains.delete(c.key)
    if (!id) return
    if (this.mail.state.emails[id]) {
      await this.mail.change(new Map([[id, 'destroy']]))
      return
    }
    try {
      const b = this.client.batch()
      b.call('Email/set', { destroy: [id] })
      await b.send()
      void this.mail.refresh()
    } catch (e) {
      if (!(e instanceof HttpError && e.status === 401)) this.onError('The draft could not be removed from the server. It is still in Drafts.')
    }
  }

  // --- sending ---

  // problems lists what stops a send before it is tried.
  problems(c: Composer): string[] {
    const out: string[] = []
    const d = c.draft
    if (d.to.length + d.cc.length + d.bcc.length === 0) out.push('Add at least one recipient.')
    if (d.attachments.some((a) => a.state === 'uploading')) out.push('Wait until the attachments are uploaded.')
    if (d.attachments.some((a) => a.state === 'failed')) out.push('Remove the attachment that failed, or try it again.')
    return out
  }

  // send submits the message. It resolves to the Sent record (for
  // "undo"), or null when it was not sent; the composer says why.
  send(c: Composer): Promise<Sent | null> {
    const stop = this.problems(c)
    if (stop.length > 0) {
      c.sendError = stop[0]!
      return Promise.resolve(null)
    }
    if (c.sending) return Promise.resolve(null)
    clearTimeout(this.timers.get(c.key))
    this.timers.delete(c.key)
    c.sending = true
    c.sendError = ''
    return this.chain(c, () => this.sendNow(c)).finally(() => (c.sending = false))
  }

  private async sendNow(c: Composer): Promise<Sent | null> {
    const drafts = this.mail.mailboxByRole('drafts')
    const sent = this.mail.mailboxByRole('sent')
    const identity = this.identity(c.draft.identityId)
    if (!drafts || !identity) {
      c.sendError = 'The mailboxes are not loaded. Try again in a moment.'
      return null
    }
    const version = c.version
    const original = c.draft.related ? this.mail.state.emails[c.draft.related.emailId] : undefined
    try {
      const b = this.client.batch()
      const calls = addSendCalls(b, {
        email: buildEmail(c.draft, identity, drafts.id, true), identityId: identity.id, draftsId: drafts.id, sentId: sent?.id ?? null,
        replaces: c.savedId, related: c.draft.related,
      })
      const r = await b.send()
      const er = r.get(calls.set)
      if (c.savedId && er.destroyed?.includes(c.savedId)) c.savedId = null
      const created = er.created?.draft
      if (!created) {
        const err = er.notCreated?.draft
        this.dropLostBlobs(c, err)
        c.sendError = createErrorText(err)
        return null
      }
      // Whatever happens to the send, this Email is the draft now.
      c.savedId = created.id
      c.savedVersion = version
      c.saveError = ''
      const sr = r.get(calls.submit)
      const sub = sr.created?.send
      if (!sub) {
        c.sendError = sendErrorText(sr.notCreated?.send)
        void this.mail.refresh()
        return null
      }
      const marked = !!calls.mark && !r.error(calls.mark) && !r.get(calls.mark).notUpdated?.[c.draft.related!.emailId]
      const record: Sent = {
        key: c.key, composer: c, submissionId: sub.id, emailId: created.id,
        until: Date.parse(sub.sendAt ?? '') || Date.now(),
        marked: marked && original && !original.keywords[c.draft.related!.keyword] ? { emailId: original.id, keyword: c.draft.related!.keyword } : null,
      }
      this.remove(c)
      this.state.sent.push(record)
      this.state.submissions[created.id] = {
        id: sub.id, identityId: identity.id, emailId: created.id, threadId: sub.threadId ?? '', sendAt: sub.sendAt ?? '',
        undoStatus: sub.undoStatus ?? 'pending', deliveryStatus: sub.deliveryStatus ?? null,
      }
      void this.mail.refresh()
      return record
    } catch (e) {
      if (e instanceof HttpError && e.status === 401) c.sendError = 'You were logged out before the message was sent. Log in and send it again.'
      else if (isMethodError(e) && e.type !== 'serverFail') c.sendError = `The message was not sent: ${e.message}`
      else c.sendError = 'The server did not answer, so it is not known whether the message was sent. Look in Sent before you send it again.'
      return null
    }
  }

  // forget drops a Sent record: its time to undo is over.
  forget(s: Sent): void {
    const at = this.state.sent.indexOf(s)
    if (at !== -1) this.state.sent.splice(at, 1)
  }

  // undo takes a send back while the server still holds the message: the
  // submission is cancelled, the message goes back to Drafts and opens
  // again. It throws with the reason when it is too late.
  async undo(s: Sent): Promise<Composer> {
    const drafts = this.mail.mailboxByRole('drafts')
    const sent = this.mail.mailboxByRole('sent')
    const b = this.client.batch()
    const u = b.call('EmailSubmission/set', { update: { [s.submissionId]: { undoStatus: 'canceled' } } })
    const failed = (await b.send()).get(u).notUpdated?.[s.submissionId]
    if (failed) {
      this.forget(s)
      throw new Error(failed.type === 'cannotUnsend' ? 'Too late: the message is on its way.' : `The send could not be taken back: ${failed.description || failed.type}`)
    }
    this.forget(s)
    const sub = this.state.submissions[s.emailId]
    if (sub) sub.undoStatus = 'canceled'
    const c = s.composer
    c.sending = false
    c.minimized = false
    c.savedId = null
    if (drafts) {
      const b2 = this.client.batch()
      const back = b2.call('Email/set', {
        update: {
          [s.emailId]: { ...(sent ? { [`mailboxIds/${sent.id}`]: null } : {}), [`mailboxIds/${drafts.id}`]: true, 'keywords/$draft': true },
          ...(s.marked ? { [s.marked.emailId]: { [`keywords/${s.marked.keyword}`]: null } } : {}),
        },
      })
      try {
        if (!(await b2.send()).get(back).notUpdated?.[s.emailId]) c.savedId = s.emailId
      } catch {
        // The text is in the composer; the next save makes a new draft.
      }
    }
    this.state.composers.push(c)
    const open = this.state.composers.at(-1)!
    if (open.savedId) await this.remap(open, open.savedId)
    // Saved again as a draft proper (it says what it answers).
    this.touch(open)
    void this.mail.refresh()
    return open
  }

  // --- delivery status ---

  private mergeSubmissions(list: EmailSubmission[]): void {
    for (const s of list) {
      const have = this.state.submissions[s.emailId]
      if (!have || have.id === s.id || have.sendAt <= s.sendAt) this.state.submissions[s.emailId] = s
    }
  }

  // loadSubmissions fetches the submissions of emails, once per email.
  async loadSubmissions(emailIds: Id[]): Promise<void> {
    const ids = emailIds.filter((id) => !this.asked.has(id) && !this.state.submissions[id])
    if (ids.length === 0) return
    for (const id of ids) this.asked.add(id)
    try {
      const b = this.client.batch()
      const q = b.call('EmailSubmission/query', { filter: { emailIds: ids } })
      const g = b.call('EmailSubmission/get', { '#ids': q.ref('/ids') })
      this.mergeSubmissions((await b.send()).get(g).list)
    } catch {
      for (const id of ids) this.asked.delete(id)
    }
  }

  // refreshSubmissions fetches the submissions that changed: the queue
  // works on them after the send.
  async refreshSubmissions(): Promise<void> {
    const since = this.client.states.EmailSubmission
    if (!since) return
    const b = this.client.batch()
    const ch = b.call('EmailSubmission/changes', { sinceState: since })
    const created = b.call('EmailSubmission/get', { '#ids': ch.ref('/created') })
    const updated = b.call('EmailSubmission/get', { '#ids': ch.ref('/updated') })
    const r = await b.send()
    const err = r.error(ch)
    if (err) {
      if (err.type !== 'cannotCalculateChanges') throw err
      // Too old a state: learn the present one and ask again for what is shown.
      const b2 = this.client.batch()
      const g = b2.call('EmailSubmission/get', { ids: Object.values(this.state.submissions).map((s) => s.id) })
      const gr = (await b2.send()).get(g)
      this.mergeSubmissions(gr.list)
      this.client.setState('EmailSubmission', gr.state)
      return
    }
    const cr = r.get(ch)
    this.mergeSubmissions([...r.get(created).list, ...r.get(updated).list])
    for (const [emailId, s] of Object.entries(this.state.submissions)) {
      if (cr.destroyed.includes(s.id)) delete this.state.submissions[emailId]
    }
    this.client.setState('EmailSubmission', cr.newState)
    if (cr.hasMoreChanges) await this.refreshSubmissions()
  }

  // unsettled: a submission is still on its way, so its status will change soon.
  unsettled(now = Date.now()): boolean {
    return Object.values(this.state.submissions).some((s) => {
      if (s.undoStatus === 'canceled') return false
      const age = now - Date.parse(s.sendAt)
      return age < 120_000 && (s.undoStatus === 'pending' || Object.values(s.deliveryStatus ?? {}).some((d) => d.delivered === 'queued'))
    })
  }

  // --- identities ---

  private async identitySet(args: { create?: Record<string, Partial<Identity>>; update?: Record<Id, Record<string, unknown>>; destroy?: Id[] }): Promise<void> {
    const b = this.client.batch()
    const set = b.call('Identity/set', args)
    const get = b.call('Identity/get', { ids: null })
    const r = await b.send()
    this.state.identities = r.get(get).list
    const sr = r.get(set)
    const failed = Object.values({ ...sr.notCreated, ...sr.notUpdated, ...sr.notDestroyed })[0]
    if (!failed) return
    if (failed.type === 'forbiddenFrom') throw new Error('The server only lets you send from your own address, with or without a +tag.')
    if (failed.type === 'forbidden') throw new Error('This identity cannot be removed.')
    throw new Error(failed.description || failed.type)
  }

  createIdentity(email: string, input: IdentityInput): Promise<void> {
    return this.identitySet({ create: { new: { email, name: input.name, replyTo: input.replyTo.length ? input.replyTo : null, textSignature: input.textSignature } } })
  }

  updateIdentity(id: Id, input: IdentityInput): Promise<void> {
    return this.identitySet({ update: { [id]: { name: input.name, replyTo: input.replyTo.length ? input.replyTo : null, textSignature: input.textSignature } } })
  }

  async destroyIdentity(id: Id): Promise<void> {
    await this.identitySet({ destroy: [id] })
    const first = this.state.identities[0]?.id ?? ''
    for (const c of this.state.composers) if (c.draft.identityId === id) c.draft.identityId = first
  }
}

// plainBody is the text of a draft whatever its mode: for a title or a check.
export function plainBody(d: Draft): string {
  return d.rich ? htmlToText(d.html) : d.text
}
