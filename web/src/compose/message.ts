// What a message being written is, how a reply or a forward starts, and
// what is sent to the server for it. Pure: no requests, no DOM.

import type { Email, EmailAddress, EmailCreate, Id, Identity, NewBodyPart } from '../jmap/types'
import { dedupe, formatAddress, sameAddress } from './address'
import { htmlToText, quote, textToHtml } from './text'

export type Kind = 'new' | 'reply' | 'replyAll' | 'forward' | 'forwardAttached'

export interface Attachment {
  key: string
  name: string
  type: string
  size: number
  // null until the upload is done.
  blobId: Id | null
  // Every attachment has a Content-ID: an inline image is referred to by
  // it, and it tells the parts apart when a saved draft is read back.
  cid: string
  inline: boolean
  state: 'uploading' | 'done' | 'failed'
  progress: number
  error: string
}

export interface Draft {
  identityId: Id
  to: EmailAddress[]
  cc: EmailAddress[]
  bcc: EmailAddress[]
  subject: string
  // The body is plain text, or rich text as the small set of HTML the
  // editor writes (images as "cid:").
  rich: boolean
  text: string
  html: string
  attachments: Attachment[]
  inReplyTo: string[]
  references: string[]
  // The message this one answers or forwards: it gets $answered or
  // $forwarded when this one is sent.
  related: { emailId: Id; keyword: '$answered' | '$forwarded' } | null
}

let counter = 0
export function newCid(): string {
  return `${Date.now().toString(36)}.${(counter++).toString(36)}.${Math.random().toString(36).slice(2, 10)}@robinmail`
}

// --- subjects ---

// replySubject puts one "Re:" in front, whatever was there: "Re: Re: x",
// "RE: x" and "Re[2]: x" all answer as "Re: x".
export function replySubject(subject: string | null | undefined): string {
  const s = (subject ?? '').trim().replace(/^(?:re(?:\[\d+\])?\s*:\s*)+/i, '')
  return `Re: ${s}`.trim()
}

export function forwardSubject(subject: string | null | undefined): string {
  const s = (subject ?? '').trim().replace(/^(?:(?:fwd?|fw)\s*:\s*)+/i, '')
  return `Fwd: ${s}`.trim()
}

// --- threading ---

// replyHeaders gives In-Reply-To and References of a reply (RFC 5322
// 3.6.4): In-Reply-To is the parent's Message-ID; References is the
// parent's References, or without those its In-Reply-To if that is one
// id, followed by the parent's Message-ID. A parent without a Message-ID
// gives neither.
export function replyHeaders(parent: Pick<Email, 'messageId' | 'inReplyTo' | 'references'>): { inReplyTo: string[]; references: string[] } {
  const id = parent.messageId ?? []
  if (id.length === 0) return { inReplyTo: [], references: [] }
  let before: string[] = []
  if (parent.references?.length) before = parent.references
  else if (parent.inReplyTo?.length === 1) before = parent.inReplyTo
  return { inReplyTo: [...id], references: [...before.filter((r) => !id.includes(r)), ...id] }
}

// --- recipients ---

function isOwn(email: string, own: string[]): boolean {
  return own.some((o) => sameAddress(o, email))
}

// replyRecipients decides who a reply goes to. The answer goes to
// Reply-To, or else From. "All" adds the other recipients of the
// original as Cc. Our own addresses are left out, except when nobody
// else is left. A reply to a message we sent ourselves goes to the
// people it was sent to.
export function replyRecipients(
  email: Pick<Email, 'from' | 'to' | 'cc' | 'replyTo'>, all: boolean, own: string[],
): { to: EmailAddress[]; cc: EmailAddress[] } {
  const from = email.from ?? []
  const origTo = email.to ?? []
  const origCc = email.cc ?? []
  const mine = from.length > 0 && from.every((a) => isOwn(a.email, own))
  let to = dedupe(mine ? origTo : email.replyTo?.length ? email.replyTo : from)
  const others = to.filter((a) => !isOwn(a.email, own))
  if (others.length > 0) to = others
  if (!all) return { to, cc: [] }
  const cc = dedupe(mine ? origCc : [...origTo, ...origCc]).filter(
    (a) => !isOwn(a.email, own) && !to.some((t) => sameAddress(t.email, a.email)),
  )
  return { to, cc }
}

// replyIdentity picks the identity a message was sent to, or the first.
export function replyIdentity(email: Pick<Email, 'to' | 'cc' | 'from'>, identities: Identity[]): Identity | undefined {
  const sentTo = [...(email.to ?? []), ...(email.cc ?? []), ...(email.from ?? [])]
  for (const a of sentTo) {
    const i = identities.find((x) => sameAddress(x.email, a.email))
    if (i) return i
  }
  return identities[0]
}

// --- bodies ---

// bodyText is the text of a message for quoting: its text body, and for
// a message that only has HTML a text rendering of that.
export function bodyText(email: Pick<Email, 'textBody' | 'bodyValues'>): string {
  const values = email.bodyValues ?? {}
  return (email.textBody ?? [])
    .map((p) => {
      const v = p.partId !== null ? values[p.partId] : undefined
      if (!v) return ''
      return p.type === 'text/html' ? htmlToText(v.value) : v.value.replace(/\r\n/g, '\n')
    })
    .filter((s) => s !== '')
    .join('\n')
    .replace(/\s+$/, '')
}

const stamp = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' })
function when(email: Pick<Email, 'sentAt' | 'receivedAt'>): string {
  const d = new Date(email.sentAt || email.receivedAt)
  return Number.isNaN(d.getTime()) ? '' : stamp.format(d)
}

// replyQuote is the quoted original under a reply.
export function replyQuote(email: Pick<Email, 'from' | 'sentAt' | 'receivedAt' | 'textBody' | 'bodyValues'>): string {
  const who = (email.from ?? []).map(formatAddress).join(', ') || 'someone'
  const date = when(email)
  return `${date ? `On ${date}, ${who}` : who} wrote:\n${quote(bodyText(email))}`
}

// forwardBlock is the original under a forward, with its header.
export function forwardBlock(email: Pick<Email, 'from' | 'to' | 'cc' | 'subject' | 'sentAt' | 'receivedAt' | 'textBody' | 'bodyValues'>): string {
  const list = (l: EmailAddress[] | null | undefined) => (l ?? []).map(formatAddress).join(', ')
  const head = [
    '---------- Forwarded message ----------',
    `From: ${list(email.from)}`,
    `Date: ${when(email)}`,
    `Subject: ${email.subject ?? ''}`,
    `To: ${list(email.to)}`,
    ...(email.cc?.length ? [`Cc: ${list(email.cc)}`] : []),
  ]
  return `${head.join('\n')}\n\n${bodyText(email)}`
}

// withSignature puts a signature under a body, after the usual "-- "
// line (RFC 3676 4.3).
export function withSignature(body: string, signature: string): string {
  const s = signature.trim()
  return s ? `${body}\n\n-- \n${s}` : body
}

// swapSignature replaces the signature of one identity by that of
// another, if it is still there as it was put.
export function swapSignature(body: string, from: string, to: string): string {
  const old = from.trim() ? `\n\n-- \n${from.trim()}` : ''
  const next = to.trim() ? `\n\n-- \n${to.trim()}` : ''
  if (old === '') {
    // Before the quoted original, or at the end.
    const at = body.search(/\n\n(On .* wrote:\n>|---------- Forwarded message ----------)/)
    return at === -1 ? body + next : body.slice(0, at) + next + body.slice(at)
  }
  const at = body.indexOf(old)
  return at === -1 ? body : body.slice(0, at) + next + body.slice(at + old.length)
}

// --- starting a message ---

export function emptyDraft(identity: Identity | undefined, rich: boolean): Draft {
  const text = withSignature('', identity?.textSignature ?? '')
  return {
    identityId: identity?.id ?? '',
    to: [], cc: [], bcc: [...(identity?.bcc ?? [])],
    subject: '',
    rich,
    text: rich ? '' : text,
    html: rich ? textToHtml(text) : '',
    attachments: [],
    inReplyTo: [], references: [], related: null,
  }
}

type Original = Pick<
  Email,
  'id' | 'blobId' | 'size' | 'from' | 'to' | 'cc' | 'replyTo' | 'subject' | 'sentAt' | 'receivedAt' | 'messageId' | 'inReplyTo' |
  'references' | 'textBody' | 'bodyValues' | 'attachments'
>

// startFrom makes the draft of a reply or a forward of a message.
export function startFrom(kind: Exclude<Kind, 'new'>, email: Original, identities: Identity[], rich: boolean): Draft {
  const identity = replyIdentity(email, identities)
  const d = emptyDraft(identity, false)
  const own = identities.map((i) => i.email)
  const signature = withSignature('', identity?.textSignature ?? '')
  if (kind === 'reply' || kind === 'replyAll') {
    const r = replyRecipients(email, kind === 'replyAll', own)
    d.to = r.to
    d.cc = r.cc
    d.subject = replySubject(email.subject)
    Object.assign(d, replyHeaders(email))
    d.related = { emailId: email.id, keyword: '$answered' }
    d.text = `${signature}\n\n${replyQuote(email)}`
  } else {
    d.subject = forwardSubject(email.subject)
    d.related = { emailId: email.id, keyword: '$forwarded' }
    // A forward is not a reply, but it belongs to the conversation.
    d.references = replyHeaders(email).references
    if (kind === 'forward') {
      d.text = `${signature}\n\n${forwardBlock(email)}`
      d.attachments = (email.attachments ?? []).filter((a) => a.blobId).map((a) => ({
        key: newCid(), name: a.name || 'attachment', type: a.type, size: a.size, blobId: a.blobId, cid: newCid(),
        inline: false, state: 'done' as const, progress: 1, error: '',
      }))
    } else {
      d.text = signature
      d.attachments = [{
        key: newCid(), name: `${(email.subject || 'message').replace(/[\\/:*?"<>|\x00-\x1f]/g, ' ').trim().slice(0, 80) || 'message'}.eml`,
        type: 'message/rfc822', size: email.size, blobId: email.blobId, cid: newCid(), inline: false, state: 'done', progress: 1, error: '',
      }]
    }
  }
  if (rich) setRich(d, true)
  return d
}

// setRich switches a draft between plain and rich text, carrying the body
// over. Going to plain text loses the formatting and inline images
// become ordinary attachments.
export function setRich(d: Draft, rich: boolean): void {
  if (d.rich === rich) return
  if (rich) {
    d.html = textToHtml(d.text)
    d.text = ''
  } else {
    d.text = htmlToText(d.html)
    d.html = ''
    for (const a of d.attachments) a.inline = false
  }
  d.rich = rich
}

// isBlank says whether a draft holds nothing a person put there.
export function isBlank(d: Draft, signature: string): boolean {
  const body = (d.rich ? htmlToText(d.html) : d.text).trim()
  const sig = withSignature('', signature).trim()
  return d.to.length + d.cc.length + d.bcc.length + d.attachments.length === 0 && d.subject.trim() === '' && (body === '' || body === sig)
}

// --- what goes to the server ---

// The header a saved draft carries to say what it answers, so that a
// draft opened again still marks the original when it is sent. It is not
// on the message that is sent.
export const DRAFT_HEADER = 'header:X-Robinmail-Draft:asText' as const

export function relatedHeader(r: Draft['related']): string {
  return r ? `${r.keyword === '$answered' ? 'reply' : 'forward'} ${r.emailId}` : ''
}

export function parseRelatedHeader(v: string | null | undefined): Draft['related'] {
  const m = /^\s*(reply|forward) ([A-Za-z0-9_-]+)\s*$/.exec(v ?? '')
  return m ? { emailId: m[2]!, keyword: m[1] === 'reply' ? '$answered' : '$forwarded' } : null
}

// usedCids lists the Content-IDs the HTML refers to.
export function usedCids(html: string): Set<string> {
  const out = new Set<string>()
  for (const m of html.matchAll(/\ssrc="cid:([^"]+)"/g)) out.add(m[1]!)
  return out
}

// buildEmail writes the Email/set create object of a draft. forSend
// leaves out the draft header; either way the message is made in Drafts
// with $draft, and a send moves it (see sendCalls in the store).
export function buildEmail(d: Draft, identity: Identity, draftsId: Id, forSend: boolean): EmailCreate {
  const text = d.rich ? htmlToText(d.html) : d.text
  const inline = d.rich ? usedCids(d.html) : new Set<string>()
  const attachments: NewBodyPart[] = d.attachments
    .filter((a) => a.state === 'done' && a.blobId)
    .map((a) => ({
      blobId: a.blobId!, type: a.type || 'application/octet-stream', name: a.name, cid: a.cid,
      disposition: inline.has(a.cid) ? 'inline' : 'attachment',
    }))
  const email: EmailCreate = {
    mailboxIds: { [draftsId]: true },
    keywords: { $draft: true, $seen: true },
    from: [{ name: identity.name || null, email: identity.email }],
    subject: d.subject,
    textBody: [{ partId: 'text', type: 'text/plain' }],
    bodyValues: { text: { value: text } },
  }
  if (d.to.length) email.to = d.to
  if (d.cc.length) email.cc = d.cc
  if (d.bcc.length) email.bcc = d.bcc
  if (identity.replyTo?.length) email.replyTo = identity.replyTo
  if (d.inReplyTo.length) email.inReplyTo = d.inReplyTo
  if (d.references.length) email.references = d.references
  if (d.rich) {
    email.htmlBody = [{ partId: 'html', type: 'text/html' }]
    email.bodyValues.html = { value: `<!DOCTYPE html><html><body>${d.html}</body></html>` }
  }
  if (attachments.length) email.attachments = attachments
  if (!forSend && d.related) email[DRAFT_HEADER] = relatedHeader(d.related)
  return email
}

// bodyOfHtml takes the part between <body> and </body> of a stored HTML
// body, as buildEmail wrote it.
export function bodyOfHtml(html: string): string {
  const m = /<body[^>]*>([\s\S]*)<\/body>/i.exec(html)
  return m ? m[1]! : html
}

// fromStored reads a draft back from the Email the server holds.
export function fromStored(
  email: Pick<Email, 'from' | 'to' | 'cc' | 'bcc' | 'subject' | 'inReplyTo' | 'references' | 'textBody' | 'htmlBody' | 'attachments' | 'bodyValues'> &
    { [DRAFT_HEADER]?: string | null },
  identities: Identity[],
): Draft {
  const from = email.from?.[0]?.email ?? ''
  const identity = identities.find((i) => sameAddress(i.email, from)) ?? identities[0]
  const values = email.bodyValues ?? {}
  const htmlPart = (email.htmlBody ?? []).find((p) => p.type === 'text/html' && p.partId !== null && values[p.partId])
  const textPart = (email.textBody ?? []).find((p) => p.type === 'text/plain' && p.partId !== null && values[p.partId])
  const rich = !!htmlPart
  const html = rich ? bodyOfHtml(values[htmlPart.partId!]!.value) : ''
  const inline = usedCids(html)
  return {
    identityId: identity?.id ?? '',
    to: email.to ?? [], cc: email.cc ?? [], bcc: email.bcc ?? [],
    subject: email.subject ?? '',
    rich,
    // A message ends with a line end, so a text that had none comes back with one.
    text: rich ? '' : (textPart ? values[textPart.partId!]!.value.replace(/\r\n/g, '\n').replace(/\n$/, '') : ''),
    html,
    attachments: (email.attachments ?? []).filter((a) => a.blobId).map((a) => {
      const cid = a.cid || newCid()
      return {
        key: newCid(), name: a.name || 'attachment', type: a.type, size: a.size, blobId: a.blobId, cid,
        inline: inline.has(cid), state: 'done' as const, progress: 1, error: '',
      }
    }),
    inReplyTo: email.inReplyTo ?? [],
    references: email.references ?? [],
    related: parseRelatedHeader(email[DRAFT_HEADER]),
  }
}
