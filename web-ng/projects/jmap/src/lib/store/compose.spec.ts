import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { JmapClient } from '../jmap/client'
import { FakeServer } from '../test/fake'
import { ComposeStore, sendErrorText } from './compose'
import { MailStore } from './mail'

const DRAFTS = 'm2'
const SENT = 'm3'
const a = (email: string, name: string | null = null) => ({ name, email })

async function setup() {
  const server = new FakeServer()
  const unauthorized = vi.fn()
  const client = new JmapClient({ fetch: server.fetch, uploader: server.uploader, onUnauthorized: unauthorized })
  const mail = new MailStore(client)
  const compose = new ComposeStore(client, mail, { debounceMs: 1000, retryMs: 5000 })
  await mail.load()
  await compose.init()
  server.requests = []
  return { server, client, mail, compose, unauthorized }
}

// The calls of a kind the server got, as [name, args].
const calls = (server: FakeServer, name: string) => server.calls.filter((c) => c[0] === name).map((c) => c[1] as Record<string, any>)
const drafts = (server: FakeServer) => [...server.emails.values()].filter((e) => e.mailboxIds[DRAFTS])

beforeEach(() => vi.useFakeTimers())
afterEach(() => vi.useRealTimers())

// tick lets the timers and the requests they start run.
const tick = (ms: number) => vi.advanceTimersByTimeAsync(ms)

describe('drafts', () => {
  it('saves a moment after the last change, as one Email in Drafts with $draft', async () => {
    const { server, compose } = await setup()
    const c = compose.compose()
    c.draft.to = [a('ann@example.org')]
    compose.touch(c)
    await tick(600)
    c.draft.subject = 'Hello'
    c.draft.text = 'First words'
    compose.touch(c)
    await tick(600)
    expect(calls(server, 'Email/set')).toHaveLength(0) // 600 ms after the last change: not yet
    await tick(500)
    const sets = calls(server, 'Email/set')
    expect(sets).toHaveLength(1)
    expect(sets[0]!.create.draft).toMatchObject({
      mailboxIds: { [DRAFTS]: true }, keywords: { $draft: true, $seen: true }, subject: 'Hello', to: [a('ann@example.org')],
      bodyValues: { text: { value: 'First words' } },
    })
    expect(sets[0]!.destroy).toBeUndefined()
    expect(c.savedId).toBe(drafts(server)[0]!.id)
    expect(c.version).toBe(c.savedVersion)
    expect(compose.hasUnsaved()).toBe(false)
  })

  it('a changed draft is a create plus the destroy of the one before, in one call', async () => {
    const { server, compose } = await setup()
    const c = compose.compose()
    c.draft.text = 'one'
    compose.touch(c)
    await tick(1000)
    const first = c.savedId!
    c.draft.text = 'one two'
    compose.touch(c)
    await tick(1000)
    const sets = calls(server, 'Email/set')
    expect(sets).toHaveLength(2)
    expect(sets[1]!.create.draft.bodyValues.text.value).toBe('one two')
    expect(sets[1]!.destroy).toEqual([first])
    expect(drafts(server).map((e) => e.id)).toEqual([c.savedId])
    expect(c.savedId).not.toBe(first)
  })

  it('does not save what did not change, and saves once for changes made during a save', async () => {
    const { server, compose } = await setup()
    const c = compose.compose()
    c.draft.text = 'one'
    compose.touch(c)
    server.hold()
    await tick(1000)
    expect(c.saving).toBe(true)
    c.draft.text = 'one two'
    compose.touch(c)
    c.draft.text = 'one two three'
    compose.touch(c)
    server.release()
    await tick(0)
    expect(compose.hasUnsaved()).toBe(true) // the server has "one"
    await tick(1000)
    await tick(1000)
    expect(calls(server, 'Email/set')).toHaveLength(2)
    expect(drafts(server)).toHaveLength(1)
    expect(server.created.get(c.savedId!)!.bodyValues.text.value).toBe('one two three')
    expect(await compose.save(c)).toBe(true)
    expect(calls(server, 'Email/set')).toHaveLength(2)
  })

  it('keeps the text when the server fails, and tries again', async () => {
    const { server, compose } = await setup()
    const c = compose.compose()
    c.draft.text = 'do not lose me'
    compose.touch(c)
    server.httpStatus = 500
    await tick(1000)
    expect(c.saveError).toMatch(/could not be saved/)
    expect(c.draft.text).toBe('do not lose me')
    expect(compose.hasUnsaved()).toBe(true)
    expect(compose.state.composers).toContain(c)
    server.httpStatus = 0
    await tick(5000)
    expect(c.saveError).toBe('')
    expect(server.created.get(c.savedId!)!.bodyValues.text.value).toBe('do not lose me')
  })

  it('keeps the composer over a 401 and saves after the next login', async () => {
    const { server, mail, compose, unauthorized } = await setup()
    const c = compose.compose()
    c.draft.subject = 'Before the session ended'
    c.draft.text = 'typed text'
    compose.touch(c)
    server.httpStatus = 401
    await tick(1000)
    expect(unauthorized).toHaveBeenCalled()
    mail.reset() // what the app does on a 401
    expect(c.saveError).toMatch(/logged out/)
    expect(compose.state.composers).toHaveLength(1)
    expect(c.draft.text).toBe('typed text')
    await tick(60_000)
    expect(drafts(server)).toHaveLength(0) // it does not hammer a server that wants a login
    // Logged in again.
    server.httpStatus = 0
    await mail.load()
    await compose.init()
    await tick(0)
    expect(c.saveError).toBe('')
    expect(drafts(server).map((e) => e.subject)).toEqual(['Before the session ended'])
  })

  it('drops the composers of another user', async () => {
    const { mail, compose } = await setup()
    compose.compose()
    mail.reset()
    await mail.load()
    mail.state.username = 'someone-else@robinmail.test'
    await compose.init()
    expect(compose.state.composers).toHaveLength(0)
  })

  it('when the create is refused but the old draft is gone, the next save does not destroy it again', async () => {
    const { server, compose } = await setup()
    const c = compose.compose()
    c.draft.text = 'one'
    compose.touch(c)
    await tick(1000)
    c.draft.text = 'two'
    compose.touch(c)
    server.failCreate = { type: 'tooLarge' }
    await tick(1000)
    expect(c.saveError).toMatch(/too large/)
    expect(c.savedId).toBeNull()
    expect(drafts(server)).toHaveLength(0)
    expect(c.draft.text).toBe('two')
    c.draft.text = 'two, shorter'
    compose.touch(c)
    await tick(1000)
    expect(calls(server, 'Email/set').at(-1)!.destroy).toBeUndefined()
    expect(drafts(server)).toHaveLength(1)
  })

  it('closing saves first; an untouched composer leaves nothing; discard destroys the draft', async () => {
    const { server, compose } = await setup()
    const untouched = compose.compose()
    expect(await compose.close(untouched)).toBe(true)
    expect(server.calls).toHaveLength(0)

    const c = compose.compose()
    c.draft.subject = 'Keep me'
    compose.touch(c)
    expect(await compose.close(c)).toBe(true)
    expect(compose.state.composers).toHaveLength(0)
    expect(drafts(server).map((e) => e.subject)).toEqual(['Keep me'])

    const again = await compose.openDraft(drafts(server)[0]!.id)
    expect(again.draft.subject).toBe('Keep me')
    expect(compose.hasUnsaved()).toBe(false)
    await compose.discard(again)
    expect(compose.state.composers).toHaveLength(0)
    expect(drafts(server)).toHaveLength(0)
  })

  it('closing fails when the draft cannot be saved, and the composer stays', async () => {
    const { server, compose } = await setup()
    const c = compose.compose()
    c.draft.subject = 'x'
    compose.touch(c)
    server.httpStatus = 503
    expect(await compose.close(c)).toBe(false)
    expect(compose.state.composers).toContain(c)
  })

  it('opens a stored draft once, with what it answers', async () => {
    const { server, mail, compose } = await setup()
    const orig = server.deliver({ subject: 'Question', from: 'ann@example.org' })
    await mail.openThread(orig.threadId)
    const c = compose.respond('reply', mail.state.emails[orig.id]!)
    c.draft.text = `Answer${c.draft.text}`
    compose.touch(c)
    expect(await compose.close(c)).toBe(true)
    const id = drafts(server)[0]!.id
    const [x, y] = await Promise.all([compose.openDraft(id), compose.openDraft(id)])
    expect(x).toBe(y)
    expect(compose.state.composers).toHaveLength(1)
    expect(x.draft.related).toEqual({ emailId: orig.id, keyword: '$answered' })
    expect(x.draft.inReplyTo).toEqual([`${orig.id}@fake`])
    expect(x.draft.to).toEqual([a('ann@example.org')])
    expect(x.draft.text.startsWith('Answer')).toBe(true)
  })
})

describe('attachments', () => {
  const file = (name: string, size: number, type = 'text/plain') => ({ file: new Blob(['x'.repeat(size)], { type }), name })

  it('uploads, then saves the draft with the blob, and follows the blob into the new Email', async () => {
    const { server, compose } = await setup()
    const c = compose.compose()
    const { added, refused } = compose.attach(c, [file('notes.txt', 20)])
    expect(refused).toEqual([])
    expect(added[0]!.state).toBe('uploading')
    expect(compose.hasUnsaved()).toBe(true)
    expect(compose.problems(c)).toContain('Wait until the attachments are uploaded.')
    await tick(0)
    expect(added[0]).toMatchObject({ state: 'done', blobId: 'upload1', progress: 1, size: 20 })
    await tick(1000)
    const att = calls(server, 'Email/set')[0]!.create.draft.attachments
    expect(att).toEqual([{ blobId: 'upload1', type: 'text/plain', name: 'notes.txt', cid: added[0]!.cid, disposition: 'attachment' }])
    // The attachment now points at the part inside the saved draft.
    expect(c.draft.attachments[0]!.blobId).toBe(`blob-${c.savedId}-2`)
    // The next save refers to that part, which exists until the same call destroys the old draft.
    c.draft.subject = 'again'
    compose.touch(c)
    await tick(1000)
    expect(c.saveError).toBe('')
    expect(c.draft.attachments[0]!.blobId).toBe(`blob-${c.savedId}-2`)
    expect(drafts(server)).toHaveLength(1)
  })

  it('refuses a file over maxSizeUpload without uploading it', async () => {
    const { server, compose } = await setup()
    server.maxSizeUpload = 1000
    await compose.client.loadSession()
    const c = compose.compose()
    const r = compose.attach(c, [file('big.bin', 2_000_000), file('small.txt', 10)])
    expect(r.refused).toEqual(['big.bin is larger than the 0 MB the server accepts.'])
    await tick(0)
    expect(c.draft.attachments.map((x) => x.name)).toEqual(['small.txt'])
    expect(server.uploads).toHaveLength(1)
  })

  it('cancels an upload that is removed', async () => {
    const { server, compose } = await setup()
    const c = compose.compose()
    server.hold()
    const { added } = compose.attach(c, [file('slow.bin', 10)])
    compose.removeAttachment(c, added[0]!)
    server.release()
    await tick(0)
    expect(c.draft.attachments).toEqual([])
    expect(server.uploads).toHaveLength(0)
  })

  it('shows a failed upload and sends it again on retry', async () => {
    const { server, compose } = await setup()
    const c = compose.compose()
    server.uploadStatus = 500
    const { added } = compose.attach(c, [file('a.txt', 10)])
    await tick(0)
    expect(added[0]).toMatchObject({ state: 'failed', blobId: null })
    expect(compose.problems(c)).toContain('Remove the attachment that failed, or try it again.')
    server.uploadStatus = 0
    compose.retryUpload(c, added[0]!)
    await tick(0)
    expect(added[0]!.state).toBe('done')
  })

  it('a pasted image is inline in rich text only', async () => {
    const { compose } = await setup()
    compose.rich = true
    const rich = compose.compose()
    expect(compose.attach(rich, [file('p.png', 5, 'image/png')], true).added[0]!.inline).toBe(true)
    expect(compose.attach(rich, [file('doc.pdf', 5, 'application/pdf')], true).added[0]!.inline).toBe(false)
    compose.rich = false
    const plain = compose.compose()
    expect(compose.attach(plain, [file('p.png', 5, 'image/png')], true).added[0]!.inline).toBe(false)
  })
})

describe('send', () => {
  async function replyTo(over: Parameters<FakeServer['deliver']>[0] = {}) {
    const ctx = await setup()
    const orig = ctx.server.deliver({ subject: 'Question', from: 'ann@example.org', ...over })
    await ctx.mail.openThread(orig.threadId)
    ctx.server.requests = []
    return { ...ctx, orig }
  }

  it('is one request: create, submit with the move to Sent, and the keyword on the original behind a result reference', async () => {
    const { server, mail, compose, orig } = await replyTo()
    const c = compose.respond('reply', mail.state.emails[orig.id]!)
    c.draft.text = `Yes.${c.draft.text}`
    compose.touch(c)
    await tick(1000)
    const savedDraft = c.savedId!
    server.requests = []
    const sent = await compose.send(c)
    expect(sent).not.toBeNull()
    // One request does all of it (a refresh may follow).
    expect(server.requests.filter((r) => r.some((x) => x[0].endsWith('/set')))).toHaveLength(1)
    const [set, submit, mark] = server.requests[0]! as [string, any, string][]
    expect(set![0]).toBe('Email/set')
    expect(set![1].create).toMatchObject({ draft: {
      mailboxIds: { [DRAFTS]: true }, keywords: { $draft: true, $seen: true }, from: [a('dev@robinmail.test')], to: [a('ann@example.org')],
      subject: 'Re: Question', inReplyTo: [`${orig.id}@fake`], references: [`${orig.id}@fake`],
    } })
    expect(set![1].create.draft['header:X-Robinmail-Draft:asText']).toBeUndefined()
    expect(set![1].destroy).toEqual([savedDraft])
    expect(submit).toEqual(['EmailSubmission/set', {
      accountId: 'acct1',
      create: { send: { identityId: 'i1', emailId: '#draft' } },
      onSuccessUpdateEmail: { '#send': { [`mailboxIds/${DRAFTS}`]: null, [`mailboxIds/${SENT}`]: true, 'keywords/$draft': null } },
    }, 'c1'])
    expect(mark).toEqual(['Email/set', {
      accountId: 'acct1',
      update: { [orig.id]: { 'keywords/$answered': true } },
      '#destroy': { resultOf: 'c1', name: 'EmailSubmission/set', path: '/created/send/dsnBlobIds' },
    }, 'c2'])

    // What the server made of it.
    const mine = server.emails.get(sent!.emailId)!
    expect(mine.mailboxIds).toEqual({ [SENT]: true })
    expect(mine.keywords).toEqual({ $seen: true })
    expect(mine.threadId).toBe(orig.threadId)
    expect(server.emails.get(orig.id)!.keywords.$answered).toBe(true)
    expect(drafts(server)).toHaveLength(0)
    expect(compose.state.composers).toHaveLength(0)
    expect(compose.state.sent).toEqual([sent])
    expect(sent!.marked).toEqual({ emailId: orig.id, keyword: '$answered' })
    expect(compose.state.submissions[sent!.emailId]).toMatchObject({ undoStatus: 'pending', deliveryStatus: { 'ann@example.org': { delivered: 'queued' } } })
  })

  it('a forward sets $forwarded; a new message has no third call', async () => {
    const { server, mail, compose, orig } = await replyTo()
    const f = compose.respond('forward', mail.state.emails[orig.id]!)
    f.draft.to = [a('bob@example.org')]
    await compose.send(f)
    expect((server.requests[0]![2]![1] as any).update).toEqual({ [orig.id]: { 'keywords/$forwarded': true } })
    expect(server.emails.get(orig.id)!.keywords).toEqual({ $forwarded: true })
    server.requests = []
    const n = compose.compose()
    n.draft.to = [a('bob@example.org')]
    await compose.send(n)
    expect(server.requests[0]!.map((c) => c[0])).toEqual(['Email/set', 'EmailSubmission/set'])
  })

  it('a refused send leaves the message as a draft, marks nothing, and says why', async () => {
    const { server, mail, compose, orig } = await replyTo()
    const c = compose.respond('reply', mail.state.emails[orig.id]!)
    server.failSend = { type: 'forbiddenToSend', description: 'rejected as spam' }
    expect(await compose.send(c)).toBeNull()
    expect(c.sendError).toBe('The server did not accept this message for sending: rejected as spam')
    expect(compose.state.composers).toContain(c)
    expect(drafts(server).map((e) => e.id)).toEqual([c.savedId])
    expect(server.emails.get(c.savedId!)!.keywords.$draft).toBe(true)
    expect(server.emails.get(orig.id)!.keywords.$answered).toBeUndefined()
    expect(server.submissions.size).toBe(0)
    // Sending again replaces that draft: nothing is left behind.
    const sent = await compose.send(c)
    expect(sent).not.toBeNull()
    expect(drafts(server)).toHaveLength(0)
    expect(server.emails.get(orig.id)!.keywords.$answered).toBe(true)
  })

  it('does not try without a recipient or while an upload runs', async () => {
    const { server, compose } = await setup()
    const c = compose.compose()
    expect(await compose.send(c)).toBeNull()
    expect(c.sendError).toBe('Add at least one recipient.')
    expect(server.calls).toHaveLength(0)
  })

  it('says it does not know when the answer is lost', async () => {
    const { server, compose } = await setup()
    const c = compose.compose()
    c.draft.to = [a('bob@example.org')]
    server.httpStatus = 502
    expect(await compose.send(c)).toBeNull()
    expect(c.sendError).toMatch(/not known whether the message was sent/)
    expect(c.sending).toBe(false)
  })

  it('words the errors of a submission', () => {
    expect(sendErrorText({ type: 'forbiddenFrom' })).toMatch(/may not send from this address/)
    expect(sendErrorText({ type: 'tooManyRecipients', maxRecipients: 100 })).toBe('Too many recipients: the server takes at most 100 for one message.')
    expect(sendErrorText({ type: 'tooManyRecipients', maxRecipients: 300, description: '300 recipients per hour' })).toBe('Too many recipients: the server takes at most 300 an hour.')
    expect(sendErrorText({ type: 'invalidRecipients', invalidRecipients: ['a@b', 'c@d'] })).toBe('The server refused these addresses: a@b, c@d')
    expect(sendErrorText({ type: 'forbiddenToSend', description: 'the scanner is away' })).toBe('The server did not accept this message for sending: the scanner is away')
    expect(sendErrorText({ type: 'tooLarge', maxSize: 26_000_000 })).toBe('The message is too large. The limit is 26 MB.')
    expect(sendErrorText({ type: 'noRecipients' })).toBe('Add at least one recipient.')
  })

  it('undo cancels the submission, puts the message back in Drafts and opens it again', async () => {
    const { server, mail, compose, orig } = await replyTo()
    const c = compose.respond('reply', mail.state.emails[orig.id]!)
    c.draft.text = `Yes.${c.draft.text}`
    const sent = (await compose.send(c))!
    server.requests = []
    const back = await compose.undo(sent)
    expect(server.requests[0]).toEqual([['EmailSubmission/set', { accountId: 'acct1', update: { [sent.submissionId]: { undoStatus: 'canceled' } } }, 'c0']])
    expect((server.requests[1]![0]![1] as any).update).toEqual({
      [sent.emailId]: { [`mailboxIds/${SENT}`]: null, [`mailboxIds/${DRAFTS}`]: true, 'keywords/$draft': true },
      [orig.id]: { 'keywords/$answered': null },
    })
    expect(server.submissions.get(sent.submissionId)!.undoStatus).toBe('canceled')
    expect(server.emails.get(orig.id)!.keywords.$answered).toBeUndefined()
    expect(compose.state.composers).toEqual([back])
    expect(compose.state.sent).toEqual([])
    expect(back.draft.text.startsWith('Yes.')).toBe(true)
    expect(back.savedId).toBe(sent.emailId)
    // It is saved again as a draft that says what it answers.
    await tick(1000)
    expect(drafts(server)).toHaveLength(1)
    expect(server.created.get(back.savedId!)!['header:X-Robinmail-Draft:asText']).toBe(`reply ${orig.id}`)
  })

  it('undo leaves a keyword that was there before the send', async () => {
    const { server, mail, compose, orig } = await replyTo({ keywords: { $seen: true, $answered: true } })
    const sent = (await compose.send(compose.respond('reply', mail.state.emails[orig.id]!)))!
    expect(sent.marked).toBeNull()
    await compose.undo(sent)
    expect(server.emails.get(orig.id)!.keywords.$answered).toBe(true)
  })

  it('undo that comes too late says so and changes nothing', async () => {
    const { server, compose } = await setup()
    const c = compose.compose()
    c.draft.to = [a('bob@example.org')]
    const sent = (await compose.send(c))!
    server.submissions.get(sent.submissionId)!.undoStatus = 'final'
    await expect(compose.undo(sent)).rejects.toThrow('Too late: the message is on its way.')
    expect(server.emails.get(sent.emailId)!.mailboxIds).toEqual({ [SENT]: true })
    expect(compose.state.composers).toHaveLength(0)
    expect(compose.state.sent).toHaveLength(0)
  })
})

describe('delivery status', () => {
  it('follows a submission through EmailSubmission/changes', async () => {
    const { server, compose } = await setup()
    const c = compose.compose()
    c.draft.to = [a('bob@example.org'), a('nobody@robinmail.test')]
    const sent = (await compose.send(c))!
    expect(compose.unsettled()).toBe(true)
    // The queue worked on it.
    const sub = server.submissions.get(sent.submissionId)!
    server.modseq++
    sub.undoStatus = 'final'
    sub.deliveryStatus = {
      'bob@example.org': { smtpReply: '250 2.0.0 ok', delivered: 'unknown', displayed: 'unknown' },
      'nobody@robinmail.test': { smtpReply: '550 5.1.1 no such user', delivered: 'no', displayed: 'unknown' },
    }
    server.log.push({ modseq: server.modseq, type: 'EmailSubmission', id: sub.id, op: 'updated' })
    server.requests = []
    await compose.refreshSubmissions()
    expect(server.requests[0]!.map((x) => x[0])).toEqual(['EmailSubmission/changes', 'EmailSubmission/get', 'EmailSubmission/get'])
    expect(compose.state.submissions[sent.emailId]!.deliveryStatus!['nobody@robinmail.test']).toEqual({ smtpReply: '550 5.1.1 no such user', delivered: 'no', displayed: 'unknown' })
    expect(compose.unsettled()).toBe(false)
    // Nothing changed: nothing fetched.
    server.requests = []
    await compose.refreshSubmissions()
    expect(server.requests[0]![1]![1]).toMatchObject({ '#ids': { path: '/created' } })
  })

  it('loads the submissions of emails once', async () => {
    const { server, compose } = await setup()
    const c = compose.compose()
    c.draft.to = [a('bob@example.org')]
    const sent = (await compose.send(c))!
    compose.state.submissions = {}
    server.requests = []
    await compose.loadSubmissions([sent.emailId, 'e999'])
    await compose.loadSubmissions([sent.emailId, 'e999'])
    expect(server.requests).toHaveLength(1)
    expect(server.requests[0]![0]).toEqual(['EmailSubmission/query', { accountId: 'acct1', filter: { emailIds: [sent.emailId, 'e999'] } }, 'c0'])
    expect(compose.state.submissions[sent.emailId]!.id).toBe(sent.submissionId)
  })
})

describe('identities', () => {
  it('adds, changes and removes a +tag identity, and refuses another address', async () => {
    const { server, compose } = await setup()
    await compose.createIdentity('dev+shop@robinmail.test', { name: 'The Shop', replyTo: [a('desk@example.org')], textSignature: 'Shop\nOpen daily' })
    expect(compose.state.identities.map((i) => i.email)).toEqual(['dev@robinmail.test', 'dev+shop@robinmail.test'])
    const id = compose.state.identities[1]!.id
    await compose.updateIdentity(id, { name: 'Shop', replyTo: [], textSignature: '' })
    expect(server.identities.get(id)).toMatchObject({ name: 'Shop', replyTo: null, textSignature: '' })
    await expect(compose.createIdentity('boss@example.org', { name: '', replyTo: [], textSignature: '' })).rejects.toThrow(/your own address/)
    await expect(compose.destroyIdentity('i1')).rejects.toThrow(/cannot be removed/)
    const c = compose.compose()
    compose.setIdentity(c, id)
    await compose.destroyIdentity(id)
    expect(compose.state.identities).toHaveLength(1)
    expect(c.draft.identityId).toBe('i1')
  })

  it('puts the signature in a new message and swaps it with the identity', async () => {
    const { compose } = await setup()
    await compose.updateIdentity('i1', { name: 'Dev', replyTo: [], textSignature: 'Dev' })
    await compose.createIdentity('dev+shop@robinmail.test', { name: 'Shop', replyTo: [], textSignature: 'The Shop' })
    const c = compose.compose()
    expect(c.draft.text).toBe('\n\n-- \nDev')
    c.draft.text = `Hello${c.draft.text}`
    compose.setIdentity(c, compose.state.identities[1]!.id)
    expect(c.draft.text).toBe('Hello\n\n-- \nThe Shop')
  })
})

describe('a mailto: link', () => {
  it('fills a new message', async () => {
    const { compose } = await setup()
    const c = compose.compose({ to: [a('ann@example.org')], cc: [], bcc: [], subject: 'Hi', body: 'Text' })
    expect(c.draft).toMatchObject({ to: [a('ann@example.org')], subject: 'Hi', text: 'Text' })
  })
})
