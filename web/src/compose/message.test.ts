import { describe, expect, it } from 'vitest'
import type { Email, Identity } from '../jmap/types'
import {
  DRAFT_HEADER, bodyText, buildEmail, emptyDraft, forwardSubject, fromStored, isBlank, parseRelatedHeader, replyHeaders, replyIdentity,
  replyRecipients, replySubject, setRich, startFrom, swapSignature, withSignature,
} from './message'

const a = (email: string, name: string | null = null) => ({ name, email })
const me = 'dev@robinmail.test'
const identity = (over: Partial<Identity> = {}): Identity => ({
  id: 'i1', name: 'Dev', email: me, replyTo: null, bcc: null, textSignature: '', htmlSignature: '', mayDelete: false, ...over,
})

function original(over: Partial<Email> = {}): Email {
  return {
    id: 'e9', blobId: 'blob-e9', threadId: 't9', mailboxIds: { m1: true }, keywords: {}, size: 1234, receivedAt: '2026-03-01T10:00:00Z',
    hasAttachment: false, preview: '', from: [a('ann@example.org', 'Ann')], to: [a(me), a('bob@example.org', 'Bob')], cc: [a('carol@example.org')],
    subject: 'Lunch', messageId: ['m3@example.org'], inReplyTo: ['m2@example.org'], references: ['m1@example.org', 'm2@example.org'],
    textBody: [{ partId: '1', blobId: 'b-1', size: 1, name: null, type: 'text/plain', disposition: null, cid: null }],
    bodyValues: { 1: { value: 'Shall we?\r\n\r\n> earlier\r\n', isEncodingProblem: false, isTruncated: false } },
    attachments: [],
    ...over,
  }
}

describe('subjects', () => {
  it.each([
    ['Lunch', 'Re: Lunch'], ['Re: Lunch', 'Re: Lunch'], ['RE: re: Re:Lunch', 'Re: Lunch'], ['Re[2]: Lunch', 'Re: Lunch'],
    ['Fwd: Lunch', 'Re: Fwd: Lunch'], ['', 'Re:'], [null, 'Re:'], ['Regarding lunch', 'Re: Regarding lunch'],
  ])('reply to %j is %j', (s, want) => {
    expect(replySubject(s)).toBe(want)
  })
  it.each([
    ['Lunch', 'Fwd: Lunch'], ['Fwd: Lunch', 'Fwd: Lunch'], ['FW: fwd: Lunch', 'Fwd: Lunch'], ['Re: Lunch', 'Fwd: Re: Lunch'], ['', 'Fwd:'],
  ])('forward of %j is %j', (s, want) => {
    expect(forwardSubject(s)).toBe(want)
  })
})

describe('replyHeaders (RFC 5322 3.6.4)', () => {
  it('appends the Message-ID of the parent to its References', () => {
    expect(replyHeaders(original())).toEqual({ inReplyTo: ['m3@example.org'], references: ['m1@example.org', 'm2@example.org', 'm3@example.org'] })
  })
  it('uses In-Reply-To of the parent when it has no References and one id there', () => {
    expect(replyHeaders(original({ references: null }))).toEqual({ inReplyTo: ['m3@example.org'], references: ['m2@example.org', 'm3@example.org'] })
    expect(replyHeaders(original({ references: null, inReplyTo: ['x@a', 'y@a'] })).references).toEqual(['m3@example.org'])
  })
  it('starts with the parent alone', () => {
    expect(replyHeaders(original({ references: null, inReplyTo: null }))).toEqual({ inReplyTo: ['m3@example.org'], references: ['m3@example.org'] })
  })
  it('gives nothing for a parent without a Message-ID', () => {
    expect(replyHeaders(original({ messageId: null }))).toEqual({ inReplyTo: [], references: [] })
  })
})

describe('replyRecipients', () => {
  const own = [me, 'dev+shop@robinmail.test']
  it('answers the sender', () => {
    expect(replyRecipients(original(), false, own)).toEqual({ to: [a('ann@example.org', 'Ann')], cc: [] })
  })
  it('answers Reply-To when there is one', () => {
    expect(replyRecipients(original({ replyTo: [a('list@example.org')] }), false, own).to).toEqual([a('list@example.org')])
  })
  it('reply all adds the other recipients, without our own addresses', () => {
    const r = replyRecipients(original({ to: [a('DEV@robinmail.test'), a('bob@example.org', 'Bob'), a('dev+shop@robinmail.test')] }), true, own)
    expect(r).toEqual({ to: [a('ann@example.org', 'Ann')], cc: [a('bob@example.org', 'Bob'), a('carol@example.org')] })
  })
  it('names nobody twice', () => {
    const r = replyRecipients(original({ to: [a('ann@example.org'), a('bob@example.org')], cc: [a('BOB@example.org'), a('carol@example.org')] }), true, own)
    expect(r.to.map((x) => x.email)).toEqual(['ann@example.org'])
    expect(r.cc.map((x) => x.email)).toEqual(['bob@example.org', 'carol@example.org'])
  })
  it('a reply to our own message goes to its recipients', () => {
    const sent = original({ from: [a(me)], to: [a('bob@example.org')], cc: [a('carol@example.org')] })
    expect(replyRecipients(sent, false, own)).toEqual({ to: [a('bob@example.org')], cc: [] })
    expect(replyRecipients(sent, true, own)).toEqual({ to: [a('bob@example.org')], cc: [a('carol@example.org')] })
  })
  it('a note to ourselves is answered to ourselves', () => {
    expect(replyRecipients(original({ from: [a(me)], to: [a(me)], cc: [] }), true, own)).toEqual({ to: [a(me)], cc: [] })
  })
})

describe('replyIdentity', () => {
  it('is the identity the message was sent to', () => {
    const ids = [identity(), identity({ id: 'i2', email: 'dev+shop@robinmail.test' })]
    expect(replyIdentity(original({ to: [a('dev+shop@robinmail.test')] }), ids)!.id).toBe('i2')
    expect(replyIdentity(original({ to: [a('other@example.org')], cc: [] }), ids)!.id).toBe('i1')
  })
})

describe('bodies', () => {
  it('quotes the text body under a line that says who wrote it', () => {
    const d = startFrom('reply', original(), [identity()], false)
    expect(d.text).toMatch(/^\n\nOn .+, Ann <ann@example\.org> wrote:\n> Shall we\?\n>\n>> earlier$/)
    expect(d.subject).toBe('Re: Lunch')
    expect(d.related).toEqual({ emailId: 'e9', keyword: '$answered' })
    expect(d.inReplyTo).toEqual(['m3@example.org'])
  })

  it('quotes a text rendering of a message that only has HTML', () => {
    const html = original({
      textBody: [{ partId: '1', blobId: 'b-1', size: 1, name: null, type: 'text/html', disposition: null, cid: null }],
      bodyValues: { 1: { value: '<p>Hello <b>you</b></p><script>steal()</script><img src="https://x.example/p.png" onerror="x()">', isEncodingProblem: false, isTruncated: false } },
    })
    expect(bodyText(html)).toBe('Hello you')
    const d = startFrom('reply', html, [identity()], true)
    expect(d.rich).toBe(true)
    // In a rich body the quote is escaped text: no tag of the original survives.
    expect(d.html).toContain('<blockquote><p>Hello you</p></blockquote>')
    expect(d.html).not.toMatch(/<script|<img|<b>|onerror/)
  })

  it('puts the signature above the quote', () => {
    const d = startFrom('reply', original(), [identity({ textSignature: 'Dev\nrobinmail' })], false)
    expect(d.text.startsWith('\n\n-- \nDev\nrobinmail\n\nOn ')).toBe(true)
  })

  it('forwards with the header of the original and its attachments by blob id', () => {
    const d = startFrom('forward', original({
      attachments: [{ partId: '2', blobId: 'blob-e9-2', size: 77, name: 'a.pdf', type: 'application/pdf', disposition: 'attachment', cid: null }],
    }), [identity()], false)
    expect(d.subject).toBe('Fwd: Lunch')
    expect(d.to).toEqual([])
    expect(d.text).toContain('---------- Forwarded message ----------\nFrom: Ann <ann@example.org>\n')
    expect(d.text).toContain('Subject: Lunch\nTo: dev@robinmail.test, Bob <bob@example.org>\nCc: carol@example.org\n\nShall we?')
    expect(d.attachments).toMatchObject([{ blobId: 'blob-e9-2', name: 'a.pdf', type: 'application/pdf', size: 77, state: 'done', inline: false }])
    expect(d.related).toEqual({ emailId: 'e9', keyword: '$forwarded' })
    expect(d.inReplyTo).toEqual([])
    expect(d.references).toEqual(['m1@example.org', 'm2@example.org', 'm3@example.org'])
  })

  it('forwards as an attachment: the whole message as message/rfc822', () => {
    const d = startFrom('forwardAttached', original({ subject: 'A/B: "plan"' }), [identity()], false)
    expect(d.attachments).toMatchObject([{ blobId: 'blob-e9', type: 'message/rfc822', name: 'A B   plan.eml', size: 1234 }])
    expect(d.text).toBe('')
  })

  it('swaps the signature when the identity changes', () => {
    expect(withSignature('Hi', 'Ann')).toBe('Hi\n\n-- \nAnn')
    expect(swapSignature('Hi\n\n-- \nAnn\n\nOn x wrote:\n> q', 'Ann', 'Shop')).toBe('Hi\n\n-- \nShop\n\nOn x wrote:\n> q')
    expect(swapSignature('Hi', '', 'Shop')).toBe('Hi\n\n-- \nShop')
    expect(swapSignature('Hi\n\nOn x wrote:\n> q', '', 'Shop')).toBe('Hi\n\n-- \nShop\n\nOn x wrote:\n> q')
    expect(swapSignature('Hi\n\n-- \nAnn', 'Ann', '')).toBe('Hi')
    // A signature the writer changed is left alone.
    expect(swapSignature('Hi\n\n-- \nAnnie', 'Ann B', 'Shop')).toBe('Hi\n\n-- \nAnnie')
  })

  it('knows a draft nobody wrote in', () => {
    const i = identity({ textSignature: 'Dev' })
    expect(isBlank(emptyDraft(i, false), 'Dev')).toBe(true)
    expect(isBlank(emptyDraft(i, true), 'Dev')).toBe(true)
    expect(isBlank({ ...emptyDraft(i, false), subject: 'x' }, 'Dev')).toBe(false)
    expect(isBlank({ ...emptyDraft(i, false), text: 'hello' }, 'Dev')).toBe(false)
  })
})

describe('buildEmail', () => {
  const d = () => ({ ...emptyDraft(identity(), false), to: [a('ann@example.org')], subject: 'Hi', text: 'Hello' })

  it('writes a plain draft', () => {
    expect(buildEmail(d(), identity(), 'm2', false)).toEqual({
      mailboxIds: { m2: true }, keywords: { $draft: true, $seen: true }, from: [{ name: 'Dev', email: me }], to: [a('ann@example.org')],
      subject: 'Hi', textBody: [{ partId: 'text', type: 'text/plain' }], bodyValues: { text: { value: 'Hello' } },
    })
  })

  it('writes text and HTML for a rich draft, inline images by cid', () => {
    const draft = d()
    setRich(draft, true)
    draft.html = '<p>See <b>this</b></p><p><img src="cid:pic@robinmail" alt=""></p>'
    const att = { key: 'k', name: 'p.png', type: 'image/png', size: 5, blobId: 'upload1', cid: 'pic@robinmail', inline: true, state: 'done' as const, progress: 1, error: '' }
    draft.attachments = [att, { ...att, key: 'k2', name: 'doc.pdf', type: 'application/pdf', blobId: 'upload2', cid: 'doc@robinmail' }, { ...att, key: 'k3', blobId: null, state: 'uploading', cid: 'x@robinmail' }]
    const e = buildEmail(draft, identity({ replyTo: [a('desk@example.org')] }), 'm2', true)
    expect(e.textBody).toEqual([{ partId: 'text', type: 'text/plain' }])
    expect(e.htmlBody).toEqual([{ partId: 'html', type: 'text/html' }])
    expect(e.bodyValues.text!.value).toBe('See this')
    expect(e.bodyValues.html!.value).toBe('<!DOCTYPE html><html><body><p>See <b>this</b></p><p><img src="cid:pic@robinmail" alt=""></p></body></html>')
    expect(e.attachments).toEqual([
      { blobId: 'upload1', type: 'image/png', name: 'p.png', cid: 'pic@robinmail', disposition: 'inline' },
      { blobId: 'upload2', type: 'application/pdf', name: 'doc.pdf', cid: 'doc@robinmail', disposition: 'attachment' },
    ])
    expect(e.replyTo).toEqual([a('desk@example.org')])
  })

  it('a saved draft says what it answers; the message that is sent does not', () => {
    const draft = startFrom('reply', original(), [identity()], false)
    const saved = buildEmail(draft, identity(), 'm2', false)
    expect(saved[DRAFT_HEADER]).toBe('reply e9')
    expect(saved.inReplyTo).toEqual(['m3@example.org'])
    expect(saved.references).toEqual(['m1@example.org', 'm2@example.org', 'm3@example.org'])
    expect(DRAFT_HEADER in buildEmail(draft, identity(), 'm2', true)).toBe(false)
    expect(parseRelatedHeader('reply e9')).toEqual({ emailId: 'e9', keyword: '$answered' })
    expect(parseRelatedHeader('forward e12')).toEqual({ emailId: 'e12', keyword: '$forwarded' })
    expect(parseRelatedHeader('reply e9 <script>')).toBeNull()
    expect(parseRelatedHeader(null)).toBeNull()
  })
})

describe('fromStored', () => {
  it('reads a rich draft with its attachments back', () => {
    const part = (partId: string, type: string) => ({ partId, blobId: `blob-e5-${partId}`, size: 1, name: null, type, disposition: null, cid: null })
    const d = fromStored({
      from: [a('dev+shop@robinmail.test')], to: [a('ann@example.org')], cc: null, bcc: [a('boss@example.org')], subject: 'Hi',
      inReplyTo: ['m3@example.org'], references: ['m3@example.org'],
      textBody: [part('1', 'text/plain')], htmlBody: [part('2', 'text/html')],
      attachments: [
        { ...part('3', 'image/png'), name: 'p.png', cid: 'pic@robinmail', disposition: 'inline', size: 9 },
        { ...part('4', 'application/pdf'), name: 'doc.pdf', cid: null, disposition: 'attachment' },
      ],
      bodyValues: {
        1: { value: 'See this', isEncodingProblem: false, isTruncated: false },
        2: { value: '<!DOCTYPE html><html><body><p>See <img src="cid:pic@robinmail" alt=""></p></body></html>', isEncodingProblem: false, isTruncated: false },
      },
      [DRAFT_HEADER]: 'reply e9',
    }, [identity(), identity({ id: 'i2', email: 'dev+shop@robinmail.test' })])
    expect(d).toMatchObject({
      identityId: 'i2', to: [a('ann@example.org')], cc: [], bcc: [a('boss@example.org')], subject: 'Hi', rich: true, text: '',
      html: '<p>See <img src="cid:pic@robinmail" alt=""></p>', inReplyTo: ['m3@example.org'], related: { emailId: 'e9', keyword: '$answered' },
    })
    expect(d.attachments).toMatchObject([
      { name: 'p.png', blobId: 'blob-e5-3', cid: 'pic@robinmail', inline: true, state: 'done' },
      { name: 'doc.pdf', blobId: 'blob-e5-4', inline: false, state: 'done' },
    ])
    expect(d.attachments[1]!.cid).toMatch(/@robinmail$/)
  })
})
