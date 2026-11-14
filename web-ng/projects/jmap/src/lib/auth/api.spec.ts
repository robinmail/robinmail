import { describe, expect, it } from 'vitest'
import { AuthError, createAppPassword, currentSession, explain, login, loginCode, logout, qrPath, revokeAppPassword, secondProof } from './api'

type Call = [string, RequestInit | undefined]

function recorder(answer: () => Response) {
  const calls: Call[] = []
  const f: typeof fetch = async (u, i) => (calls.push([String(u), i]), answer())
  return { calls, f }
}

describe('login', () => {
  it('posts JSON and returns the account', async () => {
    const { calls, f } = recorder(() => Response.json({ accountId: 'a', username: 'u@x' }))
    expect(await login('u@x', 'pw', true, f)).toEqual({ accountId: 'a', username: 'u@x' })
    expect(calls[0]![0]).toBe('/auth/login')
    expect(calls[0]![1]).toMatchObject({ method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' } })
    expect(JSON.parse(String(calls[0]![1]!.body))).toEqual({ username: 'u@x', password: 'pw', remember: true })
  })

  it('says when the account wants a code, and sends it as the second step', async () => {
    expect(await login('u@x', 'pw', false, async () => Response.json({ step: 'totp' }))).toEqual({ step: 'totp' })
    const { calls, f } = recorder(() => Response.json({ accountId: 'a', username: 'u@x' }))
    await loginCode({ code: '123456' }, f)
    await loginCode({ recoveryCode: 'abcd-efgh-ijkl-mnop' }, f)
    expect(calls.map((c) => [c[0], JSON.parse(String(c[1]!.body))])).toEqual([
      ['/auth/login/totp', { code: '123456' }],
      ['/auth/login/totp', { recoveryCode: 'abcd-efgh-ijkl-mnop' }],
    ])
  })

  it('errors carry the status, the text and how long to wait', async () => {
    const limited: typeof fetch = async () =>
      Response.json({ error: 'too many failed logins; try again later' }, { status: 429, headers: { 'Retry-After': '120' } })
    await expect(login('u@x', 'pw', false, limited)).rejects.toMatchObject({ status: 429, retryAfter: 120, message: 'too many failed logins; try again later' })
    await expect(login('u@x', 'pw', false, async () => new Response('bad gateway', { status: 502 }))).rejects.toMatchObject({ status: 502, message: 'HTTP 502' })
  })
})

describe('session', () => {
  it('is null without a login, and says when a login waits for its code', async () => {
    expect(await currentSession(async () => Response.json({ error: 'not logged in' }, { status: 401 }))).toEqual({ session: null, waiting: false })
    expect(await currentSession(async () => Response.json({ error: 'the login waits for its code', step: 'totp' }, { status: 401 }))).toEqual({ session: null, waiting: true })
    const s = { accountId: 'a', username: 'u@x', sessionId: 's', method: 'password', totp: false, hasPassword: true, appPasswords: 0, passwordOpensMail: true }
    expect(await currentSession(async () => Response.json(s))).toEqual({ session: s, waiting: false })
    await expect(currentSession(async () => Response.json({ error: 'down' }, { status: 503 }))).rejects.toBeInstanceOf(AuthError)
  })
})

describe('requests that change something', () => {
  // The server refuses anything under /auth that is not a GET and not
  // JSON, body or not: that is its guard against forms on other sites.
  it('are JSON with their content type, also without anything to say', async () => {
    const { calls, f } = recorder(() => new Response(null, { status: 204 }))
    await logout(f)
    await revokeAppPassword('id/with a slash', f)
    for (const [, init] of calls) {
      expect(init).toMatchObject({ credentials: 'same-origin', headers: { 'Content-Type': 'application/json' } })
      expect(init!.body).toBe('{}')
    }
    expect(calls.map((c) => [c[1]!.method, c[0]])).toEqual([['POST', '/auth/logout'], ['DELETE', '/auth/app-passwords/id%2Fwith%20a%20slash']])
  })

  it('carry the reason of a refusal', async () => {
    const wrong: typeof fetch = async () => Response.json({ error: 'wrong password', reason: 'password' }, { status: 403 })
    const e = await createAppPassword('Phone', [], 'x', wrong).catch((e: unknown) => e)
    expect(e).toMatchObject({ status: 403, reason: 'password' })
    expect(explain(e)).toBe('That is not your password.')
  })
})

describe('secondProof', () => {
  it('takes six digits for a code and anything else for a recovery code', () => {
    expect(secondProof(' 123 456 ')).toEqual({ code: '123456' })
    expect(secondProof('abcd-efgh-ijkl-mnop')).toEqual({ recoveryCode: 'abcd-efgh-ijkl-mnop' })
    expect(secondProof('12345')).toEqual({ recoveryCode: '12345' })
  })
})

describe('explain', () => {
  it('puts errors into words', () => {
    expect(explain(new AuthError(429, 'too many', 61))).toBe('Too many wrong tries. Try again in 2 minutes.')
    expect(explain(new AuthError(429, 'too many', 5))).toBe('Too many wrong tries. Try again in 1 minute.')
    expect(explain(new AuthError(403, 'log in again', 0, 'stale'))).toContain('Log out and in again')
    expect(explain(new AuthError(401, 'wrong code'))).toContain('That code is not right')
    expect(explain(new AuthError(409, 'TOTP is already on; turn it off first to set it up again'))).toBe('TOTP is already on; turn it off first to set it up again.')
    expect(explain(new AuthError(503, 'x'))).toContain('503')
    expect(explain(new TypeError('Failed to fetch'))).toContain('does not answer')
  })
})

describe('qrPath', () => {
  it('draws one rectangle for each run of dark modules', () => {
    expect(qrPath(['101', '011', '000'])).toBe('M0 0h1v1h-1zM2 0h1v1h-1zM1 1h2v1h-2z')
    expect(qrPath([])).toBe('')
  })
})
