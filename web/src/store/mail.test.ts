import { beforeEach, describe, expect, it, vi } from 'vitest'
import { JmapClient } from '../jmap/client'
import { FakeServer } from '../test/fake'
import { MailStore, PAGE, buildFilter, type ListSpec } from './mail'

const spec = (over: Partial<ListSpec> = {}): ListSpec => ({ mailboxId: 'm1', from: '', subject: '', unread: false, flagged: false, attachment: false, ...over })

let server: FakeServer
let store: MailStore
let errors: string[]
let unauthorized: () => void

beforeEach(() => {
  server = new FakeServer()
  unauthorized = vi.fn<() => void>()
  store = new MailStore(new JmapClient({ fetch: server.fetch, onUnauthorized: unauthorized }))
  errors = []
  store.onError = (m) => errors.push(m)
})

// Lets queued refreshes and reloads finish.
async function settle() {
  for (let i = 0; i < 20; i++) await new Promise((r) => setTimeout(r, 0))
}

async function start(s = spec()) {
  await store.load()
  await store.openList(s)
}

const subjects = () => store.state.list.ids.map((id) => store.state.emails[id]?.subject)
const box = (id: string) => store.state.mailboxes[id]!

describe('buildFilter', () => {
  it('is the mailbox alone, or an AND with the other conditions', () => {
    expect(buildFilter(spec())).toEqual({ inMailbox: 'm1' })
    expect(buildFilter(spec({ from: 'bert', subject: 'lunch', unread: true, flagged: true, attachment: true }))).toEqual({
      operator: 'AND',
      conditions: [{ inMailbox: 'm1' }, { from: 'bert' }, { subject: 'lunch' }, { notKeyword: '$seen' }, { hasKeyword: '$flagged' }, { hasAttachment: true }],
    })
  })
})

describe('load and list', () => {
  it('loads mailboxes and learns a state for every type', async () => {
    server.deliver()
    await store.load()
    expect(Object.keys(store.state.mailboxes)).toEqual(['m1', 'm2', 'm3', 'm4', 'm5'])
    expect(box('m1')).toMatchObject({ role: 'inbox', totalEmails: 1, unreadEmails: 1 })
    const now = String(server.modseq)
    expect(store.client.states).toEqual({ Mailbox: now, Email: now, Thread: now })
    expect(store.state.username).toBe('dev@robinmail.test')
  })

  it('lists one row per thread, newest first, with every email of the thread loaded', async () => {
    const a = server.deliver({ subject: 'Lunch' })
    server.deliver({ subject: 'Report' })
    const reply = server.deliver({ subject: 'Re: Lunch', thread: a.threadId })
    await start()
    expect(subjects()).toEqual(['Re: Lunch', 'Report'])
    expect(store.state.list.total).toBe(2)
    expect(store.threadEmails(a.threadId).map((e) => e.id)).toEqual([a.id, reply.id])
    // One request, chained by result references.
    const last = server.requests.at(-1)!
    expect(last.map((c) => c[0])).toEqual(['Email/query', 'Email/get', 'Thread/get', 'Email/get'])
    expect(last[0]![1]).toMatchObject({ collapseThreads: true, position: 0, limit: PAGE, calculateTotal: true })
    expect(last[3]![1]['#ids']).toEqual({ resultOf: 'c2', name: 'Thread/get', path: '/list/*/emailIds' })
  })

  it('pages with position and limit', async () => {
    for (let i = 0; i < PAGE + 5; i++) server.deliver()
    await start()
    expect(store.state.list.ids).toHaveLength(PAGE)
    expect(store.state.list.total).toBe(PAGE + 5)
    await store.loadMore()
    expect(store.state.list.ids).toHaveLength(PAGE + 5)
    expect(new Set(store.state.list.ids).size).toBe(PAGE + 5)
    expect(server.calls.filter((c) => c[0] === 'Email/query').at(-1)![1]).toMatchObject({ position: PAGE, limit: PAGE })
    const n = server.requests.length
    await store.loadMore() // everything is loaded
    expect(server.requests.length).toBe(n)
  })

  it('a page fetched from a newer result reloads the window', async () => {
    for (let i = 0; i < PAGE + 5; i++) server.deliver()
    await start()
    server.touch(store.state.list.ids[0]!, (e) => (e.mailboxIds = { m5: true })) // the first row leaves: all shift up
    await store.loadMore()
    await settle()
    expect(store.state.list.ids).toHaveLength(PAGE + 4)
    const all = [...server.emails.values()].filter((e) => e.mailboxIds.m1).length
    expect(store.state.list.ids).toHaveLength(all)
  })

  it('shows an error of the query and can try again', async () => {
    await store.load()
    server.intercept = (name) => (name === 'Email/query' ? ['error', { type: 'unsupportedFilter' }] : null)
    await store.openList(spec())
    expect(store.state.list.error).toContain('unsupportedFilter')
    expect(store.state.list.loading).toBe(false)
    server.intercept = () => null
    await store.openList(spec())
    expect(store.state.list.error).toBe('')
  })

  it('an answer for a list that was left is dropped', async () => {
    server.deliver({ subject: 'in inbox' })
    server.deliver({ subject: 'in junk', mailbox: 'm4' })
    await store.load()
    const first = store.openList(spec())
    const second = store.openList(spec({ mailboxId: 'm4' }))
    await Promise.all([first, second])
    expect(subjects()).toEqual(['in junk'])
  })
})

describe('refresh', () => {
  it('brings new mail into the list and the counts', async () => {
    server.deliver({ subject: 'old' })
    await start()
    server.deliver({ subject: 'new' })
    await store.refresh()
    expect(subjects()).toEqual(['new', 'old'])
    expect(store.state.list.total).toBe(2)
    expect(box('m1')).toMatchObject({ totalEmails: 2, unreadEmails: 2 })
    const now = String(server.modseq)
    expect(store.client.states).toEqual({ Mailbox: now, Email: now, Thread: now })
    expect(store.state.list.queryState).toBe(now)
  })

  it('is one request, and a quiet one changes nothing', async () => {
    server.deliver()
    await start()
    const n = server.requests.length
    const before = JSON.stringify(store.state)
    await store.refresh()
    expect(server.requests.length).toBe(n + 1)
    expect(server.requests.at(-1)!.map((c) => c[0])).toEqual([
      'Mailbox/changes', 'Mailbox/get', 'Mailbox/get', 'Email/changes', 'Email/get', 'Email/get', 'Thread/changes', 'Thread/get',
      'Email/queryChanges', 'Email/get', 'Thread/get', 'Email/get',
    ])
    expect(JSON.stringify(store.state)).toBe(before)
  })

  it('a reply moves its thread to the top as one row', async () => {
    const a = server.deliver({ subject: 'Lunch' })
    server.deliver({ subject: 'Report' })
    await start()
    expect(subjects()).toEqual(['Report', 'Lunch'])
    const reply = server.deliver({ subject: 'Re: Lunch', thread: a.threadId })
    await store.refresh()
    expect(subjects()).toEqual(['Re: Lunch', 'Report'])
    expect(store.state.threads[a.threadId]!.emailIds).toEqual([a.id, reply.id])
  })

  it('takes over changes made elsewhere: keywords, moves, destroyed mail, mailboxes', async () => {
    const a = server.deliver({ subject: 'a' })
    const b = server.deliver({ subject: 'b' })
    const c = server.deliver({ subject: 'c' })
    await start()
    server.touch(a.id, (e) => (e.keywords = { $seen: true, $flagged: true }))
    server.touch(b.id, (e) => (e.mailboxIds = { m5: true }))
    const made = server.addMailbox('Projects')
    await store.refresh()
    expect(store.state.emails[a.id]!.keywords).toEqual({ $seen: true, $flagged: true })
    expect(subjects()).toEqual(['c', 'a'])
    expect(box('m1')).toMatchObject({ totalEmails: 2, unreadEmails: 1 })
    expect(box('m5').totalEmails).toBe(1)
    expect(box(made).name).toBe('Projects')

    // Destroyed elsewhere, through a Mailbox/set that removes the mail too.
    server.touch(c.id, (e) => (e.mailboxIds = { [made]: true }))
    const other = new JmapClient({ fetch: server.fetch })
    const ob = other.batch()
    ob.call('Mailbox/set', { destroy: [made], onDestroyRemoveEmails: true })
    await ob.send()
    await store.refresh()
    expect(store.state.emails[c.id]).toBeUndefined()
    expect(store.state.threads[c.threadId]).toBeUndefined()
    expect(store.state.mailboxes[made]).toBeUndefined()
    expect(subjects()).toEqual(['a'])
  })

  it('keeps a loaded body when the email changes', async () => {
    const a = server.deliver()
    await start()
    await store.openThread(a.threadId)
    expect(store.state.emails[a.id]!.bodyValues!['1']!.value).toBe('body of e1')
    server.touch(a.id, (e) => (e.keywords.$flagged = true))
    await store.refresh()
    expect(store.state.emails[a.id]!.keywords.$flagged).toBe(true)
    expect(store.state.emails[a.id]!.bodyValues!['1']!.value).toBe('body of e1')
  })

  it('asks again while the server has more changes', async () => {
    await start()
    server.maxChanges = 2
    for (let i = 0; i < 5; i++) server.deliver()
    await store.refresh()
    expect(store.client.states.Email).toBe(String(server.modseq))
    expect(Object.keys(store.state.emails)).toHaveLength(5)
    expect(server.calls.filter((c) => c[0] === 'Email/changes').length).toBeGreaterThan(1)
  })

  it('starts over when the state is too old (cannotCalculateChanges)', async () => {
    server.deliver({ subject: 'a' })
    await start()
    server.deliver({ subject: 'b' })
    server.trimmed = server.modseq // our state is older than the log now
    await store.refresh()
    await settle()
    expect(subjects()).toEqual(['b', 'a'])
    expect(box('m1').totalEmails).toBe(2)
    expect(store.client.states.Email).toBe(String(server.modseq))
    server.deliver({ subject: 'c' })
    await store.refresh()
    expect(subjects()).toEqual(['c', 'b', 'a'])
  })

  it('starting over keeps an open thread and its bodies, and drops what is gone', async () => {
    const a = server.deliver({ subject: 'a' })
    const b = server.deliver({ subject: 'b' })
    await start()
    await store.openThread(a.threadId)
    server.touch(a.id, (e) => (e.keywords.$flagged = true))
    server.emails.delete(b.id) // gone without a trace in the log
    server.trimmed = server.modseq
    await store.refresh()
    await settle()
    expect(store.state.emails[a.id]!.keywords.$flagged).toBe(true)
    expect(store.state.emails[a.id]!.bodyValues).toBeDefined()
    expect(store.state.emails[b.id]).toBeUndefined()
    expect(store.state.threads[b.threadId]).toBeUndefined()
    expect(subjects()).toEqual(['a'])
  })

  it('runs the query again when only the query state is too old', async () => {
    server.deliver({ subject: 'a' })
    await start()
    server.deliver({ subject: 'b' })
    server.intercept = (name) => (name === 'Email/queryChanges' ? ['error', { type: 'cannotCalculateChanges' }] : null)
    await store.refresh()
    await settle()
    expect(subjects()).toEqual(['b', 'a'])
  })

  it('marks the store offline when the server does not answer, and back', async () => {
    await start()
    server.httpStatus = 502
    await store.refresh()
    expect(store.state.offline).toBe(true)
    server.httpStatus = 0
    await store.refresh()
    expect(store.state.offline).toBe(false)
  })

  it('a 401 goes to the login, not to "offline"', async () => {
    await start()
    server.httpStatus = 401
    await store.refresh()
    expect(unauthorized).toHaveBeenCalled()
    expect(store.state.offline).toBe(false)
  })
})

describe('actions', () => {
  it('changes here at once, before the server answered', async () => {
    const a = server.deliver()
    await start()
    server.hold()
    const done = store.setKeyword([a.id], '$seen', true)
    expect(store.state.emails[a.id]!.keywords.$seen).toBe(true)
    expect(box('m1').unreadEmails).toBe(0)
    expect(server.emails.get(a.id)!.keywords.$seen).toBeUndefined()
    server.release()
    expect(await done).toBe(true)
    expect(server.emails.get(a.id)!.keywords.$seen).toBe(true)
    expect(server.calls.find((c) => c[0] === 'Email/set')![1].update).toEqual({ [a.id]: { 'keywords/$seen': true } })
  })

  it('sends nothing when nothing would change', async () => {
    const a = server.deliver({ seen: true })
    await start()
    const n = server.requests.length
    expect(await store.setKeyword([a.id], '$seen', true)).toBe(true)
    expect(server.requests.length).toBe(n)
  })

  it('puts back what the server refuses, and says so', async () => {
    const a = server.deliver()
    const b = server.deliver()
    await start()
    server.intercept = (name) =>
      name === 'Email/set'
        ? ['Email/set', { oldState: '1', newState: '1', updated: { [b.id]: null }, notUpdated: { [a.id]: { type: 'forbidden', description: 'read-only' } }, notDestroyed: null }]
        : null
    const ok = await store.setKeyword([a.id, b.id], '$flagged', true)
    expect(ok).toBe(false)
    expect(store.state.emails[a.id]!.keywords.$flagged).toBeUndefined()
    expect(store.state.emails[b.id]!.keywords.$flagged).toBe(true)
    expect(errors).toEqual(['The server refused the change: read-only'])
  })

  it('puts everything back when the request fails: keywords, counts, list, thread', async () => {
    const a = server.deliver({ subject: 'a' })
    const b = server.deliver({ subject: 'b', thread: a.threadId })
    server.deliver({ subject: 'c' })
    await start()
    await store.openThread(a.threadId)
    const before = JSON.stringify({ m: store.state.mailboxes, ids: store.state.list.ids, t: store.state.threads, total: store.state.list.total })
    server.hold()
    const moved = store.move([a.id, b.id], 'm1', 'm5')
    const gone = store.remove([store.state.list.ids[0]!])
    expect(subjects()).toEqual([])
    expect(box('m5').totalEmails).toBe(3)
    server.httpStatus = 502
    server.release()
    expect(await moved).toBe(false)
    expect(await gone).toBe(false)
    server.httpStatus = 0
    await settle()
    expect(JSON.stringify({ m: store.state.mailboxes, ids: store.state.list.ids, t: store.state.threads, total: store.state.list.total })).toBe(before)
    expect(store.state.emails[a.id]!.mailboxIds).toEqual({ m1: true })
    expect(errors).toHaveLength(2)
  })

  it('a method error (stateMismatch, forbidden) is put back too', async () => {
    const a = server.deliver()
    await start()
    server.intercept = (name) => (name === 'Email/set' ? ['error', { type: 'forbidden', description: 'no' }] : null)
    expect(await store.setKeyword([a.id], '$seen', true)).toBe(false)
    expect(store.state.emails[a.id]!.keywords).toEqual({})
    expect(box('m1').unreadEmails).toBe(1)
    expect(errors[0]).toContain('forbidden')
  })

  it('move: the row leaves the list and the counts follow', async () => {
    const a = server.deliver({ subject: 'a' })
    server.deliver({ subject: 'b' })
    await start()
    server.hold()
    const done = store.move([a.id], 'm1', 'm5')
    expect(subjects()).toEqual(['b'])
    expect(store.state.list.total).toBe(1)
    expect(box('m1')).toMatchObject({ totalEmails: 1, unreadEmails: 1 })
    expect(box('m5')).toMatchObject({ totalEmails: 1, unreadEmails: 1 })
    server.release()
    await done
    await settle()
    expect(server.emails.get(a.id)!.mailboxIds).toEqual({ m5: true })
    expect(subjects()).toEqual(['b'])
    expect(box('m5')).toMatchObject({ totalEmails: 1, unreadThreads: 1 })
  })

  it('move of the newest email of a thread keeps the row, on an older email', async () => {
    const a = server.deliver({ subject: 'first' })
    const b = server.deliver({ subject: 'second', thread: a.threadId })
    await start()
    expect(store.state.list.ids).toEqual([b.id])
    await store.move([b.id], 'm1', 'm5')
    expect(store.state.list.ids).toEqual([a.id])
    await settle()
    expect(store.state.list.ids).toEqual([a.id])
  })

  it('label: an email can be in several mailboxes, but not in none', async () => {
    const a = server.deliver()
    const projects = server.addMailbox('Projects')
    await start()
    await store.label([a.id], projects, true)
    expect(server.emails.get(a.id)!.mailboxIds).toEqual({ m1: true, [projects]: true })
    expect(subjects()).toHaveLength(1) // still in the inbox
    await store.label([a.id], 'm1', false)
    expect(server.emails.get(a.id)!.mailboxIds).toEqual({ [projects]: true })
    expect(subjects()).toHaveLength(0)
    const sets = () => server.calls.filter((c) => c[0] === 'Email/set').length
    const n = sets()
    await store.label([a.id], projects, false)
    expect(sets()).toBe(n)
    expect(errors).toEqual(['A message has to stay in at least one mailbox.'])
  })

  it('remove: to Trash out of every mailbox, then destroyed', async () => {
    const a = server.deliver()
    const projects = server.addMailbox('Projects')
    server.touch(a.id, (e) => (e.mailboxIds[projects] = true))
    await start()
    await store.remove([a.id])
    expect(server.emails.get(a.id)!.mailboxIds).toEqual({ m5: true })
    expect(subjects()).toEqual([])
    await settle()
    expect(box('m5').totalEmails).toBe(1)
    expect(box(projects).totalEmails).toBe(0)

    await store.openList(spec({ mailboxId: 'm5' }))
    expect(subjects()).toHaveLength(1)
    await store.remove([a.id])
    expect(server.emails.has(a.id)).toBe(false)
    expect(store.state.emails[a.id]).toBeUndefined()
    expect(subjects()).toEqual([])
    await settle()
    expect(box('m5').totalEmails).toBe(0)
  })

  it('a destroy that someone else did first is fine', async () => {
    const a = server.deliver({ mailbox: 'm5' })
    await start(spec({ mailboxId: 'm5' }))
    server.emails.delete(a.id)
    expect(await store.remove([a.id])).toBe(true)
    expect(errors).toEqual([])
  })

  it('a refresh that was under way during a change does not undo it', async () => {
    const a = server.deliver()
    await start()
    server.hold()
    const refresh = store.refresh() // carries the unread email
    await Promise.resolve()
    const done = store.setKeyword([a.id], '$seen', true)
    server.release()
    await refresh
    expect(store.state.emails[a.id]!.keywords.$seen).toBe(true)
    await done
    await settle()
    expect(store.state.emails[a.id]!.keywords.$seen).toBe(true)
    expect(box('m1').unreadEmails).toBe(0)
    expect(store.client.states.Mailbox).toBe(String(server.modseq))
  })

  it('after an action the counts are the server\'s (thread counts too)', async () => {
    const a = server.deliver()
    server.deliver({ thread: a.threadId })
    await start()
    await store.setKeyword(store.threadEmails(a.threadId).map((e) => e.id), '$seen', true)
    await settle()
    expect(box('m1')).toMatchObject({ unreadEmails: 0, unreadThreads: 0, totalThreads: 1 })
  })
})

describe('mailboxes', () => {
  it('creates, renames and destroys with ifInState', async () => {
    await start()
    const id = await store.createMailbox('Projects', null)
    expect(id).toBeTruthy()
    expect(box(id!).name).toBe('Projects')
    const sub = await store.createMailbox('2026', id)
    expect(box(sub!).parentId).toBe(id)
    await store.renameMailbox(id!, 'Work')
    expect(server.mailboxes.get(id!)!.name).toBe('Work')
    expect(box(id!).name).toBe('Work')
    await store.destroyMailbox(sub!, false)
    expect(store.state.mailboxes[sub!]).toBeUndefined()
    const sets = server.calls.filter((c) => c[0] === 'Mailbox/set')
    expect(sets).toHaveLength(4)
    for (const s of sets) expect(typeof s[1].ifInState).toBe('string')
    expect(store.client.states.Mailbox).toBe(String(server.modseq))
  })

  it('tries once more when only something else changed', async () => {
    await start()
    const id = await store.createMailbox('Projects', null)
    server.deliver() // moves the state; the mailbox is as it was
    await store.renameMailbox(id!, 'Work')
    expect(server.mailboxes.get(id!)!.name).toBe('Work')
    expect(server.calls.filter((c) => c[0] === 'Mailbox/set')).toHaveLength(3) // create, mismatch, retry
  })

  it('does not retry when the mailbox itself changed', async () => {
    await start()
    const id = await store.createMailbox('Projects', null)
    server.mailboxes.get(id!)!.name = 'Renamed elsewhere'
    server.deliver()
    await expect(store.renameMailbox(id!, 'Work')).rejects.toThrow(/changed somewhere else/)
    expect(server.mailboxes.get(id!)!.name).toBe('Renamed elsewhere')
    expect(box(id!).name).toBe('Renamed elsewhere') // and we show the new truth
  })

  it('does not destroy a mailbox that got mail after the user confirmed', async () => {
    await start()
    const id = await store.createMailbox('Projects', null)
    server.deliver({ mailbox: id! })
    await expect(store.destroyMailbox(id!, true)).rejects.toThrow(/changed somewhere else/)
    expect(server.mailboxes.has(id!)).toBe(true)
  })

  it('explains a refusal', async () => {
    await start()
    const id = await store.createMailbox('Projects', null)
    server.deliver({ mailbox: id! })
    await store.refresh()
    await expect(store.destroyMailbox(id!, false)).rejects.toThrow('This mailbox still has mail in it.')
    await expect(store.destroyMailbox('m1', false)).rejects.toThrow('This mailbox cannot be changed.')
  })

  it('destroying a mailbox with its mail removes the mail here too', async () => {
    await start()
    const id = await store.createMailbox('Projects', null)
    const a = server.deliver({ mailbox: id! })
    await store.refresh()
    await store.openList(spec({ mailboxId: id! }))
    await store.destroyMailbox(id!, true)
    await settle()
    expect(store.state.emails[a.id]).toBeUndefined()
    expect(store.state.mailboxes[id!]).toBeUndefined()
  })
})
