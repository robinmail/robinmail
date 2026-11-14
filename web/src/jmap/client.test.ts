import { describe, expect, it, vi } from 'vitest'
import { HttpError, JmapClient, MethodError, USING, localPath, login, currentAccount } from './client'
import { FakeServer } from '../test/fake'

function setup() {
  const server = new FakeServer()
  const onUnauthorized = vi.fn()
  const client = new JmapClient({ fetch: server.fetch, onUnauthorized })
  return { server, client, onUnauthorized }
}

describe('session', () => {
  it('loads the session and picks the mail account', async () => {
    const { client } = setup()
    const s = await client.loadSession()
    expect(s.username).toBe('dev@robinmail.test')
    expect(client.accountId).toBe('acct1')
  })

  it('uses the paths of the session URLs on its own origin', async () => {
    const { client, server } = setup()
    await client.loadSession()
    expect(localPath('http://127.0.0.1:8080/jmap/api')).toBe('/jmap/api')
    expect(localPath('/jmap/api')).toBe('/jmap/api')
    expect(client.downloadUrl('abc-1', 'a b/c?.pdf', 'application/pdf')).toBe(
      '/jmap/download/acct1/abc-1/a%20b%2Fc%3F.pdf?type=application%2Fpdf',
    )
    expect(client.viewUrl('e1', true)).toBe('/jmap/view/acct1/e1?images=1')
    // The API request went to the path, not to http://mail.example.
    const seen: string[] = []
    const c2 = new JmapClient({ fetch: (u, i) => (seen.push(String(u)), server.fetch(u, i)) })
    await c2.batch().send()
    expect(seen).toEqual(['/.well-known/jmap', '/jmap/api'])
  })
})

describe('batch', () => {
  it('sends calls in order with accountId, call ids and result references', async () => {
    const { client, server } = setup()
    server.deliver()
    const sent: unknown[] = []
    const c = new JmapClient({ fetch: (u, i) => (i?.body && sent.push(JSON.parse(String(i.body))), server.fetch(u, i)) })
    const b = c.batch()
    const q = b.call('Email/query', { filter: { inMailbox: 'm1' } })
    const g = b.call('Email/get', { '#ids': q.ref('/ids'), properties: ['subject'] })
    const r = await b.send()
    expect(sent[0]).toEqual({
      using: USING,
      methodCalls: [
        ['Email/query', { accountId: 'acct1', filter: { inMailbox: 'm1' } }, 'c0'],
        ['Email/get', { accountId: 'acct1', '#ids': { resultOf: 'c0', name: 'Email/query', path: '/ids' }, properties: ['subject'] }, 'c1'],
      ],
    })
    expect(r.get(q).ids).toEqual(['e1'])
    expect(r.get(g).list[0]).toEqual({ id: 'e1', subject: 'Subject e1' })
    void client
  })

  it('throws the method error of a call, and only of that call', async () => {
    const { client } = setup()
    const b = client.batch()
    const ok = b.call('Mailbox/get', { ids: null })
    const bad = b.call('Email/get', { ids: null })
    const r = await b.send()
    expect(r.get(ok).list).toHaveLength(5)
    expect(() => r.get(bad)).toThrowError(MethodError)
    expect(r.error(bad)?.type).toBe('requestTooLarge')
    expect(r.error(ok)).toBeNull()
  })

  it('a failed result reference fails the call that uses it', async () => {
    const { client } = setup()
    const b = client.batch()
    const ch = b.call('Email/changes', { sinceState: 'nonsense' })
    const g = b.call('Email/get', { '#ids': ch.ref('/created') })
    const r = await b.send()
    expect(r.error(ch)?.type).toBe('cannotCalculateChanges')
    expect(r.error(g)?.type).toBe('invalidResultReference')
  })
})

describe('http errors', () => {
  it('401 forgets the session and tells the app', async () => {
    const { client, server, onUnauthorized } = setup()
    await client.loadSession()
    client.setState('Email', '4')
    server.httpStatus = 401
    await expect(client.batch().send()).rejects.toMatchObject({ status: 401 })
    expect(onUnauthorized).toHaveBeenCalledOnce()
    expect(client.session).toBeNull()
    expect(client.states).toEqual({})
  })

  it('a request-level error is a MethodError with the short type', async () => {
    const client = new JmapClient({
      fetch: async (u) =>
        String(u) === '/.well-known/jmap'
          ? new FakeServer().fetch(u)
          : Response.json({ type: 'urn:ietf:params:jmap:error:limit', detail: 'too many calls', limit: 'maxCallsInRequest' }, { status: 400 }),
    })
    await expect(client.batch().send()).rejects.toMatchObject({ type: 'limit', description: 'too many calls' })
  })

  it('a 502 without JSON is an HttpError', async () => {
    const { client, server } = setup()
    await client.loadSession()
    const real = server.fetch
    const c = new JmapClient({ fetch: async (u, i) => (String(u) === '/jmap/api' ? new Response('bad gateway', { status: 502 }) : real(u, i)) })
    await expect(c.batch().send()).rejects.toBeInstanceOf(HttpError)
  })
})

describe('state per type', () => {
  it('a /set moves the state on only when it started from the state held', async () => {
    const { client, server } = setup()
    const e = server.deliver()
    client.setState('Email', String(server.modseq))
    const set = async () => {
      const b = client.batch()
      b.call('Email/set', { update: { [e.id]: { 'keywords/$seen': true } } })
      await b.send()
    }
    await set()
    expect(client.states.Email).toBe(String(server.modseq)) // contiguous: moved on

    server.deliver() // something we have not seen
    const held = client.states.Email
    await set()
    expect(client.states.Email).toBe(held) // a gap: stays, so /changes reports the delivery
    expect(client.states.Mailbox).toBeUndefined() // other types are not touched
  })

  it('does not take the state of a /get by itself', async () => {
    const { client } = setup()
    const b = client.batch()
    b.call('Mailbox/get', { ids: null })
    await b.send()
    expect(client.states).toEqual({})
  })
})

describe('keepalive', () => {
  it('a request that changes something outlives the page; a read does not ask for that', async () => {
    const server = new FakeServer()
    const e = server.deliver()
    const seen: (boolean | undefined)[] = []
    const client = new JmapClient({ fetch: (u, i) => (String(u) === '/jmap/api' && seen.push(i?.keepalive), server.fetch(u, i)) })
    const read = client.batch()
    read.call('Mailbox/get', { ids: null })
    await read.send()
    const write = client.batch()
    write.call('Email/set', { destroy: [e.id] })
    await write.send()
    expect(seen).toEqual([false, true])
  })
})

describe('login endpoints', () => {
  it('login posts JSON and returns the account; errors carry status and text', async () => {
    const calls: [string, RequestInit | undefined][] = []
    const ok: typeof fetch = async (u, i) => (calls.push([String(u), i]), Response.json({ accountId: 'a', username: 'u@x' }))
    expect(await login('u@x', 'pw', ok)).toEqual({ accountId: 'a', username: 'u@x' })
    expect(calls[0]![0]).toBe('/auth/login')
    expect(calls[0]![1]).toMatchObject({ method: 'POST', headers: { 'Content-Type': 'application/json' } })
    expect(JSON.parse(String(calls[0]![1]!.body))).toEqual({ username: 'u@x', password: 'pw' })

    const limited: typeof fetch = async () =>
      Response.json({ error: 'too many failed logins; try again later' }, { status: 429, headers: { 'Retry-After': '120' } })
    await expect(login('u@x', 'pw', limited)).rejects.toMatchObject({ status: 429, retryAfter: 120, message: 'too many failed logins; try again later' })
  })

  it('currentAccount is null without a session', async () => {
    expect(await currentAccount(async () => Response.json({ error: 'not logged in' }, { status: 401 }))).toBeNull()
    expect(await currentAccount(async () => Response.json({ accountId: 'a', username: 'u@x' }))).toEqual({ accountId: 'a', username: 'u@x' })
  })
})
